import assert from "node:assert/strict";
import test from "node:test";
import { createUpdater } from "../web/update.js";

test("rollback restart refreshes the session before enabling update retry", async (t) => {
  const nodes = new Map();
  const node = (id) => {
    if (!nodes.has(id)) nodes.set(id, {
      hidden: false, disabled: false, textContent: "", listeners: {},
      classList: { add() {}, remove() {}, toggle() {} },
      addEventListener(event, callback) { this.listeners[event] = callback; },
    });
    return nodes.get(id);
  };
  const originalDocument = globalThis.document;
  globalThis.document = { getElementById: node };
  t.after(() => { globalThis.document = originalDocument; });
  let scheduled;
  t.mock.method(globalThis, "setTimeout", (callback) => { scheduled = callback; return 1; });
  t.mock.method(globalThis, "clearTimeout", () => { scheduled = undefined; });
  let serverEpoch = 1, sessionEpoch = 1, installs = 0;
  let state = "idle";
  const current = () => ({ state, current: "v0.3.0", latest: "v0.3.1", supported: true, available: true, message: state });
  const api = async (path, body) => {
    if (body !== undefined && serverEpoch !== sessionEpoch)
      throw Object.assign(new Error("CSRF"), { code: "CSRF", status: 403 });
    if (path === "/api/update/install") { ++installs; state = "queued"; }
    return current();
  };
  const updater = createUpdater(api, () => {}, async () => { sessionEpoch = serverEpoch; return true; });
  updater.setContext({ logged_in: true }, false);
  await updater.restore();
  await node("installUpdate").listeners.click();
  assert.equal(installs, 1);
  assert.equal(updater.busy, true);
  // A failed real update rolls back the server, changing its epoch and CSRF.
  serverEpoch = 2;
  state = "failed";
  await scheduled();
  assert.equal(sessionEpoch, 2, "reconnect must refresh session before retry");
  assert.equal(updater.busy, false);
  assert.equal(node("updateStatus").textContent, "failed");
  await node("installUpdate").listeners.click();
  assert.equal(installs, 2, "retry must reach the server with the new session");
  assert.equal(updater.busy, true);
});

test("finishing detection clears the update installation blocker", async (t) => {
  const nodes = new Map();
  const originalDocument = globalThis.document;
  globalThis.document = { getElementById(id) {
    if (!nodes.has(id)) nodes.set(id, { classList: { toggle() {} }, addEventListener() {} });
    return nodes.get(id);
  } };
  t.after(() => { globalThis.document = originalDocument; });
  const updater = createUpdater(async () => ({ state: "idle", current: "v0.3.0", latest: "v0.3.1", available: true, supported: true }), () => {}, async () => true);
  updater.setContext({ logged_in: true }, true);
  await updater.restore();
  assert.equal(nodes.get("installUpdate").disabled, true);
  assert.match(nodes.get("updateStatus").textContent, /检测进行中/);
  updater.setContext({ logged_in: true }, false);
  assert.equal(nodes.get("installUpdate").disabled, false);
  assert.doesNotMatch(nodes.get("updateStatus").textContent, /检测进行中/);
});
