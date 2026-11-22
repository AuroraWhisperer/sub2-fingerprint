package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbePreservesReasonsForUpstreamFailure(t *testing.T) {
	adminFixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/admin/accounts/data" {
			mockOK(w, map[string]any{"accounts": []any{map[string]any{"name": "fixture", "platform": "openai", "type": "oauth", "credentials": map[string]any{"access_token": "fixture-token", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339), "chatgpt_account_id": "fixture-id"}}}})
		} else {
			mockOK(w, map[string]any{"id": 8, "name": "fixture", "platform": "openai", "type": "oauth", "status": "active", "schedulable": true, "concurrency": 20, "current_concurrency": 0})
		}
	}))
	defer adminFixture.Close()
	const sensitive = "fixture-private-input"
	const requestRef = "req_a00112233445566778899aabbccddeeff"
	cases := []struct {
		name, body, code string
		stream           bool
	}{
		{"model", `{"error":{"code":"model_not_found","message":"fixture-private-input"}}`, "UPSTREAM_MODEL", false},
		{"reasoning", `{"error":{"code":"unsupported_value","param":"reasoning.effort","message":"fixture-private-input"}}`, "UPSTREAM_REASONING", false},
		{"detail string", `{"detail":"The 'gpt-6-astra' model is not supported when using this account. fixture-private-input"}`, "UPSTREAM_MODEL", false},
		{"client", `{"detail":{"code":"unsupported_client_version","message":"fixture-private-input"}}`, "UPSTREAM_CLIENT", false},
		{"nested slow down", `{"response":{"error":{"code":"slow_down","message":"fixture-private-input"}}}`, "UPSTREAM_OVERLOAD", false},
		{"capacity message", `{"message":"The selected model is at capacity. fixture-private-input"}`, "UPSTREAM_OVERLOAD", false},
		{"capacity in echoed input", `{"input":"The selected model is at capacity. fixture-private-input"}`, "UPSTREAM_BAD_REQUEST", false},
		{"parameter", `{"error":{"code":"invalid_request_error","param":"input","message":"fixture-private-input"}}`, "UPSTREAM_PARAMETER", false},
		{"opaque detail", `{"error":{"code":"fixture-private-input","param":"fixture-private-input","message":"fixture-private-input"}}`, "UPSTREAM_BAD_REQUEST", false},
		{"non-json", `<html>fixture-private-input</html>`, "UPSTREAM_BAD_REQUEST", false},
		{"oversize", `{"error":{"code":"model_not_found","message":"` + strings.Repeat("x", 16<<10) + `"}}`, "UPSTREAM_BAD_REQUEST", false},
		{"stream response failed", "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"model_not_found\",\"message\":\"fixture-private-input\"}}}\n\n", "UPSTREAM_MODEL", true},
		{"stream top-level error", "data: {\"type\":\"error\",\"param\":\"reasoning.effort\",\"message\":\"fixture-private-input\"}\n\n", "UPSTREAM_REASONING", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var count atomic.Int32
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count.Add(1)
				var body struct {
					Model     string
					Reasoning struct{ Effort string }
					Include   []string
					Store     *bool
					Stream    bool
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != probeModel || body.Reasoning.Effort != "low" || !body.Stream || body.Store == nil || *body.Store || len(body.Include) != 1 || body.Include[0] != "reasoning.encrypted_content" {
					t.Error("unexpected body sent to Responses")
				}
				if tc.name == "model" {
					w.Header().Set("X-Request-Id", requestRef)
				} else {
					w.Header().Set("X-Request-Id", sensitive)
				}
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.WriteHeader(http.StatusBadRequest)
				}
				io.WriteString(w, tc.body)
			}))
			defer upstream.Close()
			adapter, err := NewAdapter(adminFixture.URL)
			if err != nil {
				t.Fatal(err)
			}
			adapter.upstream, adapter.upstreamTransport = upstream.URL, upstream.Client().Transport.(*http.Transport)
			answer, err := adapter.Probe(context.Background(), "fixture", 8, "fixture challenge")
			assertGate(t, err, tc.code)
			if strings.Contains(err.Error(), requestRef) != (tc.name == "model") {
				t.Error("lost or fabricated the upstream request ID")
			}
			if count.Load() != 1 || answer.Text != "" || strings.Contains(err.Error(), sensitive) || (tc.stream == strings.Contains(err.Error(), "HTTP 400")) {
				t.Fatal("failed request retried, scored, leaked private detail or lost failure stage")
			}
		})
	}
}
