package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Optional integration test with the official standalone binary and a loopback upstream.
// The barrier keeps every response open until all selected accounts are in flight.
func TestInstalledCodexProbe(t *testing.T) {
	binary := os.Getenv("FINGERPRINT_TEST_CODEX_BINARY")
	if binary == "" {
		t.Skip("set FINGERPRINT_TEST_CODEX_BINARY to the official standalone executable")
	}
	count := 1
	if n := os.Getenv("FINGERPRINT_TEST_PARALLEL"); n != "" {
		var err error
		count, err = strconv.Atoi(n)
		if err != nil || count < 1 || count > maxParallelAccounts {
			t.Fatal("invalid fixture parallelism")
		}
	}
	type sample struct{ account, token, question, answer string }
	samples := make([]sample, count)
	for i := range samples {
		account := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		claims := map[string]any{"sub": "test-user", "email": "fixture@example.invalid", "exp": time.Now().Add(time.Hour).Unix(), "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": account, "chatgpt_plan_type": "plus"}}
		b, _ := json.Marshal(claims)
		token := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(b) + ".fixture"
		samples[i] = sample{account, token, fmt.Sprintf("Write the integer %d and nothing else.", 11+i), strconv.Itoa(11 + i)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()
	all := make(chan struct{})
	var lock sync.Mutex
	arrived := map[string]bool{}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model     string
			Reasoning struct{ Effort string }
			Store     *bool
			Tools     []any
			Input     []struct {
				Role    string
				Content []struct{ Type, Text string }
			}
		}
		if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&request) != nil || request.Model != probeModel || request.Reasoning.Effort != "low" || request.Store == nil || *request.Store || len(request.Tools) != 0 {
			t.Error("unexpected model request")
			w.WriteHeader(400)
			return
		}
		var prompts []string
		for _, m := range request.Input {
			if m.Role == "user" {
				for _, p := range m.Content {
					if p.Type == "input_text" {
						prompts = append(prompts, p.Text)
					}
				}
			}
		}
		var selected *sample
		for i := range samples {
			if r.Header.Get("ChatGPT-Account-Id") == samples[i].account {
				selected = &samples[i]
				break
			}
		}
		if selected == nil || r.Header.Get("Authorization") != "Bearer "+selected.token || len(prompts) != 1 || prompts[0] != selected.question || !strings.Contains(r.UserAgent(), "/0.") || r.Header.Get("Originator") == "" {
			t.Error("question, client or account mismatch")
			w.WriteHeader(400)
			return
		}
		lock.Lock()
		if arrived[selected.account] {
			lock.Unlock()
			t.Error("duplicated provider request")
			w.WriteHeader(409)
			return
		}
		arrived[selected.account] = true
		if len(arrived) == count {
			close(all)
		}
		lock.Unlock()
		select {
		case <-ctx.Done():
			w.WriteHeader(408)
			return
		case <-all:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		answer := selected.answer
		item := map[string]any{"type": "message", "id": "msg_fixture", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": answer, "annotations": []any{}}}}
		response := map[string]any{"id": "resp_fixture", "object": "response", "created_at": 1, "model": probeModel, "status": "completed", "output": []any{item}, "usage": map[string]int{"input_tokens": 8, "output_tokens": 1, "total_tokens": 9}}
		event := func(kind string, fields map[string]any) {
			fields["type"] = kind
			body, _ := json.Marshal(fields)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, body)
		}
		event("response.created", map[string]any{"response": map[string]string{"id": "resp_fixture", "status": "in_progress"}})
		event("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"type": "message", "id": "msg_fixture", "role": "assistant", "status": "in_progress", "content": []any{}}})
		event("response.output_text.delta", map[string]any{"item_id": "msg_fixture", "output_index": 0, "content_index": 0, "delta": answer})
		event("response.output_item.done", map[string]any{"output_index": 0, "item": item})
		event("response.completed", map[string]any{"response": response})
	}))
	defer upstream.Close()
	a, _ := NewAdapter("http://127.0.0.1:8080")
	a.codexBinary = binary
	a.upstream = upstream.URL
	a.upstreamTransport = upstream.Client().Transport.(*http.Transport)
	var wg sync.WaitGroup
	for _, v := range samples {
		wg.Add(1)
		go func(v sample) {
			defer wg.Done()
			h := make(http.Header)
			h.Set("Authorization", "Bearer "+v.token)
			h.Set("ChatGPT-Account-Id", v.account)
			got, err := a.probeWithCodex(ctx, h, nil, v.question)
			if err != nil || got.Text != v.answer || got.Model != probeModel || got.RequestID == "" {
				t.Errorf("fixture question %q: result mismatch (%v)", v.question, err)
				cancel()
			}
		}(v)
	}
	wg.Wait()
}
