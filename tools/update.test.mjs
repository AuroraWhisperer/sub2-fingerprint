import assert from "node:assert/strict";
import test from "node:test";
import { createUpdater } from "../web/update.js";

function updateDOM(t) {
  const nodes = new Map();
  const node = (id) => {
    if (!nodes.has(id)) {
      const classes = new Set();
      nodes.set(id, {
        hidden: true, disabled: false, textContent: "", listeners: {},
        classList: {
          add: (name) => classes.add(name),
          remove: (name) => classes.delete(name),
          toggle: (name, active) => active ? classes.add(name) : classes.delete(name),
          contains: (name) => classes.has(name),
        },
        addEventListener(event, callback) { this.listeners[event] = callback; },
      });
    }
    return nodes.get(id);
  };
  const originalDocument = globalThis.document;
  globalThis.document = { getElementById: node };
  t.after(() => { globalThis.document = originalDocument; });
  let scheduled;
  t.mock.method(globalThis, "setTimeout", (callback) => { scheduled = callback; return 1; });
  t.mock.method(globalThis, "clearTimeout", () => { scheduled = undefined; });
  return {
    node,
    tick() {
      assert.equal(typeof scheduled, "function", "update poll was scheduled");
      const next = scheduled;
      scheduled = undefined;
      return next();
    },
  };
}

test("rollback restart refreshes the session before enabling update retry", async (t) => {
  const { node, tick } = updateDOM(t);
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
  await tick();
  assert.equal(sessionEpoch, 2, "reconnect must refresh session before retry");
  assert.equal(updater.busy, false);
  assert.equal(node("updateStatus").textContent, "failed");
  await node("installUpdate").listeners.click();
  assert.equal(installs, 2, "retry must reach the server with the new session");
  assert.equal(updater.busy, true);
});

test("finishing detection clears the update installation blocker", async (t) => {
  const { node } = updateDOM(t);
  const updater = createUpdater(async () => ({ state: "failed", message: "更新失败", current: "v0.3.0", latest: "v0.3.1", available: true, supported: true }), () => {}, async () => true);
  updater.setContext({ logged_in: true }, false);
  await updater.restore();
  assert.equal(node("updateStatus").classList.contains("error"), true);
  updater.setContext({ logged_in: true }, true);
  assert.equal(node("installUpdate").disabled, true);
  assert.match(node("updateStatus").textContent, /检测进行中/);
  assert.equal(node("updateStatus").classList.contains("error"), false);
  updater.setContext({ logged_in: true }, false);
  assert.equal(node("installUpdate").disabled, false);
  assert.equal(node("updateStatus").textContent, "更新失败");
});

test("an overlapping session refresh does not strand a finished update", async (t) => {
  const { tick } = updateDOM(t);
  let state = "running";
  let refreshes = 0;
  const updater = createUpdater(async () => ({ state }), () => {}, async () => ++refreshes > 1);
  updater.setContext({ logged_in: true }, false);
  await updater.restore();
  state = "failed";
  await tick();
  assert.equal(updater.busy, true);
  await tick();
  assert.equal(updater.busy, false);
});

test("update polling refreshes authorization then exposes reconnect on repeated rejection", async (t) => {
  const { node, tick } = updateDOM(t);
  let reject = false;
  let refreshes = 0;
  const api = async () => {
    if (reject) throw Object.assign(new Error("登录已失效"), { status: 401 });
    return { state: "running" };
  };
  const updater = createUpdater(api, () => {}, async () => { refreshes++; return true; });
  updater.setContext({ logged_in: true }, false);
  await updater.restore();
  reject = true;
  await tick();
  assert.equal(updater.busy, true);
  assert.equal(refreshes, 1);
  await tick();
  assert.equal(updater.busy, false);
  assert.equal(node("reconnect").hidden, false);
});
