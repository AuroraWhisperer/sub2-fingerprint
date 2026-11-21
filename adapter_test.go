package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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

func TestAdapterSelectedAccountAndIndependentAnswers(t *testing.T) {
	var lock sync.Mutex
	var sessions []string
	var details, exports atomic.Int32
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer admin-fixture" {
			t.Errorf("unexpected admin request %s", r.URL.Path)
			w.WriteHeader(401)
			return
		}
		accountID := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/accounts/")
		if accountID == "data" {
			accountID = r.URL.Query().Get("ids")
			if len(r.URL.Query()["ids"]) != 1 || (accountID != "9" && accountID != "17") {
				t.Error("unscoped export")
				w.WriteHeader(400)
				return
			}
			exports.Add(1)
			cred := map[string]any{"access_token": "bearer-" + accountID, "refresh_token": "must-not-leave-panel", "chatgpt_account_id": "identity-" + accountID, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
			mockOK(w, map[string]any{"accounts": []any{map[string]any{"name": "duplicate", "platform": "openai", "type": "oauth", "credentials": cred}}, "proxies": []any{}})
			return
		}
		var id int
		if accountID == "9" {
			id = 9
		} else if accountID == "17" {
			id = 17
		} else {
			t.Error("unexpected detail")
			w.WriteHeader(404)
			return
		}
		details.Add(1)
		mockOK(w, map[string]any{"id": id, "name": "duplicate", "status": "active", "schedulable": true, "current_concurrency": 0, "platform": "openai", "type": "oauth", "credentials": map[string]any{"chatgpt_account_id": "identity-" + accountID}})
	}))
	defer admin.Close()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Model     string
			Reasoning map[string]string
			Input     []struct {
				Role    string
				Content []struct{ Type, Text string }
			}
			Store        *bool
			Tools        []any
			Prev         *string `json:"previous_response_id"`
			Conversation *string `json:"conversation"`
		}
		err := json.NewDecoder(r.Body).Decode(&b)
		if err != nil || b.Store == nil || *b.Store || b.Model != probeModel || b.Reasoning["effort"] != "low" || len(b.Input) != 1 || len(b.Input[0].Content) != 1 || len(b.Tools) != 0 || b.Prev != nil || b.Conversation != nil {
			t.Error("upstream request not independent")
			w.WriteHeader(400)
			return
		}
		id := b.Input[0].Content[0].Text
		if r.Header.Get("Authorization") != "Bearer bearer-"+id || r.Header.Get("ChatGPT-Account-Id") != "identity-"+id {
			t.Error("account identity crossed")
		}
		sid := r.Header.Get("Session_id")
		lock.Lock()
		sessions = append(sessions, sid)
		lock.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"%s\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"gpt-6-astra\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"%s\"}]}]}}\n\n", id, id)
	}))
	defer upstream.Close()
	a, err := NewAdapter(admin.URL)
	if err != nil {
		t.Fatal(err)
	}
	a.upstream = upstream.URL
	a.upstreamTransport = upstream.Client().Transport.(*http.Transport)
	var wg sync.WaitGroup
	for _, id := range []int64{9, 17, 9} {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			want := fmt.Sprint(id)
			r, err := a.Probe(context.Background(), "admin-fixture", id, want)
			if err != nil || r.Text != want || r.Model != probeModel || r.RequestID == "" {
				t.Errorf("probe did not match %d: %v", id, err)
			}
		}(id)
	}
	wg.Wait()
	lock.Lock()
	defer lock.Unlock()
	unique := map[string]bool{}
	for _, s := range sessions {
		unique[s] = true
	}
	if len(unique) != 3 || unique[""] || exports.Load() != 3 || details.Load() != 3 {
		t.Fatal("expected fresh session, detail and scoped authorization for each question")
	}
}

func TestAdapterStopsBeforeUpstream(t *testing.T) {
	for _, test := range []struct {
		name, status string
		busy, count  int
		expiry       int64
		code         string
	}{
		{"busy", "active", 1, 1, 9999999999, "ACCOUNT_UNAVAILABLE"}, {"disabled", "disabled", 0, 1, 9999999999, "ACCOUNT_UNAVAILABLE"}, {"ambiguous export", "active", 0, 2, 9999999999, "ACCOUNT_MISMATCH"}, {"expired access token", "active", 0, 1, 1, "UPSTREAM_TOKEN"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var exports int
			panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/data") {
					exports++
					var accs []any
					for i := 0; i < test.count; i++ {
						accs = append(accs, map[string]any{"name": "test", "platform": "openai", "type": "oauth", "credentials": map[string]any{"chatgpt_account_id": "test", "access_token": "fixture", "expires_at": test.expiry}})
					}
					mockOK(w, map[string]any{"accounts": accs})
					return
				}
				mockOK(w, map[string]any{"id": 1, "name": "test", "platform": "openai", "type": "oauth", "schedulable": true, "current_concurrency": test.busy, "status": test.status})
			}))
			defer panel.Close()
			a, _ := NewAdapter(panel.URL)
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected upstream call") }))
			defer target.Close()
			a.upstream = target.URL
			_, err := a.Probe(context.Background(), "jwt", 1, "probe")
			assertGate(t, err, test.code)
			if test.code == "ACCOUNT_UNAVAILABLE" && exports != 0 {
				t.Error("ineligible account exported")
			}
		})
	}
}

func TestAdapterAuthorizationContracts(t *testing.T) {
	for _, bad := range []string{"http://ordinary.example", "https://name:code@localhost", "https://localhost/admin", "https://localhost?a=b"} {
		if _, err := NewAdapter(bad); err == nil {
			t.Error("accepted invalid management address")
		}
	}
	var loggedOut bool
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]string
		_ = json.NewDecoder(r.Body).Decode(&p)
		switch r.URL.Path {
		case "/api/v1/auth/login":
			mockOK(w, map[string]any{"requires_2fa": true, "temp_token": "temp"})
		case "/api/v1/auth/login/2fa":
			if p["temp_token"] != "temp" || p["totp_code"] != "111111" {
				w.WriteHeader(400)
				return
			}
			mockOK(w, map[string]any{"access_token": "jwt", "refresh_token": "refresh", "expires_in": 3600, "user": map[string]any{"role": "admin", "id": 3}})
		case "/api/v1/user/totp/step-up":
			if p["code"] != "222222" || r.Header.Get("Authorization") != "Bearer jwt" {
				w.WriteHeader(401)
				return
			}
			mockOK(w, map[string]any{"verified": true})
		case "/api/v1/admin/accounts":
			mockOK(w, map[string]any{"page": 1, "pages": 1, "items": []any{map[string]any{"id": 1, "name": "acct", "platform": "openai", "type": "oauth", "status": "active", "schedulable": true, "current_concurrency": 0, "credentials": map[string]any{"refresh_token": "sensitive", "access_token": "sensitive"}}}})
		case "/api/v1/auth/logout":
			loggedOut = p["refresh_token"] == "refresh"
			mockOK(w, map[string]any{})
		default:
			w.WriteHeader(404)
		}
	}))
	defer panel.Close()
	a, _ := NewAdapter(panel.URL)
	ctx := context.Background()
	r, err := a.Login(ctx, "mail", "secret")
	if err != nil || r.Temp != "temp" || r.Admin {
		t.Fatal("login 2FA response")
	}
	r, err = a.Login2FA(ctx, r.Temp, "111111")
	if err != nil || r.UserID != 3 || !r.Admin {
		t.Fatal("account binding")
	}
	if err = a.StepUp(ctx, r.JWT, "222222"); err != nil {
		t.Fatal(err)
	}
	rows, err := a.List(ctx, r.JWT)
	if err != nil || len(rows) != 1 || !rows[0].Eligible {
		t.Fatal("list account response")
	}
	safe, _ := json.Marshal(rows)
	if strings.Contains(string(safe), "sensitive") {
		t.Fatal("credential in list result")
	}
	if err = a.Logout(ctx, r.Refresh); err != nil || !loggedOut {
		t.Fatal("logout")
	}
}

func TestAdapterNeverFollowsAuthRedirectOrLeaksBody(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("credential-bearing redirect followed") }))
	defer other.Close()
	for _, status := range []int{307, 403} {
		panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", other.URL)
			w.WriteHeader(status)
			io.WriteString(w, `{"reason":"STEP_UP_REQUIRED","message":"private-body"}`)
		}))
		a, _ := NewAdapter(panel.URL)
		_, err := a.List(context.Background(), "test")
		panel.Close()
		if err == nil || strings.Contains(err.Error(), "private-body") {
			t.Fatal("unsafe error translation")
		}
		if status == 403 {
			assertGate(t, err, "STEP_UP_REQUIRED")
		}
	}
}

func TestAdapterMiddlewareAndHandlerStepUp(t *testing.T) {
	for _, tc := range []struct {
		status     int
		body, code string
	}{
		{403, `{"code":"STEP_UP_REQUIRED","message":"private-detail"}`, "STEP_UP_REQUIRED"},
		{403, `{"code":403,"reason":"STEP_UP_REQUIRED","message":"private-detail"}`, "STEP_UP_REQUIRED"},
		{403, `{"code":"STEP_UP_TOTP_NOT_ENABLED","message":"private-detail"}`, "STEP_UP_TOTP_NOT_ENABLED"},
		{503, `{"code":"STEP_UP_UNAVAILABLE","message":"private-detail"}`, "STEP_UP_UNAVAILABLE"},
		{403, `{"code":"STEP_UP_MCP_OAUTH_FORBIDDEN","message":"private-detail"}`, "FORBIDDEN"},
		{403, `{"code":"private-detail","message":"private-detail"}`, "FORBIDDEN"},
	} {
		panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			io.WriteString(w, tc.body)
		}))
		a, _ := NewAdapter(panel.URL)
		var result exportData
		err := a.call(context.Background(), "GET", "/admin/accounts/data?ids=1&include_proxies=true", "fixture", nil, &result)
		panel.Close()
		assertGate(t, err, tc.code)
		if strings.Contains(err.Error(), "private-detail") {
			t.Fatal("forwarded upstream detail")
		}
	}
}

func TestAdapterHonorsClientPolicyAndProxyExpiry(t *testing.T) {
	for _, changeBeforeExport := range []bool{false, true} {
		var exports int
		panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/data") {
				exports++
				mockOK(w, map[string]any{"accounts": []any{map[string]any{"name": "A", "type": "oauth", "platform": "openai", "extra": map[string]bool{"codex_cli_only": true}}}})
				return
			}
			mockOK(w, map[string]any{"id": 1, "name": "A", "type": "oauth", "platform": "openai", "status": "active", "schedulable": true, "current_concurrency": 0, "extra": map[string]bool{"codex_cli_only": !changeBeforeExport}})
		}))
		a, _ := NewAdapter(panel.URL)
		a.upstream = "http://invalid.invalid"
		_, err := a.Probe(context.Background(), "fixture", 1, "test")
		panel.Close()
		assertGate(t, err, "ACCOUNT_UNAVAILABLE")
		if !changeBeforeExport && exports != 0 {
			t.Fatal("read credential for an ineligible account")
		}
	}
	var account accountData
	var data exportData
	if err := json.Unmarshal([]byte(`{"id":1,"proxy_id":8,"proxy":{"id":8,"host":"proxy.example","port":1080,"protocol":"socks5","status":"active","username":"test","expires_at":"2060-01-01T00:00:00Z"}}`), &account); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"accounts":[{"proxy_key":"bound"}],"proxies":[{"proxy_key":"bound","protocol":"socks5","host":"proxy.example","port":1080,"username":"test","status":"active","expires_at":2840140800}]}`), &data); err != nil {
		t.Fatal(err)
	}
	if dst, err := data.proxy(account); err != nil || dst.Scheme != "socks5" || dst.Host != "proxy.example:1080" {
		t.Fatal("valid proxy rejected", err)
	}
	data.Proxies[0].Expires = json.RawMessage(`1`)
	_, err := data.proxy(account)
	assertGate(t, err, "PROXY_CONFIG")
	data.Proxies[0].Expires = nil
	account.Proxy.Expires = json.RawMessage(`"2000-01-01T00:00:00Z"`)
	_, err = data.proxy(account)
	assertGate(t, err, "PROXY_CONFIG")
}

func TestAdapterIncompleteStreamsAndTokenMismatch(t *testing.T) {
	for _, f := range []string{"data: {\"type\":\"response.output_text.delta\",\"delta\":\"123\"}\n\n", "data: [DONE]\n\n", "data: {\"type\":\"response.incomplete\"}\n\n", "data: {\"type\":\"response.refusal.delta\"}\n\n", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"function_call\"}]}}\n\n"} {
		r, err := readProbeStream(strings.NewReader(f))
		if err == nil || r.Text != "" {
			t.Error("invalid stream was scored")
		}
	}
	token := "aaa." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":9999999999,"https://api.openai.com/auth":{"chatgpt_account_id":"other"}}`)) + ".zzz"
	_, err := requestHeaders(accountData{}, credentialData{Token: token, AccountID: "selected"}, time.Now())
	assertGate(t, err, "IDENTITY")
}

func TestAdapterRefreshAndVerifyOwner(t *testing.T) {
	var refreshCalls, meCalls int
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/refresh":
			refreshCalls++
			var b map[string]string
			if r.Method != "POST" || r.Header.Get("Authorization") != "" || json.NewDecoder(r.Body).Decode(&b) != nil || len(b) != 1 || b["refresh_token"] != "old-refresh" {
				t.Error("wrong refresh request")
			}
			mockOK(w, map[string]any{"access_token": "new-access", "refresh_token": "new-refresh", "token_type": "Bearer", "expires_in": 1200})
		case "/api/v1/auth/me":
			meCalls++
			if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer new-access" {
				t.Error("wrong profile request")
			}
			if meCalls == 1 {
				w.WriteHeader(503)
				return
			}
			mockOK(w, map[string]any{"id": 9, "role": "admin", "status": "active"})
		default:
			t.Error("unexpected endpoint")
			w.WriteHeader(404)
		}
	}))
	defer panel.Close()
	a, _ := NewAdapter(panel.URL)
	ctx := context.Background()
	pair, err := a.Refresh(ctx, "old-refresh")
	if err != nil || pair.Admin || pair.UserID != 0 || pair.JWT != "new-access" || pair.Refresh != "new-refresh" || pair.ExpiresIn != 1200 {
		t.Fatal("wrong unverified rotation result", err)
	}
	assertGate(t, a.VerifyAdmin(ctx, pair.JWT, 9), "SESSION_TEMPORARY")
	if err = a.VerifyAdmin(ctx, pair.JWT, 9); err != nil {
		t.Fatal(err)
	}
	assertGate(t, a.VerifyAdmin(ctx, pair.JWT, 10), "SESSION_INVALID")
	if refreshCalls != 1 || meCalls != 3 {
		t.Fatal("verification retried a rotation")
	}
}

func TestAdapterSessionFailures(t *testing.T) {
	for _, tc := range []struct {
		name, op       string
		status         int
		response, want string
	}{
		{"expired access", "me", 401, `{"code":"TOKEN_EXPIRED"}`, "ACCESS_EXPIRED"},
		{"revoked", "me", 401, `{"code":"TOKEN_REVOKED"}`, "SESSION_INVALID"},
		{"expired refresh", "refresh", 401, `{"code":401,"reason":"REFRESH_TOKEN_EXPIRED"}`, "SESSION_INVALID"},
		{"used refresh", "refresh", 401, `{"code":401,"reason":"REFRESH_TOKEN_INVALID"}`, "SESSION_INVALID"},
		{"binding changed", "me", 401, `{"code":"SESSION_BINDING_MISMATCH"}`, "SESSION_INVALID"},
		{"disabled at refresh", "refresh", 403, `{"code":403,"reason":"USER_NOT_ACTIVE"}`, "SESSION_INVALID"},
		{"role downgrade", "me", 200, `{"code":0,"data":{"id":9,"role":"user","status":"active"}}`, "SESSION_INVALID"},
		{"disabled profile", "me", 200, `{"code":0,"data":{"id":9,"role":"admin","status":"disabled"}}`, "SESSION_INVALID"},
		{"backend downgrade", "refresh", 403, `{"code":403,"message":"Backend mode is active. Only admin login is allowed."}`, "SESSION_INVALID"},
		{"missing identity fields", "me", 200, `{"code":0,"data":{"user":{"id":9,"role":"admin"}}}`, "SESSION_TEMPORARY"},
		{"repeated refresh token", "refresh", 200, `{"code":0,"data":{"access_token":"new","refresh_token":"old","expires_in":300,"token_type":"Bearer"}}`, "SESSION_TEMPORARY"},
		{"unknown expiry", "refresh", 200, `{"code":0,"data":{"access_token":"new","refresh_token":"new-refresh","token_type":"Bearer"}}`, "SESSION_TEMPORARY"},
		{"rate limit", "refresh", 429, `{"code":429,"message":"example-sensitive"}`, "SESSION_TEMPORARY"},
		{"database offline", "me", 500, `{"code":"INTERNAL_ERROR","message":"example-sensitive"}`, "SESSION_TEMPORARY"},
		{"disconnected reverse proxy", "refresh", 401, `<html>example-sensitive</html>`, "SESSION_TEMPORARY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); io.WriteString(w, tc.response) }))
			defer panel.Close()
			a, _ := NewAdapter(panel.URL)
			var err error
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
