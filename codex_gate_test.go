package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func fixtureCodexRequest(ctx context.Context, endpoint, token, accountID, prompt string) (*http.Request, error) {
	wire := map[string]any{
		"model": probeModel, "reasoning": map[string]string{"effort": "low"}, "tools": []any{}, "tool_choice": "auto", "include": []string{"reasoning.encrypted_content"},
		"store": false, "stream": true, "input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]string{"type": "input_text", "text": prompt}}}},
	}
	b, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/responses", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	bits := randomID()
	id := fmt.Sprintf("%s-%s-7%s-8%s-%s", bits[:8], bits[8:12], bits[12:15], bits[15:18], bits[18:30])
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("ChatGPT-Account-Id", accountID)
	r.Header.Set("X-Client-Request-Id", id)
	r.Header.Set("Session-Id", id)
	r.Header.Set("Thread-Id", id)
	r.Header.Set("Accept", "text/event-stream")
	r.Header.Set("User-Agent", codexProviderName+"/0.0.0 (offline fixture)")
	r.Header.Set("Originator", codexProviderName)
	return r, nil
}

func fixtureCodexTurn(ctx context.Context, binary, endpoint, token, accountID, prompt string) error {
	r, err := fixtureCodexRequest(ctx, endpoint, token, accountID, prompt)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return errors.New("fixture rejected")
	}
	return nil
}

func TestCodexOnlyForwardsOriginalCleanTurn(t *testing.T) {
	const user = `{"type":"message","role":"user","content":[{"type":"input_text","text":"42?"}]}`
	const setup = `{"type":"message","role":"developer","content":[{"type":"input_text","text":"` + codexQuestionInstructions + `"}]}`
	const defs = `{"type":"additional_tools","role":"developer","tools":[{"name":"local-tool"}]}`
	const env = `{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\nlocal-path\n</environment_context>"}]}`
	for _, tc := range []struct {
		name, inputs, modifications string
		valid                       bool
	}{
		{"plain", user, "", true}, {"official client instructions", strings.Join([]string{setup, setup, user}, ","), "", true},
		{"unexpected developer history", strings.Replace(setup, codexQuestionInstructions, "private history", 1) + "," + user, "", false},
		{"environment context", env + "," + user, "", false}, {"nested tool declarations", defs + "," + user, "", false},
		{"saved prompt", user, `,"prompt":{"id":"other"}`, false}, {"background request", user, `,"background":true`, false},
		{"another user", user + "," + user, "", false}, {"continuation", user, `,"previous_response_id":"other"`, false},
		{"assistant history", `{"type":"message","role":"assistant"},` + user, "", false},
		{"tool result", `{"type":"function_call_output","call_id":"other"},` + user, "", false},
		{"tool choice", user, `,"tool_choice":{"type":"function","name":"other"}`, false},
		{"exposed tools", user, `,"tools":[{"type":"function","name":"other"}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(`{"model":"gpt-6-astra","store":false,"stream":true,"reasoning":{"effort":"low"},"input":[` + tc.inputs + `]` + tc.modifications + `}`)
			out, err := codexQuestionBody(raw, "42?")
			if (err == nil) != tc.valid {
				t.Fatalf("unexpected validation: %v", err)
			}
			if err == nil && !bytes.Equal(out, raw) {
				t.Fatal("changed genuine client request bytes")
			}
		})
	}
}

func TestCodexKeepsNativeRequestHeaders(t *testing.T) {
	r, err := fixtureCodexRequest(context.Background(), "http://127.0.0.1:1/v1", "synthetic", "account", "question")
	if err != nil {
		t.Fatal(err)
	}
	account := make(http.Header)
	for _, key := range []string{"Authorization", "ChatGPT-Account-Id"} {
		account.Set(key, r.Header.Get(key))
	}
	r.Header.Set("X-Codex-Turn-Metadata", `{"request_kind":"turn"}`)
	h, id, err := codexOutboundHeaders(r.Header, account)
	if err != nil || id != r.Header.Get("X-Client-Request-Id") || h.Get("Session_id") != "" || h.Get("Version") != "" {
		t.Fatal("native request identity was missing or manufactured", err)
	}
	for key := range r.Header {
		if h.Get(key) != r.Header.Get(key) {
			t.Errorf("changed official header %s", key)
		}
	}
	r.Header.Set("Session_id", id)
	h, _, err = codexOutboundHeaders(r.Header, account)
	if err != nil || h.Get("Session_id") != id {
		t.Fatal("dropped an explicit client header", err)
	}
	r.Header.Add("Session-Id", id)
	_, _, err = codexOutboundHeaders(r.Header, account)
	assertGate(t, err, "CODEX_PROTOCOL")
}

func TestCodexSSERejectsToolsAndReroutes(t *testing.T) {
	complete := `data: {"type":"response.completed","response":{"status":"completed","model":"gpt-6-astra","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"42"}]}]}}` + "\r\n\r\n"
	for _, tc := range []struct{ name, early, code string }{
		{"tool item", `{"type":"response.output_item.added","item":{"type":"custom_tool_call"}}`, "UPSTREAM_REFUSAL"},
		{"alternate model", `{"type":"response.created","response":{"model":"other"}}`, "UPSTREAM_MODEL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b, err := codexVerifiedStream(strings.NewReader("data: " + tc.early + "\r\n\r\n" + complete))
			assertGate(t, err, tc.code)
			if len(b) != 0 || a.Text != "" {
				t.Fatal("unsafe response escaped")
			}
		})
	}
	a, b, err := codexVerifiedStream(strings.NewReader(complete + "data: private-trailing-frame\r\n\r\n"))
	if err != nil || a.Text != "42" || string(b) != complete {
		t.Fatal("terminal SSE was not preserved exactly", err)
	}
}

func TestCodexLocalGateOnlyOneUpstream(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Version") != "" || r.Header.Get("Originator") != codexProviderName || r.UserAgent() != codexProviderName+"/0.0.0 (offline fixture)" {
			t.Error("client identity was altered")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"status":"completed","model":"gpt-6-astra","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"42"}]}]}}`+"\n\n")
	}))
	defer up.Close()
	a, _ := NewAdapter("http://127.0.0.1:1")
	a.upstream = up.URL
	a.upstreamTransport = up.Client().Transport.(*http.Transport)
	h := make(http.Header)
	h.Set("Authorization", "Bearer fixture-token")
	h.Set("ChatGPT-Account-Id", "fixture-account")
	a.runCodex = func(ctx context.Context, binary, endpoint, token, accountID, prompt string) error {
		for _, sub := range []string{"/models", "/responses?extra=1"} {
			r, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+sub, nil)
			res, err := http.DefaultClient.Do(r)
			if err != nil {
				return err
			}
			res.Body.Close()
			if res.StatusCode != 404 {
				t.Errorf("unexpected metadata status %d", res.StatusCode)
			}
		}
		if err := fixtureCodexTurn(ctx, binary, endpoint, token, accountID, prompt); err != nil {
			return err
		}
		_ = fixtureCodexTurn(ctx, binary, endpoint, token, accountID, prompt)
		return nil
	}
	got, err := a.probeWithCodex(context.Background(), h, nil, "42?")
	assertGate(t, err, "CODEX_CONTINUATION")
	if calls.Load() != 1 || got.Text != "" {
		t.Fatal("repeated challenge was sent or scored")
	}
	a.runCodex = fixtureCodexTurn
	h.Set("User-Agent", "custom/1.2.3")
	_, err = a.probeWithCodex(context.Background(), h, nil, "42?")
	assertGate(t, err, "IDENTITY")
	if calls.Load() != 1 {
		t.Fatal("overrode account identity")
	}
}
