# Sub2API Probe Gateway Implementation Plan

> **For agentic workers:** Execute this plan task-by-task in the current session. Keep each change independently testable.

**Goal:** Deploy the ModelTrace detector through the local Sub2API gateway with one serial account-group transaction, using `gpt-6-astra` and `reasoning.effort=low`, without starting any official Codex client.

**Architecture:** The existing Go detector remains the browser-facing sidecar and continues to use the local administrator JWT for account metadata. Before every model request it resolves the exact `Probe` group, reads the selected account's original `group_ids`, temporarily adds the group, verifies the membership, sends one streaming `POST /v1/responses` request with the Probe API key, and restores the original group in cleanup. A process-wide mutex serializes all group mutations and a recovery-failure latch blocks later mutations until an operator investigates. The legacy Codex app-server path and its runtime installer are removed.

**Tech Stack:** Go standard library, systemd socket activation, Python deployment scripts, Sub2API admin API, OpenAI-compatible Responses SSE.

## Global Constraints

- Model request model is exactly `gpt-6-astra`.
- Reasoning effort is exactly `low`.
- The detector never launches, embeds, or controls an official Codex client.
- Default and maximum detector parallelism are 1.
- The Probe group is identified by an exact group name match and the account's original group IDs are restored after every attempt.
- A failed group-add verification prevents the model request.
- A failed group restore returns an explicit error and blocks later group mutations in the running process.
- A 503 from the gateway after group verification is surfaced as a Probe-account availability error.
- Credentials and response bodies are not written to logs.

---

### Task 1: Replace the request adapter with a gateway transaction

**Files:**
- Modify: `adapter.go`
- Modify: `main.go`
- Modify: `queue.go`
- Create: `group_transaction_test.go`
- Modify: `adapter_test.go`
- Remove: `codex_gate.go`, `codex_gate_test.go`, `codex_process.go`, `codex_process_test.go`, `codex_runtime_test.go`

**Interfaces:**
- `NewConfiguredAdapter(base, probeKey, probeGroupName string) (*Adapter, error)` constructs the configured adapter.
- `Adapter.Probe(ctx, jwt, accountID, prompt)` owns the serialized group transaction and gateway request.
- `Adapter.groupTransaction(ctx, jwt, accountID, fn)` preserves and verifies group IDs around `fn`.
- `Adapter.gatewayProbe(ctx, prompt)` sends the fixed streaming Responses request.

- [ ] Add typed group data and `group_ids` decoding to account details.
- [ ] Add exact `Probe` group discovery through `GET /admin/groups/all`, rejecting zero or duplicate exact matches.
- [ ] Add `PUT /admin/accounts/{id}` with the exact original-plus-Probe group ID list, then re-read and verify the result.
- [ ] Add cleanup with a fresh bounded context so cancellation and timeout still attempt restoration.
- [ ] Add the process-wide mutex and recovery-failure latch.
- [ ] Send `POST /v1/responses` with `gpt-6-astra`, `reasoning.effort=low`, `stream=true`, `store=false`, and the fixed numerical challenge instruction. Parse the existing SSE completion logic and capture a safe request ID.
- [ ] Map gateway HTTP 503 to a non-secret Probe availability error and refuse to call the gateway if group attachment verification fails.
- [ ] Set `maxParallelAccounts` and the installed default to 1.
- [ ] Replace path-B fixtures with tests covering add, verify, request, restore on success, restore on cancellation, restore failure blocking, no-account 503, and exact model/reasoning payloads.

### Task 2: Remove official-runtime deployment paths

**Files:**
- Modify: `deploy/deploy.py`
- Modify: `deploy/update.py`
- Modify: `deploy/config.example.json`
- Modify: `deploy/units/sub2-fingerprint.service`
- Modify: `deploy/DEPLOYMENT.md`
- Modify: `README.md`
- Modify: `deploy/tests/test_lifecycle.py`

**Interfaces:**
- Deployment config includes `probe_api_key` and `probe_group_name`, with the secret file readable only by the detector service account.
- `install.sh` accepts `--probe-api-key` and optional `--probe-group-name Probe`.
- Updates preserve the configured Probe settings and never install a Codex runtime.

- [ ] Remove runtime directory ownership, download, update, uninstall, and CLI code.
- [ ] Add secret-aware install configuration and default `parallel=1`.
- [ ] Keep the existing loopback socket and launcher entry point at `127.0.0.1:17861`.
- [ ] Update unit hardening and documentation so no deployment step references `codex-app-server`.
- [ ] Update lifecycle tests for the new configuration and absence of runtime installation.

### Task 3: Validate locally and deploy

**Files:**
- Modify: `VERSION` only if a release version change is required by the existing packaging workflow.
- Create: release build output outside the repository when packaging.

- [ ] Install or use a Go 1.23 toolchain and run `gofmt`.
- [ ] Run `go test ./...` and Python deployment tests.
- [ ] Build the Linux amd64 detector and run offline HTTP transaction tests.
- [ ] Upload only the built detector and deployment bundle to the existing server.
- [ ] Verify systemd socket/service and `healthz`.
- [ ] Use a real administrator session to choose one account, verify original groups, add `Probe`, verify the group, send `gpt-6-astra` with `low`, restore groups, and compare the final group set to the original.
- [ ] Verify the no-account behavior by observing a controlled 503 only when the Probe group has no schedulable account, without changing the user's account permanently.
- [ ] Record any upstream model or reasoning incompatibility separately from group-transaction failures.

### Plan self-review

- The document's selected-path requirement is covered by Task 1 and Task 2.
- The document's session, admin API, model, reasoning, low-concurrency, and deployment requirements are covered by Tasks 1–3.
- No official Codex process or runtime remains in the selected path.
- All group mutation failure cases have explicit tests and an operational verification step.
