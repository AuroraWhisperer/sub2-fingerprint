# ModelTrace Detector UI Implementation Plan

> **For agentic workers:** Execute this plan task-by-task in the current session. Keep each change independently testable.

**Goal:** Give the private ModelTrace detector a calm, Claude-inspired desktop interface with clear batch progress, compact copy, and a responsive single-column fallback.

**Architecture:** Keep the current HTML entry points, API contracts, polling cadence, scoring logic, and authentication flow. Replace the visual tokens and layout in `web/style.css`, add a small progress region to `web/index.html`, and derive progress labels and a percentage from the existing run rows in `web/results.js`.

**Tech Stack:** Embedded HTML, vanilla ES modules, CSS, Go `embed.FS`, browser polling.

## Global Constraints

- The detector continues to use `gpt-6-astra` with `reasoning.effort=low`.
- Account group mutations remain serialized with `parallel=1` until a per-account transaction lock is implemented and verified.
- The page is publicly reachable through `https://lirahub.dpdns.org/modeltrace/`; the loopback listener remains an internal upstream only.
- Progress copy must describe observable request states and must not claim an internal transaction phase that the API does not expose.
- The page must retain keyboard focus styles, reduced-motion support, loading, empty, error, and mobile states.

---

### Task 1: Replace the page composition and visual system

**Files:**
- Modify: `web/index.html`
- Modify: `web/style.css`

**Interfaces:**
- Preserve IDs used by `web/app.js`, including `auth`, `desk`, `accounts`, `questions`, `parallel`, `start`, `result`, `cancel`, and `notice`.
- Add progress IDs consumed by `web/results.js`, including `runProgress`, `progressBar`, `progressLabel`, and `progressDetail`.

- [ ] Keep the existing login, account selection, challenge selection, step-up, cancel, and result containers.
- [ ] Remove retention and implementation-copy that does not help the next user action.
- [ ] Use a warm neutral surface, dark ink, muted slate text, a restrained coral accent, and one soft shadow system.
- [ ] Make the authenticated desktop view use the available width with a selection column and a larger result column, with a single column below the mobile breakpoint.
- [ ] Give the result panel a sticky desktop header and a visible progress strip without adding decorative cards or duplicate headings.
- [ ] Preserve touch targets, focus rings, reduced-motion behavior, and readable contrast.

### Task 2: Add observable batch progress

**Files:**
- Modify: `web/results.js`

**Interfaces:**
- `renderRun(container, run, bank, bankVersion)` continues to render the full run.
- `renderRun` updates `#progressBar`, `#progressLabel`, and `#progressDetail` when the progress region exists.

- [ ] Count completed, failed, cancelled, and interrupted rows as finished.
- [ ] Set the progress percentage from finished rows divided by total rows.
- [ ] Map the existing row states to concise Chinese copy such as `排队中`, `检测中`, `等待账号空位`, and `已完成`.
- [ ] Show a completed summary and keep the cancel action visible only while the run is active.
- [ ] Preserve raw answers, model tags, question details, score output, and the existing details-open state.
- [ ] Keep the renderer safe when the progress elements are absent so cached pages do not break.

### Task 3: Verify and ship

**Files:**
- Modify: no source files beyond Tasks 1–2.
- Create: release output outside the repository.

- [ ] Run the embedded asset build and the Go and Python test suites.
- [ ] Run the Impeccable detector once against `web/index.html` and `web/style.css`.
- [ ] Reload the real browser page and verify the authenticated wide layout, progress region, result rendering, and responsive fallback.
- [ ] Recheck the server socket, health endpoint, `parallel=1`, Probe group configuration, and absence of `codex-app-server`.
- [ ] Deploy the rebuilt bundle to the existing server and verify the same checks after restart.
