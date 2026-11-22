import { el, loadBank, renderRun, active } from "./results.js";
import { createUpdater } from "./update.js";

const $ = (id) => document.getElementById(id);
const appRoot = new URL(".", document.baseURI);
let session = {},
  bank,
  loadingBank = false,
  runActive = false,
  viewed = "",
  revision = 0,
  pollTimer,
  lastRender = "",
  pollSequence = 0,
  pendingSubmit = null;
let readyAccounts = false,
  readyQuestions = false;
const uuid = /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/i;
const updates = createUpdater(api, updateStart, syncSession);
const show = (id, yes) => {
  const node = $(id);
  if (node) node.hidden = !yes;
};
function notice(text = "", error = false) {
  const panel = $("notice");
  panel.textContent = text;
  panel.hidden = !text;
  panel.classList.toggle("error", error);
}
async function api(path, body) {
  const token = (() => {
    try {
      return String(localStorage.getItem("auth_token") || "").trim();
    } catch {
      return "";
    }
  })();
  const options = {
    credentials: "same-origin",
    cache: "no-store",
    redirect: "error",
    headers: { Accept: "application/json" },
  };
  if (token) options.headers.Authorization = `Bearer ${token}`;
  if (body !== undefined) {
    options.method = "POST";
    options.headers["Content-Type"] = "application/json";
    options.headers["X-CSRF-Token"] = session.csrf;
    options.body = JSON.stringify(body);
  }
  let response;
  try {
    response = await fetch(new URL(path.replace(/^\/+/, ""), appRoot), options);
  } catch {
    throw new Error("连接中断，请刷新页面。");
  }
  const data = await response.json().catch(() => null);
  if (!response.ok || !data) {
    const error = new Error(
      data?.error || `服务响应异常（HTTP ${response.status}）。`,
    );
    Object.assign(error, { code: data?.code, status: response.status });
    throw error;
  }
  return data;
}
async function syncSession() {
  const previous = session;
  let current;
  try {
    current = await api("/api/session");
  } catch (error) {
    if (error.status === 401 && error.code === "SUB2_SESSION") {
      session = { logged_in: false };
      applySession();
      if (previous.logged_in)
        notice("登录已失效，请重新登录管理后台。", true);
      return false;
    }
    throw error;
  }
  if (session !== previous) return false;
  if (
    current.logged_in &&
    (!previous.logged_in ||
      (previous.epoch === current.epoch && previous.csrf !== current.csrf))
  ) {
    location.reload();
    return false;
  }
  session = current;
  applySession();
  show("reconnect", false);
  if (previous.logged_in && !current.logged_in)
    notice("登录已失效，请重新登录管理后台。", true);
  return current.logged_in;
}
async function failure(error) {
  notice(error.message || "操作失败，请重试。", true);
  if (String(error.code || "").startsWith("STEP_UP") && session.logged_in) {
    show("verify", true);
    $("stepForm").elements.code.focus();
  }
  if (
    error.status === 401 ||
    ["CSRF", "STALE_SESSION", "NO_CHALLENGE", "ALREADY_LOGGED_IN"].includes(
      error.code,
    )
  ) {
    try {
      await syncSession();
    } catch {
      show("reconnect", true);
    }
  } else if (!error.status || error.status >= 500) {
    show("reconnect", true);
  }
}
function action(element, event, job) {
  element.addEventListener(event, async (e) => {
    e.preventDefault();
    const button =
      event === "submit"
        ? element.querySelector("button[type=submit]")
        : element;
    if (button.disabled) return;
    button.disabled = true;
    button.setAttribute("aria-busy", "true");
    notice();
    try {
      await job();
    } catch (error) {
      await failure(error);
    } finally {
      button.removeAttribute("aria-busy");
      button.disabled = false;
      updateStart();
    }
  });
}
function applySession() {
  show("version", Boolean(session.version));
  if (session.version) $("version").textContent = session.version;
  show("auth", !session.logged_in);
  show("desk", session.logged_in);
  if (!session.logged_in) {
    stopPoll();
    ++revision;
    viewed = "";
    lastRender = "";
    pendingSubmit = null;
    runActive = false;
    readyAccounts = readyQuestions = false;
    for (const id of ["accounts", "questions", "result"])
      $(id).replaceChildren();
    show("runProgress", false);
    show("verify", false);
  }
  updateStart();
}
const checked = (root) =>
  Array.from(
    $(root).querySelectorAll("input:checked:not(:disabled)"),
    (node) => node.value,
  );
const enabledBoxes = (root) =>
  Array.from($(root).querySelectorAll("input:not(:disabled)"));
function syncToggleLabels() {
  const acctBoxes = enabledBoxes("accounts");
  const allAccts =
    acctBoxes.length > 0 && acctBoxes.every((box) => box.checked);
  $("selectAllAccounts").textContent = allAccts ? "清空" : "全选";
  $("selectAllAccounts").disabled = acctBoxes.length === 0;

  const qBoxes = enabledBoxes("questions");
  const allQs = qBoxes.length > 0 && qBoxes.every((box) => box.checked);
  $("selectAllQuestions").textContent = allQs ? "清空" : "全选";
  $("selectAllQuestions").disabled = qBoxes.length === 0;
}
function updateStart() {
  updates.setContext(session, runActive);
  const n = checked("accounts").length,
    q = checked("questions").length;
  const start = $("start");
  syncToggleLabels();
  let reason = "";
  if (!session.logged_in) reason = "未登录";
  else if (updates.busy) reason = "程序更新中";
  else if (start.hasAttribute("aria-busy")) reason = "提交中…";
  else if (runActive) reason = "检测进行中";
  else if (!readyAccounts || !readyQuestions) reason = "加载中…";
  else if (!n && !enabledBoxes("accounts").length) reason = "无可用账号";
  else if (!n) reason = "未选账号";
  else if (n > 24) reason = "最多选 24 个账号";
  else if (!q) reason = "未选挑战题";
  start.disabled = Boolean(reason);
  $("estimate").textContent =
    reason || `已选 ${n} 账号 · ${q} 题（${n * q} 次请求）`;
}
function checkbox(value, checkedValue, disabled) {
  const box = el("input");
  box.type = "checkbox";
  box.value = value;
  box.checked = checkedValue;
  box.disabled = disabled;
  return box;
}
const statusNames = new Map([
  ["active", "正常"],
  ["inactive", "停用"],
  ["disabled", "停用"],
  ["error", "异常"],
  ["expired", "过期"],
]);
async function accounts() {
  readyAccounts = false;
  $("accountInfo").textContent = "读取中…";
  updateStart();
  const { accounts: list } = await api("/api/accounts");
  if (!session.logged_in) return;
  const selected = new Set(checked("accounts"));
  const rows = list.map((item) => {
    const row = el("label", null, "choice");
    const main = el("span", null, "choice-main");
    const titleLine = el("span", null, "choice-title");
    titleLine.append(
      el("span", item.name || "未命名账号", "name"),
      el("span", `#${item.id}`, "acct-id"),
    );
    main.append(titleLine);
    if (item.reason) {
      main.append(el("span", item.reason, "choice-sub"));
    }
    const meta = el("span", null, "choice-meta");
    if (
      Number.isInteger(item.current_concurrency) &&
      Number.isInteger(item.concurrency)
    ) {
      const limit = item.concurrency > 0 ? item.concurrency : "∞";
      const full =
        item.eligible &&
        item.concurrency > 0 &&
        item.current_concurrency >= item.concurrency;
      meta.append(
        el(
          "span",
          `${item.current_concurrency}/${limit}`,
          `cap-badge${full ? " full" : ""}`,
        ),
      );
    }
    const state = statusNames.get(item.status) || item.status || "未知";
    meta.append(
      el("span", state, `state ${item.eligible ? "good" : "bad"}`),
    );
    row.append(
      checkbox(
        item.id,
        selected.has(String(item.id)) && item.eligible,
        !item.eligible,
      ),
      main,
      meta,
    );
    return row;
  });
  $("accounts").replaceChildren(
    ...(rows.length
      ? rows
      : [el("p", "暂无 OpenAI OAuth 账号。", "empty")]),
  );
  const eligibleCount = list.filter((item) => item.eligible).length;
  $("accountInfo").textContent = `${eligibleCount}/${list.length} 可用`;
  readyAccounts = true;
  updateStart();
}
async function questions() {
  readyQuestions = false;
  updateStart();
  const { questions: list } = await api("/api/questions");
  if (!session.logged_in) return;
  const rows = list.map((item, index) => {
    const choice = el("label", null, "q-card");
    choice.append(
      checkbox(item.id, index < 3, false),
      el("span", `挑战 ${item.number ?? index + 1}`, "name"),
    );
    return choice;
  });
  $("questions").replaceChildren(...rows);
  readyQuestions = list.length > 0 && list.length <= 3;
  updateStart();
}
const stopPoll = () => clearTimeout(pollTimer);
function selectRun(id, alreadyActive = false) {
  if (id && !uuid.test(id)) throw new Error("任务编号异常，请刷新本次结果。");
  stopPoll();
  ++revision;
  viewed = id;
  lastRender = "";
  if (id && alreadyActive) {
    $("progressLabel").textContent = "正在发起检测…";
    $("progressDetail").textContent = "准备中";
    $("runProgress").dataset.state = "running";
    $("progressBar").dataset.state = "running";
    $("progressBar").firstElementChild.style.width = "8%";
    show("runProgress", true);
    $("result").replaceChildren();
  } else {
    $("result").replaceChildren(
      el(
        "p",
        id ? "正在同步检测结果…" : "选择账号和挑战题后开始检测。",
        "empty",
      ),
    );
    show("runProgress", false);
  }
  runActive = alreadyActive;
  updateStart();
  show("cancel", false);
  if (id) return poll(revision);
}
async function currentRun() {
  const version = revision;
  const { runs } = await api("/api/runs");
  if (!session.logged_in || revision !== version) return;
  const latest = runs[0]?.id || "";
  if (latest && viewed === latest) return poll(version);
  return selectRun(latest, active(runs[0]?.state));
}
async function poll(version) {
  const runID = viewed;
  const sequence = ++pollSequence;
  if (!session.logged_in || !runID || version !== revision) return;
  stopPoll();
  try {
    const run = await api(`/api/runs/${runID}`);
    if (version !== revision || sequence !== pollSequence || !session.logged_in)
      return;
    const imprint = JSON.stringify(run);
    if (imprint !== lastRender) {
      renderRun($("result"), run, bank, session.bank_version);
      lastRender = imprint;
      if (
        run.rows.some(
          (row) =>
            row.code === "STEP_UP_REQUIRED" ||
            String(row.error || "").includes("二次验证"),
        )
      ) {
        show("verify", true);
      }
    }
    runActive = active(run.state);
    updateStart();
    show("cancel", runActive);
    if (runActive) pollTimer = setTimeout(() => poll(version), 3000);
  } catch (error) {
    if (version !== revision || sequence !== pollSequence) return;
    if (error.status === 404) {
      selectRun("");
      notice("服务器已清除本次检测结果。");
    } else {
      await failure(error);
    }
  }
}
async function prepareBank() {
  if (bank || loadingBank) return;
  loadingBank = true;
  $("bankMessage").textContent = "正在加载指纹库…";
  show("bankStatus", true);
  show("retryBank", false);
  try {
    bank = await loadBank();
  } catch (error) {
    $("bankMessage").textContent = error.message?.includes("指纹库")
      ? error.message
      : "指纹库加载失败。";
    show("retryBank", true);
    return;
  } finally {
    loadingBank = false;
  }
  show("bankStatus", false);
  if (session.logged_in && lastRender)
    renderRun($("result"), JSON.parse(lastRender), bank, session.bank_version);
}
async function initialize() {
  show("reconnect", false);
  try {
    session = await api("/api/session");
  } catch (error) {
    if (error.status === 401 && error.code === "SUB2_SESSION") {
      session = { logged_in: false };
      applySession();
      return;
    }
    throw error;
  }
  applySession();
  if (!session.logged_in) return;
  void prepareBank();
  const results = await Promise.allSettled([
    updates.restore(),
    accounts(),
    questions(),
    currentRun(),
  ]);
  const failed = results.find((r) => r.status === "rejected");
  if (failed) {
    show("reconnect", true);
    throw failed.reason;
  }
}
function selection() {
  return {
    accounts: checked("accounts").map(Number),
    questions: checked("questions"),
    parallel: 1,
  };
}
async function submitRun() {
  updateStart();
  const payload = selection();
  const signature = JSON.stringify(payload);
  if (!(await syncSession())) return;
  if (runActive) {
    notice("检测进行中，请等待当前批次完成。");
    return;
  }
  if (JSON.stringify(selection()) !== signature) {
    notice("所选账号或题目已变化，请确认后再开始检测。");
    return;
  }
  if (typeof session.epoch !== "string" || !session.epoch)
    throw new Error("无法验证检测服务版本，请刷新页面后重试。");
  if (!pendingSubmit || pendingSubmit.signature !== signature)
    pendingSubmit = {
      id: crypto.randomUUID(),
      epoch: session.epoch,
      signature,
    };
  const attempt = pendingSubmit;
  let id;
  try {
    ({ id } = await api("/api/runs", {
      ...payload,
      id: attempt.id,
      epoch: attempt.epoch,
    }));
  } catch (error) {
    if (error.status === 409 && error.code === "OLD_RUN")
      error.message = "检测服务已重启，请重新点击“开始检测”。";
    if (error.status && error.status < 500 && error.code !== "CSRF")
      pendingSubmit = null;
    throw error;
  }
  if (pendingSubmit !== attempt) return;
  if (id !== attempt.id)
    throw new Error("任务编号异常，请刷新确认是否已提交。");
  pendingSubmit = null;
  await selectRun(id, true);
}
for (const id of ["accounts", "questions"])
  $(id).addEventListener("change", updateStart);
$("selectAllAccounts").addEventListener("click", () => {
  const boxes = enabledBoxes("accounts");
  const next = !boxes.every((box) => box.checked);
  for (const box of boxes.slice(0, 24)) box.checked = next;
  if (!next) for (const box of boxes) box.checked = false;
  updateStart();
});
$("selectAllQuestions").addEventListener("click", () => {
  const boxes = enabledBoxes("questions");
  const next = !boxes.every((box) => box.checked);
  for (const box of boxes) box.checked = next;
  updateStart();
});
action($("stepForm"), "submit", async () => {
  const field = $("stepForm").elements.code,
    code = field.value;
  field.value = "";
  await api("/api/stepup", { code });
  show("verify", false);
  notice("二次验证已完成。");
});
action($("refreshAccounts"), "click", accounts);
action($("reloadResults"), "click", currentRun);
action($("retryBank"), "click", prepareBank);
action($("runForm"), "submit", submitRun);
action($("cancel"), "click", async () => {
  await api(`/api/runs/${viewed}/cancel`, {});
  await poll(revision);
});
action($("reconnect"), "click", initialize);
initialize().catch(async (error) => {
  show("reconnect", true);
  await failure(error);
});
