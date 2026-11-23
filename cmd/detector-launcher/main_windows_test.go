//go:build windows

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLauncherChecksHomeAndDoesNotFollowRedirects(t *testing.T) {
	health := `{"status":"ok","version":"test"}`
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte(health))
			return
		}
		w.Write([]byte("page"))
	}))
	defer healthy.Close()
	if err := checkPage(healthy.URL + "/"); err != nil {
		t.Fatal(err)
	}
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("followed a redirect outside the detector") }))
	defer other.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte(health))
			return
		}
		http.Redirect(w, r, other.URL, 302)
	}))
	defer redirect.Close()
	if err := checkPage(redirect.URL + "/"); err == nil {
		t.Fatal("accepted redirected home page")
	}
}

func TestDetectorURL(t *testing.T) {
	got, err := detectorURL("https://example.test/modeltrace")
	if err != nil || got != "https://example.test/modeltrace/" {
		t.Fatalf("detector URL = %q, %v", got, err)
	}
	for _, raw := range []string{"example.test/path", "file:///tmp/path", "https://user@example.test/", "https://example.test/path?next=/", "https://example.test/path#hash"} {
		if _, err := detectorURL(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}
