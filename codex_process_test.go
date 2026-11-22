package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This optional offline test exercises the actual Codex binary and never connects to an upstream provider.
func TestOfficialCodexLoopback(t *testing.T) {
	binary := os.Getenv("FINGERPRINT_TEST_CODEX_BINARY")
	if binary == "" {
		t.Skip("set FINGERPRINT_TEST_CODEX_BINARY to the official executable")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("expected absolute path to the client")
	}
	accountID := "00000000-0000-4000-8000-000000000029"
	claims, _ := json.Marshal(map[string]any{"email": "synthetic@example.invalid", "exp": time.Now().Add(time.Hour).Unix(), "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": accountID}})
	token := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(claims) + ".unsigned"
	prompt := "Return the integer equal to nineteen plus twenty-three."
	for _, mode := range []string{"complete", "refused", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			var count atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/fixture-nonce/v1/responses" || r.Method != "POST" {
					http.NotFound(w, r)
					return
				}
				count.Add(1)
				var payload struct {
					Model     string
					Reasoning struct{ Effort string }
					Input     []struct {
						Type, Role string
						Content    []struct{ Type, Text string }
					}
					Store              *bool
					Tools              []json.RawMessage
					PreviousResponseID *string `json:"previous_response_id"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("invalid model payload: %v", err)
				}
				if payload.Model != probeModel || payload.Reasoning.Effort != "low" || payload.Store == nil || *payload.Store || len(payload.Tools) != 0 || payload.PreviousResponseID != nil || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("ChatGPT-Account-Id") != accountID || r.Header.Get("Originator") != codexProviderName || !strings.HasPrefix(r.Header.Get("User-Agent"), codexProviderName+"/") {
					t.Error("model request did not match the selected account and settings")
				}
				if len(payload.Input) != 2 {
					t.Errorf("expected only the developer instruction and the question; found %d input items", len(payload.Input))
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				for i, role := range []string{"developer", "user"} {
					message := payload.Input[i]
					want := []string{codexQuestionInstructions, prompt}[i]
					if message.Type != "message" || message.Role != role || len(message.Content) != 1 || message.Content[0].Type != "input_text" || message.Content[0].Text != want {
						t.Errorf("unexpected %s message in the official client's model request", role)
					}
				}
				switch mode {
				case "cancel":
					cancel()
					return
				case "refused":
					w.WriteHeader(400)
					io.WriteString(w, `{"error":{"message":"FIXTURE_PRIVATE_ERROR"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				item := `{"type":"message","role":"assistant","id":"msg_fixture","status":"completed","content":[{"type":"output_text","text":"42","annotations":[]}]}`
				fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"in_progress\"}}\n\n")
				fmt.Fprintf(w, "data: {\"type\":\"response.output_item.added\",\"item\":%s}\n\n", item)
				fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"item\":%s}\n\n", item)
				fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"model\":%q,\"status\":\"completed\",\"output\":[%s]}}\n\n", probeModel, item)
			}))
			defer server.Close()
			err := runCodexProcess(ctx, binary, server.URL+"/fixture-nonce/v1", token, accountID, prompt)
			if count.Load() != 1 {
				t.Errorf("expected one dispatch, got %d", count.Load())
			}
			if mode == "complete" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || strings.Contains(err.Error(), "FIXTURE_PRIVATE_ERROR") {
				t.Fatal("returned a private error or accepted failure")
			}
			if mode == "cancel" {
				assertGate(t, err, "CANCELED")
			}
		})
	}
}

func TestCodexRPCRejectsUnrelatedOrIncompleteTurn(t *testing.T) {
	const prompt = "unique current challenge"
	for _, test := range []struct{ name, method, params, code string }{
		{"question differs", "item/completed", `{"threadId":"fresh","turnId":"one","item":{"type":"userMessage","content":[{"type":"text","text":"different"}]}}`, "CODEX_PROTOCOL"},
		{"another thread", "item/completed", `{"threadId":"different","turnId":"one","item":{"type":"agentMessage","text":"42"}}`, "CODEX_PROTOCOL"},
		{"another turn", "item/completed", `{"threadId":"fresh","turnId":"different","item":{"type":"agentMessage","text":"42"}}`, "CODEX_PROTOCOL"},
		{"model rerouted", "model/rerouted", `{}`, "UPSTREAM_MODEL"},
		{"tool action", "item/started", `{"threadId":"fresh","turnId":"one","item":{"type":"commandExecution"}}`, "UPSTREAM_REFUSAL"},
		{"missing answer", "turn/completed", `{"threadId":"fresh","turn":{"id":"one","status":"completed"}}`, "CODEX_PROTOCOL"},
		{"failed turn", "turn/completed", `{"threadId":"fresh","turn":{"id":"one","status":"failed"}}`, "CODEX_TURN_FAILED"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := codexProbeRPC{thread: "fresh", turn: "one", prompt: prompt}
			m := codexMessage{Method: test.method}
			if err := json.Unmarshal([]byte(test.params), &m.Params); err != nil {
				t.Fatal(err)
			}
			assertGate(t, p.notification(&m), test.code)
		})
	}
	p := codexProbeRPC{}
	assertGate(t, p.notification(&codexMessage{Method: "external/operation", ID: json.RawMessage("5")}), "CODEX_CLIENT_ACTION")
}

func TestCodexTmpfsUsesNearestMount(t *testing.T) {
	mounts := "9 0 0:9 / / rw - ext4 disk rw\n10 9 0:10 / /run rw - tmpfs tmpfs rw\n11 10 0:11 / /run/disk-dir rw - ext4 disk rw\n"
	if !codexTmpfsContains(mounts, "/run/sub2-fingerprint") || codexTmpfsContains(mounts, "/run/disk-dir/scratch") || codexTmpfsContains(mounts, "/runtime/scratch") {
		t.Fatal("did not distinguish tmpfs from disk-backed directories")
	}
}
