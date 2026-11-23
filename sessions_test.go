package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func (m *isolatedSub2) Refresh(context.Context, string) (LoginResult, error) {
	return LoginResult{JWT: "renewed-access", Refresh: "rotated-refresh", ExpiresIn: 1800}, nil
}
func (m *isolatedSub2) VerifyAdmin(context.Context, string, int64) error { return nil }

func TestExternalSessionRenewalUpdatesRunCredential(t *testing.T) {
	a, _ := testApp(t)
	b := testBrowser{t: t, a: a, token: "original"}
	if code, _, _ := b.call("GET", "/api/session", nil, ""); code != 200 {
		t.Fatal(code)
	}
	s := a.sessions[cookieKey(b.cookie.Value)]
	if _, deadline := s.ctx.Deadline(); deadline || !s.external {
		t.Fatal("external session was published with a fixed deadline or missing source")
	}
	credential := s.probeJWT
	b.token = "renewed"
	if code, _, _ := b.call("GET", "/api/session", nil, ""); code != 200 || *credential.Load() != b.token {
		t.Fatal("renewal did not update the original credential slot")
	}
	if s.ctx.Err() != nil {
		t.Fatal("renewal cancelled the session")
	}
	s.mu.Lock()
	a.mu.Lock()
	s.end = time.Now().Add(-time.Second)
	a.mu.Unlock()
	s.mu.Unlock()
	a.prune()
	if s.ctx.Err() != context.Canceled {
		t.Fatal("expiration did not cancel session work")
	}
}

func TestPostWithoutCookieDoesNotCreateExternalSession(t *testing.T) {
	a, _ := testApp(t)
	b := testBrowser{t: t, a: a, token: "external-admin"}
	if code, out, _ := b.post("/api/runs", struct{}{}); code != 403 || out["code"] != "CSRF" {
		t.Fatal(code, out)
	}
	if b.cookie != nil || len(a.sessions) != 0 {
		t.Fatal("rejected POST allocated a session")
	}
}

type loginTestAPI struct {
	*isolatedSub2
	muLogin           sync.Mutex
	rotations, checks int
	verifyErr         error
}

func (m *loginTestAPI) Refresh(ctx context.Context, value string) (LoginResult, error) {
	m.muLogin.Lock()
	defer m.muLogin.Unlock()
	m.rotations++
	if value != "mock-refresh" {
		return LoginResult{}, sessionInvalid
	}
	return m.isolatedSub2.Refresh(ctx, value)
}
func (m *loginTestAPI) VerifyAdmin(_ context.Context, _ string, id int64) error {
	m.muLogin.Lock()
	defer m.muLogin.Unlock()
	m.checks++
	if id != 11 {
		return sessionInvalid
	}
	return m.verifyErr
}

func TestRememberLoginSurvivesIdleRestartWithoutPassword(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STATE_DIRECTORY", filepath.Join(dir, "auth"))
	cfg := settings{Listen: "127.0.0.1:17861", Data: filepath.Join(dir, "runs"), Parallel: 1, Base: "http://127.0.0.1:8080"}
	api := &loginTestAPI{isolatedSub2: newIsolatedSub2()}
	makeApp := func() *app {
		a, err := newApp(cfg, api)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(a.queue.close)
		return a
	}
	b := testBrowser{t: t, a: makeApp()}
	b.call("GET", "/api/session", nil, "")
	code, out, _ := b.post("/api/login", map[string]any{"email": "admin@example.com", "password": "fixture-password", "remember": true})
	if code != 200 || out["remembered"] != true || b.cookie.MaxAge != 7*86400 || !b.cookie.HttpOnly {
		t.Fatal("login did not set seven day cookie", code)
	}
	saved := filepath.Join(b.a.stateDir, cookieKey(b.cookie.Value)+".session")
	read := func() string {
		raw, err := os.ReadFile(saved)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	disk := read()
	if strings.Contains(disk, "fixture-password") || strings.Contains(disk, b.cookie.Value) || strings.Contains(disk, "mock-upstream-login-token") {
		t.Fatal("unexpected saved secret")
	}
	original := *b.cookie
	b.a = makeApp()
	api.verifyErr = sessionTemporary
	if code, _, _ := b.call("GET", "/api/session", nil, ""); code != 503 {
		t.Fatal("temporary outage discarded the login or granted access", code)
	}
	if !strings.Contains(read(), "rotated-refresh") {
		t.Fatal("rotated token lost on profile error")
	}
	api.verifyErr = nil
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		copyBrowser := b
		go func() {
			defer wg.Done()
			code, out, _ := copyBrowser.call("GET", "/api/session", nil, "")
			if code != 200 || out["logged_in"] != true {
				t.Error("restore failed", code)
			}
		}()
	}
	wg.Wait()
	if api.rotations != 1 || api.checks != 2 {
		t.Fatalf("wrong renew or verification count: %d %d", api.rotations, api.checks)
	}
	b.call("GET", "/api/session", nil, "")
	b.post("/api/logout", map[string]any{})
	if _, err := os.Stat(saved); !os.IsNotExist(err) {
		t.Fatal("logout left a private token")
	}
	replay := testBrowser{t: t, a: makeApp(), cookie: &original}
	if code, _, _ := replay.call("GET", "/api/accounts", nil, ""); code != 401 {
		t.Fatal("old cookie survived logout")
	}
}

func TestRememberRevocationAndTemporaryChoice(t *testing.T) {
	a, m := testApp(t)
	b := testBrowser{t: t, a: a}
	b.call("GET", "/api/session", nil, "")
	creds := map[string]any{"email": "admin@example.com", "password": "fixture-password", "remember": false}
	if code, res, _ := b.post("/api/login", creds); code != 200 || res["remembered"] != false || b.cookie.MaxAge != 0 {
		t.Fatal("opt out failed")
	}
	if files, err := os.ReadDir(a.stateDir); err != nil || len(files) != 0 {
		t.Fatal("temporary session persisted")
	}
	b.post("/api/logout", map[string]any{})
	creds["remember"] = true
	b.post("/api/login", creds)
	reloaded, err := newApp(a.cfg, &loginTestAPI{isolatedSub2: m, verifyErr: sessionInvalid})
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.queue.close()
	b.a = reloaded
	if code, res, _ := b.call("GET", "/api/session", nil, ""); code != 200 || res["logged_in"] != false {
		t.Fatal("invalidated role retained access")
	}
	if files, err := os.ReadDir(a.stateDir); err != nil || len(files) != 0 {
		t.Fatal("revoked session persisted")
	}
}

func TestIdleExitWaitsForInflightWork(t *testing.T) {
	activity := newIdleTracker()
	now := time.Now().Add(3 * time.Minute)
	if activity.idle(now, 2*time.Minute, true) || activity.idle(now.Add(time.Minute), 2*time.Minute, false) || !activity.idle(now.Add(2*time.Minute), 2*time.Minute, false) {
		t.Fatal("idle grace did not honor active work")
	}
}
