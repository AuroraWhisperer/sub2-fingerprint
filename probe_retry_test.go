package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestQueueRetries503WhenSwitchingAccounts(t *testing.T) {
	var mu sync.Mutex
	groups := map[int64][]int64{1: {2}, 2: {3}}
	updates := map[int64]int{}
	var transitions []string
	attempts := map[int64]int{}
	successes := map[string]int{}
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/v1/admin/groups/all":
			mockOK(w, []groupData{{ID: 9, Name: "Probe"}})
		case strings.HasPrefix(r.URL.Path, "/api/v1/admin/accounts/"):
			id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/v1/admin/accounts/"), 10, 64)
			if _, ok := groups[id]; !ok {
				http.NotFound(w, r)
				return
			}
			if r.Method == http.MethodPut {
				var payload struct {
					GroupIDs []int64 `json:"group_ids"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if containsGroup(payload.GroupIDs, 9) && containsGroup(groups[3-id], 9) {
					t.Error("accounts overlapped in the Probe group")
				}
				if containsGroup(payload.GroupIDs, 9) {
					transitions = append(transitions, fmt.Sprintf("attach:%d", id))
				} else {
					transitions = append(transitions, fmt.Sprintf("restore:%d", id))
					for _, question := range []string{"q1", "q2", "q3"} {
						if successes[fmt.Sprintf("%d-%s", id, question)] != 1 {
							t.Errorf("account %d released before question %s completed", id, question)
						}
					}
				}
				groups[id] = payload.GroupIDs
				updates[id]++
				mockOK(w, map[string]any{})
				return
			}
			mockOK(w, map[string]any{
				"id": id, "name": fmt.Sprintf("account-%d", id),
				"platform": "openai", "type": "oauth", "status": "active",
				"schedulable": true, "current_concurrency": 0, "concurrency": 3,
				"group_ids": groups[id],
			})
		case r.URL.Path == "/v1/responses":
			var id int64
			for accountID, membership := range groups {
				if containsGroup(membership, 9) {
					id = accountID
				}
			}
			if id == 0 {
				t.Error("model request without Probe membership")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			attempts[id]++
			// Admin reads already show account 2, but the gateway's scheduler
			// still has no available account for its first three requests.
			if id == 2 && attempts[id] <= 3 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			var payload struct {
				Input []struct {
					Content []struct{ Text string }
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || len(payload.Input) != 1 || len(payload.Input[0].Content) != 1 {
				t.Error("invalid model request")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			answer := fmt.Sprintf("%d-%s", id, payload.Input[0].Content[0].Text)
			successes[answer]++
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", answer)
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(panel.Close)
	adapter, err := NewConfiguredAdapter(panel.URL, "probe-key", "Probe")
	if err != nil {
		t.Fatal(err)
	}
	adapter.retryDelay = time.Millisecond
	q, err := openQueue("", "fixture", adapter.Probe)
	if err != nil {
		t.Fatal(err)
	}
	q.probeBatch = adapter.ProbeBatch
	t.Cleanup(q.close)
	req := RunRequest{
		ID: "1eee2222-0000-4000-8000-000000000022", Epoch: "fixture",
		Accounts: []int64{1, 2}, Questions: []string{"q1", "q2", "q3"}, Parallel: 1,
	}
	questions := []Question{{ID: "q1", Prompt: "q1"}, {ID: "q2", Prompt: "q2"}, {ID: "q3", Prompt: "q3"}}
	if err := q.start(context.Background(), 7, fixedJWT("jwt"), req,
		map[int64]AccountView{1: {ID: 1, Name: "first"}, 2: {ID: 2, Name: "second"}}, questions); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { q.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("sequential account run did not finish")
	}
	run, err := q.get(7, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != "done" || len(run.Rows) != 6 {
		t.Fatalf("unexpected run: %+v", run)
	}
	for _, row := range run.Rows {
		want := fmt.Sprintf("%d-%s", row.AccountID, row.QuestionID)
		if row.Status != "done" || row.Text != want {
			t.Errorf("account %d question %s: status=%s text=%q code=%s", row.AccountID, row.QuestionID, row.Status, row.Text, row.Code)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(groups, map[int64][]int64{1: {2}, 2: {3}}) || updates[1] != 2 || updates[2] != 2 {
		t.Errorf("groups were not kept for retries and restored once: groups=%v updates=%v", groups, updates)
	}
	if want := []string{"attach:1", "restore:1", "attach:2", "restore:2"}; !reflect.DeepEqual(transitions, want) {
		t.Errorf("incorrect account lifecycle: got %v, want %v", transitions, want)
	}
	if attempts[1] != 3 || attempts[2] != 6 || len(successes) != 6 {
		t.Errorf("unexpected requests: attempts=%v successes=%v", attempts, successes)
	}
	for answer, count := range successes {
		if count != 1 {
			t.Errorf("successful question %s was sent %d times", answer, count)
		}
	}
}

func TestProbeRetries503WithinOneGroupTransaction(t *testing.T) {
	f := newProbeFixture(t, []int64{2})
	f.gatewayHandler = func(w http.ResponseWriter, _ *http.Request) {
		if got := f.groupsSnapshot(); !reflect.DeepEqual(got, []int64{2, 9}) {
			t.Errorf("Probe membership changed between attempts: %v", got)
		}
		if f.gatewayCalls.Load() < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"42\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")
	}
	answer, err := f.adapter.Probe(context.Background(), fixedJWT("admin-fixture"), 1, "retry me")
	if err != nil || answer.Text != "42" || f.gatewayCalls.Load() != 3 {
		t.Fatalf("unexpected retry result: answer=%+v err=%v attempts=%d", answer, err, f.gatewayCalls.Load())
	}
	if got := f.groupsSnapshot(); !reflect.DeepEqual(got, f.initialGroups) || len(f.updateSnapshot()) != 2 {
		t.Fatalf("retry did not preserve a single group transaction: groups=%v updates=%v", got, f.updateSnapshot())
	}
}

func TestProbeDoesNotRetryOtherFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		code   string
	}{
		{"unauthorized", 401, "private details", "PROBE_KEY_INVALID"},
		{"forbidden", 403, "private details", "PROBE_FORBIDDEN"},
		{"rate limited", 429, "private details", "PROBE_LIMIT"},
		{"server error", 500, "private details", "UPSTREAM_HTTP"},
		{"bad gateway", 502, "private details", "UPSTREAM_HTTP"},
		{"gateway timeout", 504, "private details", "UPSTREAM_HTTP"},
		{"partial stream", 200, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"42\"}\n\n", "UPSTREAM_INCOMPLETE"},
		{"stream error", 200, "data: {\"type\":\"response.failed\"}\n\n", "UPSTREAM_STREAM_ERROR"},
		{"connection closed", 0, "", "NETWORK"},
	} {
		for _, transientFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/after503=%t", tc.name, transientFirst), func(t *testing.T) {
				f := newProbeFixture(t, []int64{2})
				f.gatewayHandler = func(w http.ResponseWriter, _ *http.Request) {
					if transientFirst && f.gatewayCalls.Load() == 1 {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					if tc.status == 0 {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						conn.Close()
						return
					}
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
				}
				_, err := f.adapter.Probe(context.Background(), fixedJWT("admin-fixture"), 1, "do not replay")
				assertGate(t, err, tc.code)
				wantAttempts := int32(1)
				if transientFirst {
					wantAttempts++
				}
				if f.gatewayCalls.Load() != wantAttempts {
					t.Fatalf("non-503 failure was retried: %d attempts", f.gatewayCalls.Load())
				}
				if got := f.groupsSnapshot(); !reflect.DeepEqual(got, f.initialGroups) {
					t.Fatalf("failed probe did not restore groups: %v", got)
				}
			})
		}
	}
}

func TestProbeRetryWaitHonorsContextAndRestoresGroups(t *testing.T) {
	for _, code := range []string{"CANCELED", "TIMEOUT"} {
		t.Run(code, func(t *testing.T) {
			f := newProbeFixture(t, []int64{2})
			f.adapter.retryDelay = time.Hour
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if code == "TIMEOUT" {
				f.adapter.requestTimeout = 200 * time.Millisecond
			}
			f.gatewayHandler = func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
				if code == "CANCELED" {
					time.AfterFunc(50*time.Millisecond, cancel)
				}
			}
			done := make(chan error, 1)
			go func() {
				_, err := f.adapter.Probe(ctx, fixedJWT("admin-fixture"), 1, "stop waiting")
				done <- err
			}()
			select {
			case err := <-done:
				assertGate(t, err, code)
			case <-time.After(2 * time.Second):
				t.Fatal("retry wait ignored context")
			}
			if f.gatewayCalls.Load() != 1 {
				t.Fatalf("unexpected request after context ended: %d", f.gatewayCalls.Load())
			}
			if got := f.groupsSnapshot(); !reflect.DeepEqual(got, f.initialGroups) {
				t.Fatalf("cancelled retry did not restore groups: %v", got)
			}
		})
	}
}
