package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func updateFixture(t *testing.T) (*app, *testBrowser, *isolatedSub2) {
	t.Helper()
	a, model := testApp(t)
	root := t.TempDir()
	a.updater.requestPath = filepath.Join(root, "update.request")
	a.updater.statusPath = filepath.Join(root, "status.json")
	a.updater.unitPath = filepath.Join(root, "update.path")
	if err := os.WriteFile(a.updater.unitPath, []byte("fixture unit"), 0600); err != nil {
		t.Fatal(err)
	}
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v99.0.0","draft":false,"prerelease":false}`)
	}))
	t.Cleanup(panel.Close)
	a.updater.latestURL = panel.URL
	b := &testBrowser{t: t, a: a}
	b.call("GET", "/api/session", nil, "")
	if status, _, body := b.post("/api/login", map[string]string{"email": "admin@example.com", "password": "fixture-password"}); status != 200 {
		t.Fatal(status, body)
	}
	return a, b, model
}

func TestUpdateEndpointsRequireAdminAndCSRF(t *testing.T) {
	a, b, _ := updateFixture(t)
	anonymous := &testBrowser{t: t, a: a}
	if code, _, _ := anonymous.call("GET", "/api/update", nil, ""); code != 401 {
		t.Fatalf("anonymous update status: %d", code)
	}
	for _, path := range []string{"/api/update/check", "/api/update/install"} {
		if code, _, _ := b.call("POST", path, map[string]string{}, "https://other.invalid"); code != 403 {
			t.Fatalf("accepted foreign origin for %s: %d", path, code)
		}
		if code, _, _ := b.post(path, map[string]string{"command": "arbitrary"}); code != 400 {
			t.Fatalf("accepted parameters for %s: %d", path, code)
		}
		if code, _, _ := b.call("GET", path, nil, ""); code != 405 {
			t.Fatalf("accepted GET for %s: %d", path, code)
		}
	}
	if _, err := os.Stat(a.updater.requestPath); !os.IsNotExist(err) {
		t.Fatal("rejected request created update trigger")
	}
}

func TestSlowUpdateCheckDoesNotBlockSameSessionStatus(t *testing.T) {
	a, b, _ := updateFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		fmt.Fprint(w, `{"tag_name":"v99.0.0"}`)
	}))
	t.Cleanup(panel.Close)
	a.updater.latestURL = panel.URL
	checking := *b
	checked := make(chan struct{})
	go func() {
		defer close(checked)
		checking.post("/api/update/check", struct{}{})
	}()
	defer func() {
		close(release)
		<-checked
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("release check did not reach the fixture")
	}

	status := make(chan int, 1)
	go func() {
		code, _, _ := b.call("GET", "/api/update", nil, "")
		status <- code
	}()
	select {
	case code := <-status:
		if code != http.StatusOK {
			t.Fatal(code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("release check blocked the same session's status request")
	}
}

func TestUpdateRequestBlocksDetectionAndDeduplicates(t *testing.T) {
	a, b, _ := updateFixture(t)
	if code, body, _ := b.post("/api/update/check", struct{}{}); code != 200 || body["latest"] != "v99.0.0" || body["available"] != true {
		t.Fatal(code, body)
	}
	for range 2 {
		if code, body, _ := b.post("/api/update/install", struct{}{}); code != 202 || body["state"] != "queued" {
			t.Fatal(code, body)
		}
	}
	if data, err := os.ReadFile(a.updater.requestPath); err != nil || len(data) != 0 {
		t.Fatal("update request must be an empty file", err)
	}
	if code, body, _ := b.post("/api/runs", struct{}{}); code != 409 || body["code"] != "UPDATE_ACTIVE" {
		t.Fatal("update did not block detection", code, body)
	}
	if err := os.WriteFile(a.updater.statusPath, []byte(`{"state":"running"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(a.updater.requestPath); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := b.post("/api/runs", struct{}{}); code != 409 || body["code"] != "UPDATE_ACTIVE" {
		t.Fatal("running update did not block detection", code, body)
	}
	if err := os.WriteFile(a.updater.statusPath, []byte(`{"state":"failed","private":"secret"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := b.call("GET", "/api/update", nil, ""); code != 200 || body["state"] != "failed" || body["private"] != nil {
		t.Fatal(code, body)
	}
	if code, body, _ := b.post("/api/runs", struct{}{}); code != 409 || body["code"] == "UPDATE_ACTIVE" {
		t.Fatal("finished update still blocks detection", code, body)
	}
}

func TestUpdateRefusesDuringAccountDetection(t *testing.T) {
	a, b, model := updateFixture(t)
	qs := a.groups[0][:1]
	req := RunRequest{ID: "1eee2222-0000-4000-8000-000000000026", Epoch: b.epoch, Accounts: []int64{101}, Questions: []string{qs[0].ID}, Parallel: 1}
	if code, _, body := b.post("/api/runs", req); code != 202 {
		t.Fatal(code, body)
	}
	waitStart(t, model)
	if code, body, _ := b.post("/api/update/install", struct{}{}); code != 409 || body["code"] != "DETECTION_ACTIVE" {
		t.Fatal(code, body)
	}
	if _, err := os.Stat(a.updater.requestPath); !os.IsNotExist(err) {
		t.Fatal("active detection created update trigger")
	}
	close(model.release)
	waitRun(t, a.queue, 11, req.ID)
}

func TestUpdateRetryAfterServerRestartRenewsExternalSession(t *testing.T) {
	_, b, _ := updateFixture(t)
	b.token = "fixture-external-admin"
	b.call("GET", "/api/session", nil, "")
	oldCSRF, oldEpoch := b.csrf, b.epoch
	if code, _, _ := b.post("/api/update/install", struct{}{}); code != 202 {
		t.Fatal("initial install", code)
	}
	// Rollback starts a new application with no prior in-memory page sessions.
	restarted, _, _ := updateFixture(t)
	b.a = restarted
	if err := os.WriteFile(restarted.updater.statusPath, []byte(`{"state":"failed"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := b.call("GET", "/api/update", nil, ""); code != 200 || body["state"] != "failed" {
		t.Fatal(code, body)
	}
	if code, body, _ := b.post("/api/update/install", struct{}{}); code != 403 || body["code"] != "CSRF" {
		t.Fatal("stale CSRF was accepted", code, body)
	}
	if code, _, _ := b.call("GET", "/api/session", nil, ""); code != 200 || b.csrf == oldCSRF || b.epoch == oldEpoch {
		t.Fatal("session did not renew after restart", code)
	}
	if code, body, _ := b.post("/api/update/install", struct{}{}); code != 202 || body["state"] != "queued" {
		t.Fatal("retry after session renewal", code, body)
	}
}

func TestUpdateUnavailableAndUnresponsiveInstaller(t *testing.T) {
	a, b, _ := updateFixture(t)
	if err := os.Remove(a.updater.unitPath); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := b.post("/api/update/install", struct{}{}); code != 409 || body["code"] != "UPDATE_UNSUPPORTED" {
		t.Fatal(code, body)
	}
	if err := os.WriteFile(a.updater.requestPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Minute)
	if err := os.Chtimes(a.updater.requestPath, old, old); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := b.call("GET", "/api/update", nil, ""); code != 200 || body["state"] != "failed" {
		t.Fatal(code, body)
	}
	if _, err := os.Stat(a.updater.requestPath); !os.IsNotExist(err) {
		t.Fatal("unanswered trigger was not released")
	}
}

func TestUpdateReleaseValidationAndCache(t *testing.T) {
	for _, body := range []string{`{"tag_name":"v99.0.0","draft":true}`, `{"tag_name":"v99.0.0","prerelease":true}`, `{"tag_name":"not-a-version"}`, `{}`} {
		t.Run(body, func(t *testing.T) {
			panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer panel.Close()
			u := newUpdater()
			u.latestURL = panel.URL
			if err := u.check(context.Background()); err == nil {
				t.Fatal("invalid release accepted")
			}
		})
	}
	var calls atomic.Int32
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"tag_name":"v99.0.0"}`)
	}))
	defer panel.Close()
	u := newUpdater()
	u.latestURL = panel.URL
	for range 2 {
		if err := u.check(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("release checks were not cached")
	}
	if !newerVersion("v0.10.0", "v0.9.9") || newerVersion("v0.2.2", "v0.3.0") || newerVersion("v1.0.0", "v1.0.0") {
		t.Fatal("incorrect version comparison")
	}
}
