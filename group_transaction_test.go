package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type probeFixture struct {
	t *testing.T

	server  *httptest.Server
	adapter *Adapter

	mu             sync.Mutex
	initialGroups  []int64
	currentGroups  []int64
	updates        [][]int64
	detailReads    int
	verifyGroups   []int64
	groups         []groupData
	accountStatus  string
	schedulable    bool
	busy           int
	limit          int
	expires        *int64
	gatewayStatus  int
	gatewayCalls   atomic.Int32
	gatewayEntered chan struct{}
	gatewayOnce    sync.Once
	gatewayHandler func(http.ResponseWriter, *http.Request)
	failRestore    bool
	rejectAttach   int
	restoreJWT     string
	putCalls       int
	requestPayload map[string]any
	requestAuth    string
	requestContent string
	requestMu      sync.Mutex
}

func newProbeFixture(t *testing.T, initial []int64) *probeFixture {
	t.Helper()
	f := &probeFixture{
		t:              t,
		initialGroups:  append([]int64(nil), initial...),
		currentGroups:  append([]int64(nil), initial...),
		groups:         []groupData{{ID: 9, Name: "Probe"}, {ID: 2, Name: "Business"}},
		accountStatus:  "active",
		schedulable:    true,
		limit:          20,
		gatewayStatus:  http.StatusOK,
		gatewayEntered: make(chan struct{}),
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	var err error
	f.adapter, err = NewConfiguredAdapter(f.server.URL, "probe-key", "Probe")
	if err != nil {
		t.Fatal(err)
	}
	f.adapter.retryDelay = time.Millisecond
	return f
}

func (f *probeFixture) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/v1/admin/groups/all":
		f.mu.Lock()
		groups := append([]groupData(nil), f.groups...)
		f.mu.Unlock()
		mockOK(w, groups)
	case "/api/v1/admin/accounts":
		f.mu.Lock()
		row := f.accountMapLocked()
		f.mu.Unlock()
		mockOK(w, map[string]any{"page": 1, "pages": 1, "items": []any{row}})
	case "/api/v1/admin/accounts/1":
		if r.Method == http.MethodGet {
			f.writeDetail(w)
			return
		}
		if r.Method == http.MethodPut {
			f.update(w, r)
			return
		}
		http.Error(w, "method", http.StatusMethodNotAllowed)
	case "/v1/responses":
		f.gateway(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *probeFixture) accountMapLocked() map[string]any {
	row := map[string]any{
		"id":                  1,
		"name":                "fixture-account",
		"platform":            "openai",
		"type":                "oauth",
		"status":              f.accountStatus,
		"schedulable":         f.schedulable,
		"current_concurrency": f.busy,
		"concurrency":         f.limit,
		"group_ids":           append([]int64(nil), f.currentGroups...),
	}
	if f.expires != nil {
		row["expires_at"] = *f.expires
	}
	return row
}

func (f *probeFixture) writeDetail(w http.ResponseWriter) {
	f.mu.Lock()
	f.detailReads++
	groups := append([]int64(nil), f.currentGroups...)
	if len(f.updates) == 1 && f.verifyGroups != nil {
		groups = append([]int64(nil), f.verifyGroups...)
	}
	row := f.accountMapLocked()
	row["group_ids"] = groups
	f.mu.Unlock()
	mockOK(w, row)
}

func (f *probeFixture) update(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		GroupIDs []int64 `json:"group_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putCalls++
	isRestore := reflect.DeepEqual(payload.GroupIDs, f.initialGroups)
	if !isRestore && f.rejectAttach != 0 {
		w.WriteHeader(f.rejectAttach)
		if f.rejectAttach == http.StatusForbidden {
			_, _ = w.Write([]byte(`{"code":"STEP_UP_REQUIRED"}`))
		}
		return
	}
	if isRestore && f.restoreJWT != "" && r.Header.Get("Authorization") != "Bearer "+f.restoreJWT {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if isRestore && f.failRestore {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"restore_failed"}`))
		return
	}
	f.currentGroups = append([]int64(nil), payload.GroupIDs...)
	f.updates = append(f.updates, append([]int64(nil), payload.GroupIDs...))
	mockOK(w, map[string]any{})
}

func (f *probeFixture) gateway(w http.ResponseWriter, r *http.Request) {
	f.gatewayCalls.Add(1)
	f.gatewayOnce.Do(func() { close(f.gatewayEntered) })
	if f.gatewayHandler != nil {
		f.gatewayHandler(w, r)
		return
	}
	f.requestAuth = r.Header.Get("Authorization")
	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		f.t.Errorf("invalid gateway payload: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.requestMu.Lock()
	f.requestPayload = payload
	if input, ok := payload["input"].([]any); ok && len(input) == 1 {
		if item, ok := input[0].(map[string]any); ok {
			if content, ok := item["content"].([]any); ok && len(content) == 1 {
				if part, ok := content[0].(map[string]any); ok {
					f.requestContent, _ = part["text"].(string)
				}
			}
		}
	}
	f.requestMu.Unlock()
	if f.gatewayStatus != http.StatusOK {
		w.WriteHeader(f.gatewayStatus)
		_, _ = w.Write([]byte("private gateway details"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Request-Id", "req_0123456789abcdef")
	_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"42\"}\n\n")
	_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"gpt-6-astra\",\"output\":[]}}\n\n")
}

func (f *probeFixture) groupsSnapshot() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.currentGroups...)
}

func (f *probeFixture) checkDetailReads(want int) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.detailReads != want {
		f.t.Errorf("account detail reads: got %d, want %d", f.detailReads, want)
	}
}

func (f *probeFixture) updateSnapshot() [][]int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([][]int64, len(f.updates))
	for i, groups := range f.updates {
		result[i] = append([]int64(nil), groups...)
	}
	return result
}

func TestProbeGroupTransactionAddsVerifiesRequestsAndRestores(t *testing.T) {
	f := newProbeFixture(t, []int64{2, 3})
	answer, err := f.adapter.Probe(context.Background(), fixedJWT("admin-fixture"), 1, "add 20 and 22")
	if err != nil {
		t.Fatal(err)
	}
	if answer.Text != "42" || answer.Model != "gpt-6-astra" || answer.RequestID == "" {
		t.Fatalf("unexpected answer: %+v", answer)
	}
	if got := f.groupsSnapshot(); !reflect.DeepEqual(got, f.initialGroups) {
		t.Fatalf("groups were not restored: %v", got)
	}
	updates := f.updateSnapshot()
	if len(updates) != 2 || !reflect.DeepEqual(updates[0], []int64{2, 3, 9}) || !reflect.DeepEqual(updates[1], []int64{2, 3}) {
		t.Fatalf("unexpected group updates: %v", updates)
	}
	if f.gatewayCalls.Load() != 1 {
		t.Fatalf("expected one model request, got %d", f.gatewayCalls.Load())
	}
	f.checkDetailReads(3) // Initial state, verified attachment, verified restoration.
	f.requestMu.Lock()
	defer f.requestMu.Unlock()
	if f.requestAuth != "Bearer probe-key" || f.requestContent != "add 20 and 22" {
		t.Fatalf("gateway identity or prompt mismatch: auth=%q prompt=%q", f.requestAuth, f.requestContent)
	}
	payload := f.requestPayload
	if payload["model"] != "gpt-6-astra" || payload["stream"] != true || payload["store"] != false {
		t.Fatalf("fixed request fields mismatch: %#v", payload)
	}
	reasoning, ok := payload["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "low" {
		t.Fatalf("reasoning mismatch: %#v", payload["reasoning"])
	}
	if payload["instructions"] != probeInstruction {
		t.Fatalf("instruction mismatch: %#v", payload["instructions"])
	}
}

func TestProbeBatchKeepsProbeMembershipUntilAllRequestsFinish(t *testing.T) {
	f := newProbeFixture(t, []int64{2, 3})
	f.limit = 3
	release := make(chan struct{})
	threeEntered := make(chan struct{})
	var enteredOnce sync.Once
	var active atomic.Int32
	var peak atomic.Int32
	f.gatewayHandler = func(w http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		for {
			previous := peak.Load()
			if current <= previous || peak.CompareAndSwap(previous, current) {
				break
			}
		}
		if current == 3 {
			enteredOnce.Do(func() { close(threeEntered) })
		}
		<-release
		active.Add(-1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"42\"}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"gpt-6-astra\",\"output\":[]}}\n\n")
	}

	type result struct {
		index  int
		answer ProbeAnswer
		err    error
	}
	results := make(chan result, 3)
	done := make(chan error, 1)
	go func() {
		done <- f.adapter.ProbeBatch(context.Background(), fixedJWT("admin-fixture"), 1,
			[]string{"first", "second", "third"},
			func(index int, answer ProbeAnswer, err error) {
				results <- result{index: index, answer: answer, err: err}
			})
	}()

	select {
	case <-threeEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("batch did not start three model requests")
	}
	if got := f.groupsSnapshot(); !reflect.DeepEqual(got, []int64{2, 3, 9}) {
		t.Fatalf("Probe membership was removed while requests were active: %v", got)
	}
	if updates := f.updateSnapshot(); len(updates) != 1 {
		t.Fatalf("batch performed unexpected group updates while active: %v", updates)
	}
	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("batch did not finish")
	}
	seen := map[int]bool{}
	for range 3 {
		select {
		case result := <-results:
			seen[result.index] = true
			if result.err != nil || result.answer.Text != "42" {
				t.Fatalf("unexpected batch result: %+v", result)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("missing batch result")
		}
	}
	if len(seen) != 3 || peak.Load() != 3 {
		t.Fatalf("batch did not run three requests concurrently: seen=%v peak=%d", seen, peak.Load())
	}
	f.checkDetailReads(3)
	if got := f.groupsSnapshot(); !reflect.DeepEqual(got, f.initialGroups) {
		t.Fatalf("groups were not restored after batch: %v", got)
	}
	updates := f.updateSnapshot()
	if len(updates) != 2 || !reflect.DeepEqual(updates[1], f.initialGroups) {
		t.Fatalf("unexpected batch group updates: %v", updates)
	}
}

func TestProbeBatchRespectsAvailableAccountCapacity(t *testing.T) {
	f := newProbeFixture(t, []int64{2})
	f.limit = 1
	var active atomic.Int32
	var peak atomic.Int32
	release := make(chan struct{})
	f.gatewayHandler = func(w http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		for {
			previous := peak.Load()
			if current <= previous || peak.CompareAndSwap(previous, current) {
				break
			}
		}
		<-release
		active.Add(-1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"42\"}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"gpt-6-astra\",\"output\":[]}}\n\n")
	}
	done := make(chan error, 1)
	go func() {
		done <- f.adapter.ProbeBatch(context.Background(), fixedJWT("admin-fixture"), 1,
			[]string{"first", "second", "third"}, nil)
	}()
	select {
	case <-f.gatewayEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("capacity-limited batch did not start")
	}
	time.Sleep(50 * time.Millisecond)
	if peak.Load() != 1 {
		t.Fatalf("batch exceeded account capacity: peak=%d", peak.Load())
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("capacity-limited batch did not finish")
	}
}

func TestProbeBatchUsesIndependentRequestTimeoutsForCapacityWaves(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
	}{
		{name: "one available slot", limit: 1},
		{name: "two available slots", limit: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newProbeFixture(t, []int64{2})
			f.limit = tc.limit
			f.adapter.requestTimeout = 25 * time.Millisecond
			f.gatewayHandler = func(_ http.ResponseWriter, _ *http.Request) {
				time.Sleep(75 * time.Millisecond)
			}

			results := make(chan error, 3)
			err := f.adapter.ProbeBatch(context.Background(), fixedJWT("admin-fixture"), 1,
				[]string{"first", "second", "third"},
				func(_ int, _ ProbeAnswer, err error) {
					results <- err
				})
			if err != nil {
				t.Fatal(err)
			}
			for index := 0; index < 3; index++ {
				assertGate(t, <-results, "TIMEOUT")
			}
			if got := f.groupsSnapshot(); !reflect.DeepEqual(got, f.initialGroups) {
				t.Fatalf("groups were not restored after all timed-out requests: %v", got)
			}
			if updates := f.updateSnapshot(); len(updates) != 2 || !reflect.DeepEqual(updates[1], f.initialGroups) {
				t.Fatalf("unexpected group updates after timed-out batch: %v", updates)
			}
		})
	}
}

func TestProbeBatchRestoresAfterCancellation(t *testing.T) {
	f := newProbeFixture(t, []int64{2})
	f.limit = 3
	release := make(chan struct{})
	f.gatewayHandler = func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"42\"}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"gpt-6-astra\",\"output\":[]}}\n\n")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan error, 3)
	done := make(chan error, 1)
	go func() {
		done <- f.adapter.ProbeBatch(ctx, fixedJWT("admin-fixture"), 1,
			[]string{"first", "second", "third"},
			func(_ int, _ ProbeAnswer, err error) {
				results <- err
			})
	}()

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for f.gatewayCalls.Load() < 3 {
		select {
		case <-deadline.C:
			t.Fatal("cancellation batch did not start all requests")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation batch did not finish")
	}
	for index := 0; index < 3; index++ {
		<-results
	}
	if got := f.groupsSnapshot(); !reflect.DeepEqual(got, f.initialGroups) {
		t.Fatalf("groups were not restored after cancellation: %v", got)
	}
	if updates := f.updateSnapshot(); len(updates) != 2 || !reflect.DeepEqual(updates[1], f.initialGroups) {
		t.Fatalf("unexpected group updates after cancellation: %v", updates)
	}
}

func TestProbePreservesExistingProbeMembership(t *testing.T) {
	f := newProbeFixture(t, []int64{2, 9})
	if _, err := f.adapter.Probe(context.Background(), fixedJWT("admin-fixture"), 1, "already attached"); err != nil {
		t.Fatal(err)
	}
	if got := f.groupsSnapshot(); !reflect.DeepEqual(got, []int64{2, 9}) {
		t.Fatalf("existing Probe membership changed: %v", got)
	}
	if len(f.updateSnapshot()) != 0 {
		t.Fatalf("existing membership caused a mutation: %v", f.updateSnapshot())
	}
	f.checkDetailReads(1)
}

func TestProbeDoesNotCallGatewayWhenAttachVerificationFails(t *testing.T) {
	f := newProbeFixture(t, []int64{2})
	f.verifyGroups = []int64{2}
	_, err := f.adapter.Probe(context.Background(), fixedJWT("admin-fixture"), 1, "must not send")
	assertGate(t, err, "GROUP_ATTACH_FAILED")
	if f.gatewayCalls.Load() != 0 {
		t.Fatal("gateway was called before group attachment verification")
	}
	if got := f.groupsSnapshot(); !reflect.DeepEqual(got, []int64{2}) {
		t.Fatalf("failed attachment was not restored: %v", got)
	}
	updates := f.updateSnapshot()
	if len(updates) != 2 || !reflect.DeepEqual(updates[1], []int64{2}) {
		t.Fatalf("unexpected recovery updates: %v", updates)
	}
}

func TestProbeRestoresAfterCancellation(t *testing.T) {
	f := newProbeFixture(t, []int64{2})
	releaseGateway := make(chan struct{})
	f.gatewayHandler = func(w http.ResponseWriter, r *http.Request) {
		<-releaseGateway
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := f.adapter.Probe(ctx, fixedJWT("admin-fixture"), 1, "cancel me")
		done <- err
	}()
	select {
	case <-f.gatewayEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("gateway request did not start")
	}
	cancel()
	select {
	case err := <-done:
		assertGate(t, err, "CANCELED")
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled probe did not finish")
	}
	close(releaseGateway)
	if got := f.groupsSnapshot(); !reflect.DeepEqual(got, []int64{2}) {
		t.Fatalf("cancelled probe did not restore groups: %v", got)
	}
}

func TestProbeRestoreFailureBlocksLaterMutations(t *testing.T) {
	f := newProbeFixture(t, []int64{2})
	f.gatewayStatus = http.StatusServiceUnavailable
	f.failRestore = true
	_, err := f.adapter.Probe(context.Background(), fixedJWT("admin-fixture"), 1, "no account")
	assertGate(t, err, "GROUP_RESTORE_FAILED")
	if f.adapter.recoveryFailed != true {
		t.Fatal("recovery failure latch was not set")
	}
	updatesBefore := len(f.updateSnapshot())
	_, err = f.adapter.Probe(context.Background(), fixedJWT("admin-fixture"), 1, "blocked")
	assertGate(t, err, "GROUP_RECOVERY_BLOCKED")
	if len(f.updateSnapshot()) != updatesBefore {
		t.Fatal("blocked transaction changed account groups")
	}
}

func TestRejectedAttachDoesNotRestoreOrBlockLaterProbes(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{400, "ADMIN_DETAIL_BAD_REQUEST"},
		{401, "UNAUTHORIZED"},
		{403, "STEP_UP_REQUIRED"},
		{404, "NOT_FOUND"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			f := newProbeFixture(t, []int64{2})
			f.rejectAttach = tc.status
			_, err := f.adapter.Probe(context.Background(), fixedJWT("admin-fixture"), 1, "not sent")
			assertGate(t, err, tc.code)
			f.mu.Lock()
			puts := f.putCalls
			f.mu.Unlock()
			if puts != 1 || f.adapter.recoveryBlocked() || f.gatewayCalls.Load() != 0 {
				t.Fatalf("rejected attach triggered follow-up work: PUTs=%d", puts)
			}
		})
	}
}

func TestProbeCleanupUsesRenewedJWT(t *testing.T) {
	f := newProbeFixture(t, []int64{2})
	f.restoreJWT = "renewed"
	var renewed atomic.Bool
	f.gatewayHandler = func(w http.ResponseWriter, _ *http.Request) {
		renewed.Store(true)
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"42\"}\n\n",
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")
	}
	token := func() string {
		if renewed.Load() {
			return "renewed"
		}
		return "original"
	}
	answer, err := f.adapter.Probe(context.Background(), token, 1, "test")
	if err != nil || answer.Text != "42" || !reflect.DeepEqual(f.groupsSnapshot(), []int64{2}) {
		t.Fatalf("cleanup with renewed token failed: answer=%+v err=%v", answer, err)
	}
}

func TestProbeNoAccount503IsReturnedAfterSuccessfulRecovery(t *testing.T) {
	f := newProbeFixture(t, []int64{2})
	f.gatewayStatus = http.StatusServiceUnavailable
	_, err := f.adapter.Probe(context.Background(), fixedJWT("admin-fixture"), 1, "no account")
	assertGate(t, err, "PROBE_NO_ACCOUNT")
	if got := f.gatewayCalls.Load(); got != 5 {
		t.Fatalf("expected five bounded attempts, got %d", got)
	}
	if got := f.groupsSnapshot(); !reflect.DeepEqual(got, []int64{2}) {
		t.Fatalf("503 probe did not restore groups: %v", got)
	}
}

func TestProbeRejectsAmbiguousProbeGroup(t *testing.T) {
	f := newProbeFixture(t, []int64{2})
	f.groups = []groupData{{ID: 9, Name: "Probe"}, {ID: 10, Name: "Probe"}}
	_, err := f.adapter.Probe(context.Background(), fixedJWT("admin-fixture"), 1, "ambiguous")
	assertGate(t, err, "GROUP_AMBIGUOUS")
	if f.gatewayCalls.Load() != 0 || len(f.updateSnapshot()) != 0 {
		t.Fatal("ambiguous Probe group caused a mutation or model request")
	}
}
