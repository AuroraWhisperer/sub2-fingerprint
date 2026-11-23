package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestCapacityUsesSub2Counts(t *testing.T) {
	for _, test := range []struct {
		name     string
		used     int
		limit    int
		eligible bool
		canSend  bool
	}{
		{"in use below limit", 1, 20, true, true},
		{"at limit", 20, 20, true, false},
		{"above a newly reduced limit", 21, 20, true, false},
		{"unlimited", 5, 0, true, true},
		{"missing limit", 0, 0, false, false},
		{"missing usage", -1, 20, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newProbeFixture(t, []int64{2})
			f.busy = test.used
			f.limit = test.limit
			f.schedulable = test.eligible
			if test.name == "missing usage" {
				f.busy = -1
			}
			rows, err := f.adapter.List(context.Background(), "admin-fixture")
			if err != nil || len(rows) != 1 || rows[0].Eligible != test.eligible {
				t.Fatalf("list returned %+v, %v", rows, err)
			}
			if test.eligible && (*rows[0].Used != test.used || *rows[0].Limit != test.limit) {
				t.Fatal("capacity display differs from Sub2API", rows[0])
			}
			_, err = f.adapter.Probe(context.Background(), fixedJWT("admin-fixture"), 1, "test")
			switch {
			case test.canSend:
				if err != nil {
					t.Fatalf("eligible account was not probed: %v", err)
				}
			case test.eligible:
				if err != errAccountFull {
					t.Fatalf("expected pre-probe wait, got %v", err)
				}
			default:
				assertGate(t, err, "ACCOUNT_UNAVAILABLE")
			}
			if (f.gatewayCalls.Load() > 0) != test.canSend {
				t.Fatal("unexpected model request", f.gatewayCalls.Load())
			}
		})
	}
}

func TestCapacityWaitYieldsWorkerAndCanCancel(t *testing.T) {
	for _, cancelWhileWaiting := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "cancel"}[cancelWhileWaiting], func(t *testing.T) {
			var free atomic.Bool
			var sent atomic.Int32
			otherDone := make(chan struct{})
			q, _ := openQueue("", "fixture", func(ctx context.Context, _ probeToken, id int64, prompt string) (ProbeAnswer, error) {
				if id == 11 && !free.Load() {
					return ProbeAnswer{}, errAccountFull
				}
				if id == 12 {
					close(otherDone)
				} else {
					sent.Add(1)
				}
				if until, ok := ctx.Deadline(); !ok || time.Until(until) < 118*time.Second {
					t.Error("a capacity wait consumed the model timeout")
				}
				return ProbeAnswer{Text: "answer"}, nil
			})
			t.Cleanup(q.close)
			req := RunRequest{ID: "1eee2222-0000-4000-8000-000000000008", Epoch: "process", Accounts: []int64{11, 12}, Questions: []string{"test"}, Parallel: 1}
			if err := q.start(context.Background(), 1, fixedJWT("fixture"), req, map[int64]AccountView{11: {ID: 11}, 12: {ID: 12}}, []Question{{ID: "test", Prompt: "test"}}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-otherDone:
			case <-time.After(2 * time.Second):
				t.Fatal("a full account prevented a ready account from starting")
			}
			waiting, err := q.get(1, req.ID)
			if err != nil || waiting.Rows[0].Status != "waiting_capacity" || sent.Load() != 0 {
				t.Fatal("full account was sent or not shown as waiting", waiting, err)
			}
			if cancelWhileWaiting {
				if err = q.stopRun(1, req.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				free.Store(true)
			}
			completed := make(chan struct{})
			go func() { q.wg.Wait(); close(completed) }()
			limit := capacityPollInterval + 2*time.Second
			if cancelWhileWaiting {
				limit = time.Second
			}
			select {
			case <-completed:
			case <-time.After(limit):
				t.Fatal("capacity wait did not resolve")
			}
			final, _ := q.get(1, req.ID)
			if cancelWhileWaiting && (sent.Load() != 0 || final.Rows[0].Status != "cancelled") {
				t.Fatal("sent a challenge after cancellation", final)
			}
			if !cancelWhileWaiting && (sent.Load() != 1 || final.Rows[0].Status != "done") {
				t.Fatal("free capacity did not send exactly once", final)
			}
		})
	}
}
