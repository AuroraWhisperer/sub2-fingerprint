# Batch Probe Timeout Implementation Plan

> **For agentic workers:** Execute this plan task-by-task in the current session. Keep each change independently testable.

**Goal:** Let all selected questions in one account finish within their per-question request budgets while keeping the Probe group attached until the whole batch has completed.

**Architecture:** Keep account workers serial and keep one group transaction around each account batch. Give each model request its own bounded context, and give the surrounding batch transaction enough time for the selected requests to run in capacity-limited waves plus group setup and cleanup. Preserve cancellation so an operator stop still cancels every in-flight request and runs the existing recovery path.

**Tech Stack:** Go standard library, `context`, existing queue and adapter test fixtures.

## Global Constraints

- Different accounts remain serial because `maxParallelAccounts` stays `1`.
- One account's selected questions remain in one Probe group transaction.
- The Probe membership verification must finish before any model request is sent.
- The original account group IDs must be restored after every temporary attachment.
- Each model request keeps the existing request timeout and fixed `gpt-6-astra` with `reasoning.effort=low`.
- A cancellation must wait for the batch worker goroutines before the group transaction returns.

### Task 1: Separate batch and request timeouts

**Files:**
- Modify: `adapter.go`
- Modify: `queue.go`
- Modify: `group_transaction_test.go`
- Modify: `batch_queue_test.go`

**Interfaces:**
- `Adapter.ProbeBatch` continues to report one result per prompt and returns only after all request workers have exited.
- `queue.probeBatchWhenReady` continues to retry capacity-full responses and reports capacity wait separately from model request timeout.

- [ ] Add a batch timeout helper based on the number of selected prompts and the maximum request waves, while preserving the existing single-question timeout.
- [ ] Derive an independent request context for each `gatewayProbe` call so a later wave receives a full request budget.
- [ ] Keep the parent batch context in charge of cancellation and group restoration.
- [ ] Add tests for three prompts with one available slot and two available slots, asserting that later waves are allowed to finish and that all group restoration occurs after the final result.
- [ ] Add a cancellation test that releases all request workers before the batch returns.

### Task 2: Verify and package

**Files:**
- Modify: `VERSION` only if the existing release workflow requires a version change.
- Create: release output outside the repository.

- [ ] Run `gofmt`, `go test ./...`, `go vet ./...`, deployment tests, JavaScript syntax checks, and `git diff --check`.
- [ ] Build the Linux amd64 detector and deploy it through the existing update script without replacing server configuration or login state.
- [ ] Verify `/healthz`, the socket, `parallel=1`, Probe group configuration, and absence of `codex-app-server`.
- [ ] Run a real three-question test through the existing browser tunnel and confirm that every selected question completes and the original groups are restored.
