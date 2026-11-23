package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mockOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": v})
}

func assertGate(t *testing.T, err error, code string) {
	t.Helper()
	var e *GateError
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("expected gate %s, received %v", code, err)
	}
}

func fixedJWT(jwt string) probeToken { return func() string { return jwt } }

func TestAdapterAuthorizationContracts(t *testing.T) {
	for _, bad := range []string{
		"http://ordinary.example",
		"https://name:code@localhost",
		"https://localhost/admin",
		"https://localhost?a=b",
	} {
		if _, err := NewAdapter(bad); err == nil {
			t.Errorf("accepted invalid management address %q", bad)
		}
	}
	for _, badKey := range []string{"contains space", "line\nbreak"} {
		if _, err := NewConfiguredAdapter("http://127.0.0.1:8080", badKey, "Probe"); err == nil {
			t.Errorf("accepted invalid Probe key %q", badKey)
		}
	}
}

func TestAdapterLoginListStepUpAndLogout(t *testing.T) {
	var loggedOut bool
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]string
		_ = json.NewDecoder(r.Body).Decode(&p)
		switch r.URL.Path {
		case "/api/v1/auth/login":
			mockOK(w, map[string]any{"requires_2fa": true, "temp_token": "temp"})
		case "/api/v1/auth/login/2fa":
			if p["temp_token"] != "temp" || p["totp_code"] != "111111" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mockOK(w, map[string]any{
				"access_token": "jwt", "refresh_token": "refresh", "expires_in": 3600,
				"user": map[string]any{"role": "admin", "id": 3},
			})
		case "/api/v1/user/totp/step-up":
			if p["code"] != "222222" || r.Header.Get("Authorization") != "Bearer jwt" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			mockOK(w, map[string]any{"verified": true})
		case "/api/v1/admin/accounts":
			mockOK(w, map[string]any{
				"page": 1, "pages": 1,
				"items": []any{map[string]any{
					"id": 1, "name": "acct", "platform": "openai", "type": "oauth", "status": "active",
					"schedulable": true, "current_concurrency": 0, "concurrency": 20,
					"credentials": map[string]any{"refresh_token": "sensitive", "access_token": "sensitive"},
				}},
			})
		case "/api/v1/auth/logout":
			loggedOut = p["refresh_token"] == "refresh"
			mockOK(w, map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer panel.Close()

	a, err := NewAdapter(panel.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := a.Login(ctx, "mail", "secret")
	if err != nil || r.Temp != "temp" || r.Admin {
		t.Fatalf("login 2FA response: %+v, %v", r, err)
	}
	r, err = a.Login2FA(ctx, r.Temp, "111111")
	if err != nil || r.UserID != 3 || !r.Admin {
		t.Fatalf("account binding: %+v, %v", r, err)
	}
	if err = a.StepUp(ctx, r.JWT, "222222"); err != nil {
		t.Fatal(err)
	}
	rows, err := a.List(ctx, r.JWT)
	if err != nil || len(rows) != 1 || !rows[0].Eligible {
		t.Fatalf("list account response: %+v, %v", rows, err)
	}
	safe, _ := json.Marshal(rows)
	if strings.Contains(string(safe), "sensitive") {
		t.Fatal("credential entered account list result")
	}
	if err = a.Logout(ctx, r.Refresh); err != nil || !loggedOut {
		t.Fatalf("logout: %v", err)
	}
}

func TestAdapterNeverFollowsAuthRedirectOrLeaksBody(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("credential-bearing redirect followed")
	}))
	defer other.Close()
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusForbidden} {
		panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", other.URL)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"reason":"STEP_UP_REQUIRED","message":"private-body"}`)
		}))
		a, err := NewAdapter(panel.URL)
		if err != nil {
			t.Fatal(err)
		}
		err = a.call(context.Background(), http.MethodGet, "/admin/accounts?page=1", "test", nil, &struct{}{})
		panel.Close()
		if err == nil || strings.Contains(err.Error(), "private-body") {
			t.Fatal("unsafe error translation")
		}
		if status == http.StatusForbidden {
			assertGate(t, err, "STEP_UP_REQUIRED")
		}
	}
}

func TestAdapterAdminFailuresAreClassifiedWithoutRemoteText(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		code   string
	}{
		{http.StatusForbidden, `{"code":"STEP_UP_REQUIRED","message":"private-detail"}`, "STEP_UP_REQUIRED"},
		{http.StatusForbidden, `{"code":403,"reason":"STEP_UP_REQUIRED","message":"private-detail"}`, "STEP_UP_REQUIRED"},
		{http.StatusForbidden, `{"code":"STEP_UP_TOTP_NOT_ENABLED","message":"private-detail"}`, "STEP_UP_TOTP_NOT_ENABLED"},
		{http.StatusServiceUnavailable, `{"code":"STEP_UP_UNAVAILABLE","message":"private-detail"}`, "STEP_UP_UNAVAILABLE"},
		{http.StatusForbidden, `{"code":"STEP_UP_MCP_OAUTH_FORBIDDEN","message":"private-detail"}`, "FORBIDDEN"},
		{http.StatusForbidden, `{"code":"private-detail","message":"private-detail"}`, "FORBIDDEN"},
	} {
		panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))
		a, err := NewAdapter(panel.URL)
		if err != nil {
			t.Fatal(err)
		}
		err = a.call(context.Background(), http.MethodGet, "/admin/accounts/data?ids=1", "fixture", nil, &struct{}{})
		panel.Close()
		assertGate(t, err, tc.code)
		if strings.Contains(err.Error(), "private-detail") {
			t.Fatal("forwarded upstream detail")
		}
	}
}

func TestAdapterRefreshAndVerifyOwner(t *testing.T) {
	var refreshCalls, meCalls int
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/refresh":
			refreshCalls++
			var b map[string]string
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" ||
				json.NewDecoder(r.Body).Decode(&b) != nil || len(b) != 1 || b["refresh_token"] != "old-refresh" {
				t.Error("wrong refresh request")
			}
			mockOK(w, map[string]any{
				"access_token": "new-access", "refresh_token": "new-refresh",
				"token_type": "Bearer", "expires_in": 1200,
			})
		case "/api/v1/auth/me":
			meCalls++
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer new-access" {
				t.Error("wrong profile request")
			}
			if meCalls == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			mockOK(w, map[string]any{"id": 9, "role": "admin", "status": "active"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer panel.Close()
	a, err := NewAdapter(panel.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pair, err := a.Refresh(ctx, "old-refresh")
	if err != nil || pair.Admin || pair.UserID != 0 || pair.JWT != "new-access" || pair.Refresh != "new-refresh" || pair.ExpiresIn != 1200 {
		t.Fatalf("wrong unverified rotation result: %+v, %v", pair, err)
	}
	assertGate(t, a.VerifyAdmin(ctx, pair.JWT, 9), "SESSION_TEMPORARY")
	if err = a.VerifyAdmin(ctx, pair.JWT, 9); err != nil {
		t.Fatal(err)
	}
	assertGate(t, a.VerifyAdmin(ctx, pair.JWT, 10), "SESSION_INVALID")
	if refreshCalls != 1 || meCalls != 3 {
		t.Fatalf("verification retried a rotation: refresh=%d me=%d", refreshCalls, meCalls)
	}
}

func TestAdapterCurrentAdminValidatesBrowserAccessToken(t *testing.T) {
	role := "admin"
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/me" || r.Header.Get("Authorization") != "Bearer browser-token" {
			t.Fatalf("wrong browser profile request: %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		mockOK(w, map[string]any{"id": 17, "role": role, "status": "active"})
	}))
	defer panel.Close()
	a, err := NewAdapter(panel.URL)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := a.CurrentAdmin(context.Background(), "browser-token"); err != nil || id != 17 {
		t.Fatalf("valid administrator token rejected: id=%d err=%v", id, err)
	}
	role = "user"
	if _, err := a.CurrentAdmin(context.Background(), "browser-token"); err == nil {
		t.Fatal("non-administrator token accepted")
	}
}

func TestAdapterSessionFailures(t *testing.T) {
	for _, tc := range []struct {
		name, op       string
		status         int
		response, want string
	}{
		{"expired access", "me", http.StatusUnauthorized, `{"code":"TOKEN_EXPIRED"}`, "ACCESS_EXPIRED"},
		{"revoked", "me", http.StatusUnauthorized, `{"code":"TOKEN_REVOKED"}`, "SESSION_INVALID"},
		{"expired refresh", "refresh", http.StatusUnauthorized, `{"code":401,"reason":"REFRESH_TOKEN_EXPIRED"}`, "SESSION_INVALID"},
		{"used refresh", "refresh", http.StatusUnauthorized, `{"code":401,"reason":"REFRESH_TOKEN_INVALID"}`, "SESSION_INVALID"},
		{"binding changed", "me", http.StatusUnauthorized, `{"code":"SESSION_BINDING_MISMATCH"}`, "SESSION_INVALID"},
		{"disabled at refresh", "refresh", http.StatusForbidden, `{"code":403,"reason":"USER_NOT_ACTIVE"}`, "SESSION_INVALID"},
		{"role downgrade", "me", http.StatusOK, `{"code":0,"data":{"id":9,"role":"user","status":"active"}}`, "SESSION_INVALID"},
		{"disabled profile", "me", http.StatusOK, `{"code":0,"data":{"id":9,"role":"admin","status":"disabled"}}`, "SESSION_INVALID"},
		{"backend downgrade", "refresh", http.StatusForbidden, `{"code":403,"message":"Backend mode is active. Only admin login is allowed."}`, "SESSION_INVALID"},
		{"missing identity fields", "me", http.StatusOK, `{"code":0,"data":{"user":{"id":9,"role":"admin"}}}`, "SESSION_TEMPORARY"},
		{"repeated refresh token", "refresh", http.StatusOK, `{"code":0,"data":{"access_token":"new","refresh_token":"old","expires_in":300,"token_type":"Bearer"}}`, "SESSION_TEMPORARY"},
		{"unknown expiry", "refresh", http.StatusOK, `{"code":0,"data":{"access_token":"new","refresh_token":"new-refresh","token_type":"Bearer"}}`, "SESSION_TEMPORARY"},
		{"rate limit", "refresh", http.StatusTooManyRequests, `{"code":429,"message":"example-sensitive"}`, "SESSION_TEMPORARY"},
		{"database offline", "me", http.StatusInternalServerError, `{"code":"INTERNAL_ERROR","message":"example-sensitive"}`, "SESSION_TEMPORARY"},
		{"disconnected reverse proxy", "refresh", http.StatusUnauthorized, `<html>example-sensitive</html>`, "SESSION_TEMPORARY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.response)
			}))
			defer panel.Close()
			a, err := NewAdapter(panel.URL)
			if err != nil {
				t.Fatal(err)
			}
			if tc.op == "refresh" {
				_, err = a.Refresh(context.Background(), "old")
			} else {
				err = a.VerifyAdmin(context.Background(), "jwt", 9)
			}
			assertGate(t, err, tc.want)
			if strings.Contains(err.Error(), "example-sensitive") {
				t.Fatal("remote error text leaked")
			}
		})
	}
}

func TestAdminBadRequestShowsStageWithoutEchoingPrivateDetails(t *testing.T) {
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"code":400,"reason":"fixture-private-information","message":"fixture-private-information"}`)
	}))
	defer panel.Close()
	a, err := NewAdapter(panel.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, code string
	}{
		{"/admin/accounts?page=1", "ADMIN_LIST_BAD_REQUEST"},
		{"/admin/accounts/17", "ADMIN_DETAIL_BAD_REQUEST"},
		{"/admin/accounts/data?ids=17&include_proxies=true", "ADMIN_EXPORT_BAD_REQUEST"},
	} {
		err := a.call(context.Background(), http.MethodGet, tc.path, "fixture", nil, &struct{}{})
		assertGate(t, err, tc.code)
		if strings.Contains(err.Error(), "fixture-private-information") || !strings.Contains(err.Error(), "HTTP 400") {
			t.Fatal("admin diagnostics lost the HTTP status or exposed remote content", tc.code)
		}
	}
}

func TestProbeStreamScoresOnlyTheFinalAnswer(t *testing.T) {
	const note = `{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Thinking about 999"}]}`
	const final = `{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"42"}]}`
	frame := func(s string) string { return "data: " + s + "\n\n" }
	streamed := frame(`{"type":"response.output_item.done","item":`+note+"}") + frame(`{"type":"response.output_item.done","item":`+final+"}")
	for _, end := range []string{`[]`, `[` + note + `,` + final + `]`} {
		answer, err := readProbeStream(context.Background(), strings.NewReader(streamed+frame(`{"type":"response.completed","response":{"status":"completed","model":"gpt-6-astra","output":`+end+"}}")))
		if err != nil || answer.Text != "42" {
			t.Fatalf("incorrect final answer: %q, %v", answer.Text, err)
		}
	}
	_, err := readProbeStream(context.Background(), strings.NewReader(frame(`{"type":"response.output_text.delta","delta":"Thinking about 999"}`)+frame(`{"type":"response.completed","response":{"status":"completed","output":[`+note+`]}}`)))
	assertGate(t, err, "UPSTREAM_INCOMPLETE")
}

type failedProbeStream struct{ err error }

func (s failedProbeStream) Read([]byte) (int, error) { return 0, s.err }

func TestProbeStreamClassifiesReadErrorsAndLargeFrames(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{context.DeadlineExceeded, "TIMEOUT"},
		{context.Canceled, "CANCELED"},
		{io.ErrUnexpectedEOF, "NETWORK"},
		{io.EOF, "UPSTREAM_INCOMPLETE"},
	} {
		_, err := readProbeStream(context.Background(), failedProbeStream{tc.err})
		assertGate(t, err, tc.code)
	}
	ctx := context.Background()
	stream := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"42\",\"extra\":\"" +
		strings.Repeat("x", 256<<10) + "\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n"
	if answer, err := readProbeStream(ctx, strings.NewReader(stream)); err != nil || answer.Text != "42" {
		t.Fatalf("valid large frame: answer=%+v err=%v", answer, err)
	}
	_, err := readProbeStream(ctx, strings.NewReader(strings.Repeat("x", 513<<10)))
	assertGate(t, err, "UPSTREAM_SIZE")
	_, err = readProbeStream(ctx, strings.NewReader(strings.Repeat(": comment\n", 230000)))
	assertGate(t, err, "UPSTREAM_SIZE")
}

func TestProbeGateway503IsClassifiedWithoutLeakingBody(t *testing.T) {
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/responses" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "private upstream details")
			return
		}
		http.NotFound(w, r)
	}))
	defer panel.Close()
	a, err := NewConfiguredAdapter(panel.URL, "probe-key", "Probe")
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.gatewayProbeOnce(context.Background(), "42")
	assertGate(t, err, "PROBE_NO_ACCOUNT")
	if strings.Contains(err.Error(), "private upstream details") {
		t.Fatal("gateway body leaked")
	}
}

func TestHTTPFailureHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assertGate(t, httpFailure(ctx, context.Canceled, "测试 "), "CANCELED")
}

func TestAccountEligibilityRequiresCapacityFields(t *testing.T) {
	row := accountData{ID: 1, Platform: "openai", Type: "oauth", Status: "active", Schedulable: boolPtr(true)}
	if got := row.eligibility(time.Now()); got == "" {
		t.Fatal("accepted an account with missing capacity fields")
	}
}

func boolPtr(v bool) *bool { return &v }
