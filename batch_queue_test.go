package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestQueueRunsOneAccountsQuestionsConcurrentlyWithBatchProbe(t *testing.T) {
	q, err := openQueue("", "fixture", func(context.Context, probeToken, int64, string) (ProbeAnswer, error) {
		t.Fatal("queue used the single-question probe path")
		return ProbeAnswer{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)

	release := make(chan struct{})
	threeStarted := make(chan struct{})
	var once sync.Once
	var active atomic.Int32
	var peak atomic.Int32
	q.probeBatch = func(ctx context.Context, _ probeToken, _ int64, prompts []string, report probeBatchReporter) error {
		var workers sync.WaitGroup
		for index, prompt := range prompts {
			workers.Add(1)
			go func(index int, prompt string) {
				defer workers.Done()
				current := active.Add(1)
				for {
					previous := peak.Load()
					if current <= previous || peak.CompareAndSwap(previous, current) {
						break
					}
				}
				if current == 3 {
					once.Do(func() { close(threeStarted) })
				}
				select {
				case <-ctx.Done():
					report(index, ProbeAnswer{}, ctx.Err())
				case <-release:
					report(index, ProbeAnswer{Text: fmt.Sprintf("answer-%s", prompt)}, nil)
				}
				active.Add(-1)
			}(index, prompt)
		}
		workers.Wait()
		return nil
	}

	req := RunRequest{
		ID:       "1eee2222-0000-4000-8000-000000000021",
		Epoch:    "fixture",
		Accounts: []int64{11},
		Questions: []string{
			"q1",
			"q2",
			"q3",
		},
		Parallel: 1,
	}
	questions := []Question{
		{ID: "q1", Prompt: "first"},
		{ID: "q2", Prompt: "second"},
		{ID: "q3", Prompt: "third"},
	}
	if err := q.start(context.Background(), 7, fixedJWT("jwt"), req, map[int64]AccountView{
		11: {ID: 11, Name: "fixture-account"},
	}, questions); err != nil {
		t.Fatal(err)
	}
	select {
	case <-threeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("batch probe did not start three requests")
	}
	close(release)
	q.wg.Wait()

	run, err := q.get(7, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != "done" || peak.Load() != 3 {
		t.Fatalf("unexpected batch run state=%s peak=%d", run.State, peak.Load())
	}
	for index, row := range run.Rows {
		if row.Status != "done" || row.Text == "" {
			t.Fatalf("row %d was not completed: %+v", index, row)
		}
		if row.QuestionID != questions[index].ID {
			t.Fatalf("row %d changed order: %+v", index, row)
		}
	}
}

func TestProbeBatchTimeoutCoversCapacityLimitedWaves(t *testing.T) {
	const requestTimeout = 25 * time.Millisecond
	for _, tc := range []struct {
		name        string
		concurrency int
		waves       int
	}{
		{name: "one available slot", concurrency: 1, waves: 3},
		{name: "two available slots", concurrency: 2, waves: 2},
		{name: "three available slots", concurrency: 3, waves: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := probeBatchTimeout(requestTimeout, 3, tc.concurrency)
			want := time.Duration(tc.waves)*requestTimeout + probeBatchAdminCalls*adminTimeout
			if got != want {
				t.Fatalf("batch timeout=%s, want %s", got, want)
			}
		})
	}

	if got, want := maxProbeBatchTimeout(3), probeBatchTimeout(probeTimeout, 3, 1); got != want {
		t.Fatalf("queue batch timeout=%s, want serial upper bound %s", got, want)
	}
}

func TestBatchReportsRecoveryFailureAndSkipsOtherAccounts(t *testing.T) {
	q, err := openQueue("", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	q.probeBatch = func(_ context.Context, _ probeToken, account int64, prompts []string, report probeBatchReporter) error {
		if account != 11 {
			t.Errorf("attempted account %d after the recovery failure", account)
		}
		for i := range prompts {
			report(i, ProbeAnswer{Text: "42"}, nil)
		}
		return gate("GROUP_RESTORE_FAILED", "fixture restore failed")
	}
	req := RunRequest{ID: "11112222-0000-4000-8000-000000000062", Accounts: []int64{11, 22}, Parallel: 1}
	qs := []Question{{ID: "q1", Prompt: "first"}, {ID: "q2", Prompt: "second"}}
	if err := q.start(context.Background(), 7, fixedJWT("fixture"), req, map[int64]AccountView{11: {}, 22: {}}, qs); err != nil {
		t.Fatal(err)
	}
	run := waitRun(t, q, 7, req.ID)
	if run.Failure == nil || run.Failure.AccountID != 11 || run.Failure.Code != "GROUP_RESTORE_FAILED" {
		t.Fatalf("missing batch recovery failure: %+v", run)
	}
	for i, row := range run.Rows {
		if i < len(qs) {
			if row.Status != "done" || row.Text != "42" {
				t.Errorf("recovery failure overwrote answer: %+v", row)
			}
		} else if row.Status != "failed" || row.Started != "" {
			t.Errorf("later account was not skipped: %+v", row)
		}
	}
}

func TestRecoveryFailureTakesPrecedenceOverProbeDeadline(t *testing.T) {
	restoreFailure := gate("GROUP_RESTORE_FAILED", "fixture restore failed")
	q := queue{
		probe: func(context.Context, probeToken, int64, string) (ProbeAnswer, error) {
			return ProbeAnswer{}, restoreFailure
		},
		probeBatch: func(context.Context, probeToken, int64, []string, probeBatchReporter) error {
			return restoreFailure
		},
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := q.probeWhenReady(ctx, fixedJWT("fixture"), &Row{}, Question{}, nil)
	assertGate(t, err, "GROUP_RESTORE_FAILED")
	assertGate(t, q.probeBatchWhenReady(ctx, fixedJWT("fixture"), nil, 0, 1, []string{"first", "second"}, nil, nil), "GROUP_RESTORE_FAILED")
}
