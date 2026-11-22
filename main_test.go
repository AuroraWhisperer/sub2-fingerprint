package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type isolatedSub2 struct {
	mu         sync.Mutex
	calls      map[int64]int
	in         map[int64]int
	peakActive int
	started    chan int64
	release    chan struct{}
	logout     bool
	listed     []AccountView
}

func newIsolatedSub2() *isolatedSub2 {
	return &isolatedSub2{calls: map[int64]int{}, in: map[int64]int{}, started: make(chan int64, 12), release: make(chan struct{}), listed: []AccountView{{ID: 101, Name: "甲", Eligible: true}, {ID: 202, Name: "乙", Eligible: true}, {ID: 303, Name: "丙", Eligible: true}}}
}
func (m *isolatedSub2) Login(_ context.Context, email, pw string) (LoginResult, error) {
	if pw != "fixture-password" {
		return LoginResult{}, &GateError{"UNAUTHORIZED", "未授权"}
	}
	if email == "user@example.com" {
		return LoginResult{JWT: "mock-non-admin", UserID: 13}, nil
	}
	if email == "two@example.com" {
		return LoginResult{Temp: "mock-login-challenge"}, nil
	}
	return LoginResult{JWT: "mock-upstream-login-token", UserID: 11, Admin: true, Refresh: "mock-refresh"}, nil
}
func (m *isolatedSub2) Login2FA(_ context.Context, tmp, code string) (LoginResult, error) {
	if tmp != "mock-login-challenge" || code != "123456" {
		return LoginResult{}, &GateError{"UNAUTHORIZED", "验证码无效"}
	}
	return LoginResult{JWT: "mock-second-admin", UserID: 12, Admin: true}, nil
}
func (m *isolatedSub2) Logout(context.Context, string) error { m.logout = true; return nil }
func (m *isolatedSub2) CurrentAdmin(_ context.Context, token string) (int64, error) {
	if token == "" {
		return 0, sessionInvalid
	}
	return 11, nil
}
func (m *isolatedSub2) StepUp(context.Context, string, string) error        { return nil }
func (m *isolatedSub2) List(context.Context, string) ([]AccountView, error) { return m.listed, nil }
func (m *isolatedSub2) Probe(ctx context.Context, jwt string, id int64, prompt string) (ProbeAnswer, error) {
	m.mu.Lock()
	m.calls[id]++
	m.in[id]++
	active := 0
	for _, n := range m.in {
		active += n
	}
	if m.in[id] > 1 {
		panic("same account ran in parallel")
	}
	if active > m.peakActive {
		m.peakActive = active
	}
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.in[id]--; m.mu.Unlock() }()
	m.started <- id
	select {
	case <-ctx.Done():
		return ProbeAnswer{}, ctx.Err()
	case <-m.release:
	}
	return ProbeAnswer{Text: fmt.Sprintf("account=%d; question=%s", id, prompt), Model: "gpt-6-astra"}, nil
}

type testBrowser struct {
	t      *testing.T
	a      *app
	cookie *http.Cookie
	token  string
	csrf   string
	epoch  string
}

func (b *testBrowser) call(method, path string, body any, origin string) (int, map[string]any, string) {
	b.t.Helper()
	bytes, _ := json.Marshal(body)
	r := httptest.NewRequest(method, "http://"+b.a.cfg.Listen+path, strings.NewReader(string(bytes)))
	if b.cookie != nil {
		r.AddCookie(b.cookie)
	}
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	if method == "POST" {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", b.csrf)
	}
	w := httptest.NewRecorder()
	b.a.ServeHTTP(w, r)
	resp := w.Result()
	for _, c := range resp.Cookies() {
		b.cookie = c
	}
	text := w.Body.String()
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if epoch, ok := out["epoch"].(string); ok {
		b.epoch = epoch
	}
	if csrf, ok := out["csrf"].(string); ok {
		b.csrf = csrf
	}
	return resp.StatusCode, out, text
}
func (b *testBrowser) post(p string, v any) (int, map[string]any, string) {
	return b.call("POST", p, v, "http://"+b.a.cfg.Listen)
}
func testApp(t *testing.T) (*app, *isolatedSub2) {
	t.Helper()
	return testAppWithParallel(t, 1)
}
func testAppWithParallel(t *testing.T, parallel int) (*app, *isolatedSub2) {
	t.Helper()
	t.Setenv("STATE_DIRECTORY", filepath.Join(t.TempDir(), "auth"))
	m := newIsolatedSub2()
	a, err := newApp(settings{Listen: "127.0.0.1:17861", Data: t.TempDir(), Parallel: parallel}, m)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.queue.close)
	return a, m
}
func waitStart(t *testing.T, m *isolatedSub2) int64 {
	t.Helper()
	select {
	case id := <-m.started:
		return id
	case <-time.After(4 * time.Second):
		t.Fatal("worker did not start")
		return 0
	}
}
func waitRun(t *testing.T, q *queue, owner int64, id string) *Run {
	t.Helper()
	until := time.Now().Add(4 * time.Second)
	for time.Now().Before(until) {
		r, e := q.get(owner, id)
		if e != nil {
			t.Fatal(e)
		}
		if !nonterminal(r.State) {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("run did not finish")
	return nil
}
func TestAdminSessionAndBoundedBatch(t *testing.T) {
	a, m := testAppWithParallel(t, 1)
	m.listed = append(m.listed, AccountView{ID: 404, Name: "account four", Eligible: true})
	b := testBrowser{t: t, a: a}
	if c, _, _ := b.call("GET", "/api/accounts", nil, ""); c != 401 {
		t.Fatal(c)
	}
	b.call("GET", "/api/session", nil, "")
	creds := map[string]string{"email": "admin@example.com", "password": "fixture-password"}
	if c, _, _ := b.call("POST", "/api/login", creds, "http://different.invalid"); c != 403 {
		t.Fatal(c)
	}
	old := *b.cookie
	if c, _, text := b.post("/api/login", creds); c != 200 || strings.Contains(text, "mock-upstream-login-token") || old.Value == b.cookie.Value {
		t.Fatal("login did not rotate or filtered response failed", c)
	}
	stale := testBrowser{t: t, a: a, cookie: &old}
	if c, _, _ := stale.call("GET", "/api/accounts", nil, ""); c != 401 {
		t.Fatal("reused anonymous session")
	}
	qs := a.groups[0]
	req := RunRequest{Epoch: b.epoch, ID: "1eee2222-0000-4000-8000-000000000001", Parallel: 1, Accounts: []int64{101, 202, 303, 404}, Questions: []string{qs[0].ID, qs[1].ID}}
	if c, _, txt := b.post("/api/runs", req); c != 202 {
		t.Fatal(c, txt)
	}
	first := waitStart(t, m)
	if first != 101 && first != 202 && first != 303 && first != 404 {
		t.Fatalf("unexpected account %d in first probe", first)
	}
	pending, err := a.queue.get(11, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	codes := make(map[string]string)
	for _, row := range pending.Rows {
		if row.RequestID == "" || codes[row.RequestID] != "" || strings.ContainsAny(row.RequestID, "- ") {
			t.Fatal("each selected account and challenge must have a unique code before dispatch")
		}
		codes[row.RequestID] = fmt.Sprintf("%d/%s", row.AccountID, row.QuestionID)
		if row.AccountID != first && row.Status != "queued" {
			t.Fatalf("account %d started before serial slot was free", row.AccountID)
		}
	}
	if c, _, _ := b.post("/api/runs", req); c != 200 {
		t.Fatal("duplicate submit", c)
	}
	bad := req
	bad.Questions = []string{qs[2].ID}
	if c, _, _ := b.post("/api/runs", bad); c != 409 {
		t.Fatal("idempotency conflict", c)
	}
	close(m.release)
	r := waitRun(t, a.queue, 11, req.ID)
	if len(r.Rows) != 8 || r.State != "done" {
		t.Fatal("wrong results")
	}
	for _, row := range r.Rows {
		if codes[row.RequestID] != fmt.Sprintf("%d/%s", row.AccountID, row.QuestionID) {
			t.Fatal("a reply changed the code assigned to the original account and challenge")
		}
		if !strings.Contains(row.Text, fmt.Sprintf("account=%d", row.AccountID)) || row.QuestionID == "" || row.RequestID == "" || row.Status != "done" {
			t.Fatal("cross-account or missing result")
		}
	}
	m.mu.Lock()
	if m.peakActive != 1 || m.calls[101] != 2 || m.calls[202] != 2 || m.calls[303] != 2 || m.calls[404] != 2 {
		t.Errorf("unexpected upstream calls %v, peak %d", m.calls, m.peakActive)
	}
	m.mu.Unlock()
	if _, err := os.Stat(filepath.Join(a.cfg.Data, req.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("completed answers must only exist in memory")
	}
	b.post("/api/logout", map[string]any{})
	if !m.logout {
		t.Error("dedicated session not revoked")
	}
	b.post("/api/login", map[string]string{"email": "two@example.com", "password": "fixture-password"})
	if c, _, _ := b.call("GET", "/api/runs", nil, ""); c != 401 {
		t.Fatal("unverified 2fa session")
	}
	b.post("/api/login2fa", map[string]string{"code": "123456"})
	if c, _, _ := b.call("GET", "/api/runs/"+req.ID, nil, ""); c != 404 {
		t.Fatal("history owner mismatch")
	}
}

func TestSub2BrowserSessionBridgeDoesNotLogOutSub2(t *testing.T) {
	a, m := testApp(t)
	b := testBrowser{t: t, a: a, token: "browser-access-token"}
	if code, out, _ := b.call("GET", "/api/session", nil, ""); code != 200 || out["logged_in"] != true {
		t.Fatalf("browser session was not bridged: code=%d out=%v", code, out)
	}
	if code, _, _ := b.call("GET", "/api/accounts", nil, ""); code != 200 {
		t.Fatalf("bridged session could not list accounts: %d", code)
	}
	if code, _, _ := b.post("/api/logout", map[string]any{}); code != 200 {
		t.Fatalf("local logout failed: %d", code)
	}
	if m.logout {
		t.Fatal("ModelTrace logout revoked the Sub2API session")
	}
	if code, out, _ := b.call("GET", "/api/session", nil, ""); code != 200 || out["logged_in"] != true {
		t.Fatalf("browser session was not reattached after local logout: code=%d out=%v", code, out)
	}
}
func TestSelectedChallengeNumbersSurviveReopening(t *testing.T) {
	a, m := testApp(t)
	close(m.release)
	b := testBrowser{t: t, a: a}
	b.call("GET", "/api/session", nil, "")
	if status, _, _ := b.post("/api/login", map[string]string{"email": "admin@example.com", "password": "fixture-password"}); status != http.StatusOK {
		t.Fatal("login failed", status)
	}
	for n, selected := range [][]int{{3}, {2, 3}} {
		status, _, body := b.call("GET", "/api/questions", nil, "")
		var catalogue struct{ Questions []Question }
		if status != http.StatusOK || json.Unmarshal([]byte(body), &catalogue) != nil || len(catalogue.Questions) != 3 {
			t.Fatal("could not read the three challenges")
		}
		ids := make([]string, 0, len(selected))
		for _, number := range selected {
			q := catalogue.Questions[number-1]
			if q.Number != number {
				t.Fatalf("catalogue lists challenge %d as %d", number, q.Number)
			}
			ids = append(ids, q.ID)
		}
		req := RunRequest{ID: fmt.Sprintf("1eee2222-0000-4000-8000-%012d", n+1), Epoch: b.epoch, Accounts: []int64{101}, Questions: ids, Parallel: 1}
		if status, _, _ := b.post("/api/runs", req); status != http.StatusAccepted {
			t.Fatal("could not start selected challenges", status)
		}
		waitRun(t, a.queue, 11, req.ID)
		b.call("GET", "/api/questions", nil, "") // Reloading the page fetches a fresh catalogue first.
		status, _, body = b.call("GET", "/api/runs/"+req.ID, nil, "")
		var reopened Run
		if status != http.StatusOK || json.Unmarshal([]byte(body), &reopened) != nil || len(reopened.Questions) != len(selected) || len(reopened.Rows) != len(selected) {
			t.Fatal("could not reopen selected challenges")
		}
		for i, number := range selected {
			q := catalogue.Questions[number-1]
			if reopened.Questions[i] != q || reopened.Rows[i].QuestionID != q.ID || reopened.Rows[i].Text != "account=101; question="+q.Prompt {
				t.Fatalf("reopened result lost challenge %d", number)
			}
		}
	}
}
func TestConfiguredParallelLimits(t *testing.T) {
	for _, limit := range []int{1} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			a, m := testAppWithParallel(t, limit)
			b := testBrowser{t: t, a: a}
			b.call("GET", "/api/session", nil, "")
			if status, session, _ := b.post("/api/login", map[string]string{"email": "admin@example.com", "password": "fixture-password"}); status != http.StatusOK || session["parallel"] != float64(limit) {
				t.Fatalf("expected configured limit %d in login response, got %v (HTTP %d)", limit, session["parallel"], status)
			}
			close(m.release)
			req := RunRequest{Epoch: b.epoch, ID: "1eee2222-0000-4000-8000-000000000006", Parallel: limit + 1, Accounts: []int64{101}, Questions: []string{a.groups[0][0].ID}}
			if status, body, _ := b.post("/api/runs", req); status != http.StatusBadRequest || body["code"] != "SELECTION" {
				t.Errorf("configured limit %d allowed parallel %d: HTTP %d, code %v", limit, req.Parallel, status, body["code"])
			}
			a.queue.wg.Wait()
			if len(m.calls) != 0 {
				t.Fatal("over-limit request issued probes", m.calls)
			}
		})
	}
	for _, invalid := range []int{0, 2, 4} {
		cfg := settings{Listen: "127.0.0.1:17861", Data: t.TempDir(), Parallel: invalid}
		if _, err := newApp(cfg, newIsolatedSub2()); err == nil {
			t.Fatalf("accepted server configuration with parallel %d", invalid)
		}
	}
}

func TestStaleEpochDoesNotIssueProbe(t *testing.T) {
	previous, _ := testApp(t)
	previousBrowser := testBrowser{t: t, a: previous}
	previousBrowser.call("GET", "/api/session", nil, "")
	a, m := testApp(t)
	b := testBrowser{t: t, a: a}
	b.call("GET", "/api/session", nil, "")
	if previousBrowser.epoch == "" || previousBrowser.epoch == b.epoch {
		t.Fatal("expected a new epoch after restart")
	}
	if status, _, _ := b.post("/api/login", map[string]string{"email": "admin@example.com", "password": "fixture-password"}); status != http.StatusOK {
		t.Fatal("login failed", status)
	}
	close(m.release)
	req := RunRequest{ID: "1eee2222-0000-4000-8000-000000000005", Epoch: previousBrowser.epoch, Accounts: []int64{101}, Questions: []string{a.groups[0][0].ID}, Parallel: 1}
	if status, body, _ := b.post("/api/runs", req); status != http.StatusConflict || body["code"] != "OLD_RUN" {
		t.Errorf("stale submission got %d, code %v", status, body["code"])
	}
	a.queue.wg.Wait()
	if len(m.calls) != 0 {
		t.Errorf("stale submission issued upstream probes: %v", m.calls)
	}
}

func TestCancelAndRestartClearResults(t *testing.T) {
	dir := t.TempDir()
	m := newIsolatedSub2()
	q, err := openQueue(dir, "fixture-bank", m.Probe)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	id := "1eee2222-0000-4000-8000-000000000002"
	qs := []Question{{ID: "q1", Prompt: "one"}, {ID: "q2", Prompt: "two"}}
	req := RunRequest{ID: id, Accounts: []int64{101}, Questions: []string{"q1", "q2"}, Parallel: 1}
	if err := q.start(context.Background(), 11, "fixture", req, map[int64]AccountView{101: m.listed[0]}, qs); err != nil {
		t.Fatal(err)
	}
	waitStart(t, m)
	if !q.busy() {
		t.Fatal("queue is working")
	}
	if _, err := q.get(12, id); err == nil {
		t.Fatal("cross-owner read")
	}
	if err := q.stopRun(12, id); err == nil {
		t.Fatal("cross-owner cancellation")
	}
	if err := q.stopRun(11, id); err != nil {
		t.Fatal(err)
	}
	r := waitRun(t, q, 11, id)
	if q.busy() || r.State != "cancelled" || r.Rows[1].Status != "cancelled" {
		t.Fatal("unexpected cancelled state")
	}
	q.close()
	restarted, err := openQueue(dir, "fixture-bank", m.Probe)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.close()
	if _, err := restarted.get(11, id); err != errAbsent || len(restarted.history(11)) != 0 {
		t.Fatal("answers survived restart")
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatal("queue wrote files", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.calls[101] != 1 {
		t.Fatal("a cancelled question was issued or replayed")
	}
}

func TestRetiredAndDuplicateSubmissionsDoNotReplay(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	q, err := openQueue(dir, "fixture-bank", func(ctx context.Context, jwt string, id int64, prompt string) (ProbeAnswer, error) {
		calls++
		return ProbeAnswer{Text: "private fixture answer"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer q.close()
	accounts := map[int64]AccountView{1: {ID: 1, Name: "demo"}}
	qs := []Question{{ID: "q", Prompt: "one"}}
	a := RunRequest{ID: "1eee2222-0000-4000-8000-000000000011", Epoch: "process", Accounts: []int64{1}, Questions: []string{"q"}, Parallel: 1}
	b := a
	b.ID = "1eee2222-0000-4000-8000-000000000012"
	start := func(req RunRequest) error { return q.start(context.Background(), 3, "fixture", req, accounts, qs) }
	if err := start(a); err != nil {
		t.Fatal(err)
	}
	first := waitRun(t, q, 3, a.ID)
	if first.Rows[0].Text != "private fixture answer" {
		t.Fatal("missing answer")
	}
	if err := start(a); err != nil {
		t.Fatal("duplicate submit", err)
	}
	changed := a
	changed.Parallel = 2
	if err := start(changed); err == nil {
		t.Fatal("changed parallel accepted")
	}
	bad := b
	bad.Accounts = []int64{2}
	if err := start(bad); err == nil {
		t.Fatal("unlisted account accepted")
	}
	if _, err := q.get(3, a.ID); err != nil {
		t.Fatal("invalid new batch erased previous answer")
	}
	if err := start(b); err != nil {
		t.Fatal(err)
	}
	waitRun(t, q, 3, b.ID)
	if _, err := q.get(3, a.ID); err != errAbsent {
		t.Fatal("old answer still readable")
	}
	if _, err := q.get(4, b.ID); err != errAbsent || len(q.history(4)) != 0 {
		t.Fatal("owner isolation failed")
	}
	if summaries := q.history(3); len(summaries) != 1 || summaries[0].ID != b.ID {
		t.Fatal("old answer still listed")
	}
	if err := start(a); err == nil || calls != 2 {
		t.Fatal("retired request replayed")
	}
	// An exhausted ID cache must stop accepting work instead of evicting old IDs.
	q.mu.Lock()
	for len(q.seen) < maxSeenSubmissions {
		q.seen[fmt.Sprint(len(q.seen))] = struct{}{}
	}
	q.mu.Unlock()
	c := a
	c.ID = "1eee2222-0000-4000-8000-000000000013"
	if err := start(c); err == nil || calls != 2 {
		t.Fatal("an ID was evicted")
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatal("retired answers persisted", err)
	}
}
func TestSelectionAndOrigin(t *testing.T) {
	a, _ := testApp(t)
	b := testBrowser{t: t, a: a}
	b.call("GET", "/api/session", nil, "")
	b.post("/api/login", map[string]string{"email": "admin@example.com", "password": "fixture-password"})
	req := RunRequest{Epoch: b.epoch, ID: "1eee2222-0000-4000-8000-000000000003", Accounts: []int64{101}, Parallel: 1, Questions: []string{"freeform prompt"}}
	if c, _, _ := b.post("/api/runs", req); c != 400 {
		t.Fatal(c)
	}
	r := httptest.NewRequest("GET", "http://wrong.invalid/api/session", nil)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("invalid Host accepted")
	}
	if c, _, _ := b.call("GET", "/api/runs", nil, ""); c != 200 {
		t.Fatal(c)
	}
}

func TestRejectedQuestionDoesNotSuppressIndependentChallenge(t *testing.T) {
	for _, tc := range []struct{ code, second string }{
		{"UPSTREAM_REFUSAL", "done"}, {"UPSTREAM_INCOMPLETE", "done"}, {"UPSTREAM_INPUT", "done"}, {"UPSTREAM_RATE_LIMIT", "failed"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			q, err := openQueue(t.TempDir(), "fixture-bank", func(ctx context.Context, jwt string, id int64, prompt string) (ProbeAnswer, error) {
				if prompt == "first" {
					return ProbeAnswer{}, gate(tc.code, "fixture failure")
				}
				return ProbeAnswer{Text: "independent answer"}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer q.close()
			req := RunRequest{ID: "1eee2222-0000-4000-8000-000000000004", Parallel: 1, Accounts: []int64{7}, Questions: []string{"q1", "q2"}}
			qs := []Question{{ID: "q1", Prompt: "first"}, {ID: "q2", Prompt: "second"}}
			if err := q.start(context.Background(), 11, "fixture", req, map[int64]AccountView{7: {ID: 7, Name: "demo"}}, qs); err != nil {
				t.Fatal(err)
			}
			r := waitRun(t, q, 11, req.ID)
			if r.Rows[0].Status != "failed" || r.Rows[1].Status != tc.second || (r.Rows[1].Started != "") != (tc.second == "done") {
				t.Fatal("wrong follow-up after", tc.code, r.Rows[1].Status)
			}
		})
	}
}
