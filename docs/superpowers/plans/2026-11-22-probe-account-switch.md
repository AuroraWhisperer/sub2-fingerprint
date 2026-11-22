# Probe Account Switch Retry Implementation Plan

> **For agentic workers:** Execute this plan task-by-task in the current session. Keep each change independently testable.

**Goal:** Allow sequential account detection to survive transient gateway HTTP 503 responses while Sub2API refreshes its scheduler after a group change.

**Architecture:** Keep the existing account-group transaction and per-question concurrency. Wrap one gateway attempt with bounded retries for `PROBE_NO_ACCOUNT` only, inside the existing transaction and request deadline, so the selected account stays attached until every question finishes.

**Tech Stack:** Go standard library, httptest, Sub2API Responses gateway.

## Global Constraints

- Preserve existing unrelated workspace edits.
- Retry HTTP 503 only; never replay a successful answer, stream failure, network failure, or another HTTP error.
- Use at most five attempts, with 1, 2, 4, and 8 second waits; cancellation and existing deadlines apply throughout.
- Restore original groups on success, exhausted retries, timeout, and cancellation.
- Do not change account concurrency, model parameters, or deployment configuration.

### Task 1: Reproduce the account switch and protect retry boundaries

**Files:** Create `probe_retry_test.go`; extend `group_transaction_test.go`.

- [x] Exercise the real queue and adapter with two accounts and three questions. The first account succeeds; the second account's gateway returns 503 before its scheduler view catches up. Assert six successful rows, exactly one successful answer per question, no overlapping account membership, and restored groups.
- [x] Cover eventual success, five exhausted attempts, cancellation during backoff, deadline expiry, and non-retryable HTTP/stream/network failures with a short internal test delay.
- [x] Run the regression against the old implementation and confirm the second account fails.

### Task 2: Retry inside the owning gateway adapter

**Files:** Modify `adapter.go`, `adapter_test.go`, `group_transaction_test.go`, `README.md`, and `deploy/DEPLOYMENT.md`.

- [x] Extract the existing single-attempt method as `gatewayProbeOnce(ctx context.Context, prompt string) (ProbeAnswer, error)`.
- [x] Keep `gatewayProbe` as the shared single-question and batch entry point. Return immediately unless the attempt returns a `GateError` with code `PROBE_NO_ACCOUNT`; otherwise wait with a context-aware timer and double the delay for the next attempt.
- [x] Close each failed response before waiting and reuse the same context for all attempts.
- [x] Give the adapter an internal retry delay initialized to one second; use a short delay in HTTP fixtures.
- [x] Document the bounded wait and the final persistent-503 behavior.
- [x] Run focused regression tests, `go test ./...`, and `go vet ./...`; cross-build the Linux server.

### Validation boundary

Local fixtures reproduce delayed gateway scheduling. Production root cause is inferred from the reported successful individual run, immediate sequential failures, and Sub2API's asynchronous scheduler implementation; production logs and a live two-account run are still needed to confirm the deployed result.

### Completed validation

- The original implementation failed the two-account regression with all three second-account rows reporting `PROBE_NO_ACCOUNT`.
- The fixed implementation passes the regression and asserts the exact order `attach:1, restore:1, attach:2, restore:2`, with every question completed before its account is released.
- `go test ./... -count=1` and `go vet ./...` passed.
- Linux amd64 cross-build succeeded at `dist/probe-switch/sub2-fingerprint`.
- Focused `go test -race` could compile but could not start on this Windows toolchain (`0xc0000139`). PE import inspection found unavailable `kernel32.dll` imports: `WakeByAddressSingle`, `WakeByAddressAll`, and `WaitOnAddress`. Race validation remains unverified.
- No server deployment or live model request was performed.
