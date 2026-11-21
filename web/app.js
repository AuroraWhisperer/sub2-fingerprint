import { el, loadBank, renderRun, label, date, active } from "./results.js";

const $ = (id) => document.getElementById(id);
let session = {},
  bank,
  viewed = "",
  revision = 0,
  pollTimer,
  lastRender = "",
  pollSequence = 0,
  pendingSubmit = null;
let readyAccounts = false,
  readyQuestions = false;
const uuid = /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/i;
const show = (id, yes) => {
  $(id).hidden = !yes;
};
function notice(text = "", error = false) {
  const panel = $("notice");
  panel.textContent = text;
  panel.hidden = !text;
  panel.classList.toggle("error", error);
}
async function api(path, body) {
  const options = {
    credentials: "same-origin",
    cache: "no-store",
    redirect: "error",
    headers: { Accept: "application/json" },
  };
  if (body !== undefined) {
    options.method = "POST";
    options.headers["Content-Type"] = "application/json";
    options.headers["X-CSRF-Token"] = session.csrf;
    options.body = JSON.stringify(body);
  }
  let response;
  try {
    response = await fetch(path, options);
  } catch {
    throw new Error("连接中断，请检查本地 SSH 隧道后刷新。");
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
async function failure(error) {
  notice(error.message || "操作失败，请重试。", true);
  if (String(error.code || "").startsWith("STEP_UP") && session.logged_in) {
    $("verify").open = true;
    $("stepForm").elements.code.focus();
  }
  if (error.status === 401 || /csrf/i.test(error.code || "")) {
    try {
      session = await api("/api/session");
      applySession();
    } catch {
      show("reconnect", true);
    }
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
  if (session.version)
    $("version").textContent = `${session.version} · 版本与更新`;
  show("auth", !session.logged_in);
  show("desk", session.logged_in);
  show("logout", session.logged_in || session.needs_2fa);
  show("twoForm", !session.logged_in && session.needs_2fa);
  show("loginForm", !session.logged_in && !session.needs_2fa);
  $("authTitle").textContent = session.needs_2fa
    ? "验证管理员身份"
    : "登录 Sub2API";
  $("authHint").textContent = session.needs_2fa
    ? "输入验证器生成的六位验证码。"
    : "使用管理员账号读取服务器上的 OpenAI 账号。";
  $("parallel").options[1].disabled = !(session.parallel >= 2);
  if (session.parallel < 2) $("parallel").value = "1";
  if (!session.logged_in) {
    stopPoll();
    ++revision;
    viewed = "";
    lastRender = "";
    pendingSubmit = null;
    readyAccounts = readyQuestions = false;
    for (const id of ["accounts", "questions", "result", "history"])
      $(id).replaceChildren();
  }
  updateStart();
}
const checked = (root) =>
  Array.from(
    $(root).querySelectorAll("input:checked:not(:disabled)"),
    (node) => node.value,
  );
function updateStart() {
  const n = checked("accounts").length,
    q = checked("questions").length;
  $("estimate").textContent =
    n && q
      ? `本批次最多发送 ${n * q} 次模型请求，使用所选账号的额度。`
      : "请选择账号和至少一道挑战题。";
  $("start").disabled =
    !session.logged_in ||
    !readyAccounts ||
    !readyQuestions ||
    !bank ||
    !n ||
    !q ||
    $("start").hasAttribute("aria-busy");
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
  const selected = new Set(checked("accounts"));
  readyAccounts = false;
  $("accountInfo").textContent = "正在读取…";
  updateStart();
  const { accounts: list } = await api("/api/accounts");
  if (!session.logged_in) return;
  const rows = list.map((item) => {
    const row = el("label", null, "choice");
    const description = el("span");
    description.append(
      el("span", item.name || "未命名账号", "name"),
      el("span", ` · #${item.id}`, "small"),
    );
    if (item.reason)
      description.append(el("br"), el("span", item.reason, "small"));
    const state = statusNames.get(item.status) || item.status || "未知";
    row.append(
      checkbox(
        item.id,
        selected.has(String(item.id)) && item.eligible,
        !item.eligible,
      ),
      description,
      el("span", state, `state ${item.eligible ? "good" : ""}`),
    );
    return row;
  });
  $("accounts").replaceChildren(
    ...(rows.length
      ? rows
      : [el("p", "当前没有 OpenAI OAuth 账号。", "empty")]),
  );
  $("accountInfo").textContent =
    `${list.filter((item) => item.eligible).length} 个可检测 / 共 ${list.length} 个`;
  readyAccounts = true;
  updateStart();
}
async function questions() {
  readyQuestions = false;
  const { questions: list } = await api("/api/questions");
  if (!session.logged_in) return;
  const rows = list.map((item, index) => {
    const row = el("div", null, "qrow"),
      choice = el("label", null, "choice");
    choice.append(
      checkbox(item.id, index === 0, false),
      el("span", `挑战 ${index + 1}`, "name"),
      el("span", `${item.expected_count} 个数字`, "small"),
    );
    const detail = el("details");
    detail.append(el("summary", "查看题目"), el("p", item.prompt, "prompt"));
    row.append(choice, detail);
    return row;
  });
  $("questions").replaceChildren(...rows);
  readyQuestions = list.length > 0 && list.length <= 3;
  updateStart();
}
const stopPoll = () => clearTimeout(pollTimer);
function hashID() {
  const value = location.hash.slice(1).replace(/^run\//, "");
  return uuid.test(value) ? value : "";
}
function selectRun(id, updateURL = true) {
  if (id && !uuid.test(id)) return;
  stopPoll();
  ++revision;
  viewed = id;
  lastRender = "";
  $("result").replaceChildren(
    el(
      "p",
      id
        ? "正在读取检测记录…"
        : "完成检测后，在这里查看每个账号的答案与归因概率。",
      "empty",
    ),
  );
  $("history").value = id;
  show("cancel", false);
  if (updateURL)
    history.replaceState(
      null,
      "",
      location.pathname + (id ? `#run/${id}` : ""),
    );
  if (id) return poll(revision);
}
async function historyList(preferred) {
  const { runs } = await api("/api/runs");
  preferred ??= viewed || hashID();
  if (!session.logged_in) return;
  const select = $("history");
  select.replaceChildren(
    ...runs.map(
      (run) =>
        new Option(
          `${date(run.at)} · ${label(run.state)} · ${run.id.slice(0, 8)}`,
          run.id,
        ),
    ),
  );
  if (!runs.length) select.add(new Option("暂无记录", ""));
  const next = runs.some((r) => r.id === preferred)
    ? preferred
    : runs[0]?.id || "";
  if (next !== viewed || !lastRender) await selectRun(next);
  else select.value = next;
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
      for (const option of $("history").options)
        if (option.value === run.id)
          option.textContent = `${date(run.at)} · ${label(run.state)} · ${run.id.slice(0, 8)}`;
      if (
        run.rows.some(
          (row) =>
            row.code === "STEP_UP_REQUIRED" ||
            String(row.error || "").includes("二次验证"),
        )
      )
        $("verify").open = true;
    }
    show("cancel", active(run.state));
    if (active(run.state)) pollTimer = setTimeout(() => poll(version), 3000);
  } catch (error) {
    if (version !== revision || sequence !== pollSequence) return;
    await failure(error);
    if (
      version === revision &&
      session.logged_in &&
      (!error.status || error.status >= 500)
    )
      pollTimer = setTimeout(() => poll(version), 6000);
  }
}
async function initialize() {
  show("reconnect", false);
  session = await api("/api/session");
  applySession();
  if (!session.logged_in) return;
  const results = await Promise.allSettled([
    accounts(),
    questions(),
    loadBank().then((value) => {
      bank = value;
      updateStart();
    }),
  ]);
  await historyList();
  const failed = results.find((r) => r.status === "rejected");
  if (failed) {
    show("reconnect", true);
    throw failed.reason;
  }
}
async function submitRun() {
  const payload = {
    accounts: checked("accounts").map(Number),
    questions: checked("questions"),
    parallel: Number($("parallel").value),
  };
  const signature = JSON.stringify(payload);
  // A lost submit response can be retried with the original idempotency key.
  if (!pendingSubmit || pendingSubmit.signature !== signature)
    pendingSubmit = { id: crypto.randomUUID(), signature };
  let id;
  try {
    ({ id } = await api("/api/runs", { ...payload, id: pendingSubmit.id }));
  } catch (error) {
    if (error.status && error.status < 500) pendingSubmit = null;
    throw error;
  }
  if (!uuid.test(id))
    throw new Error("任务编号异常。请刷新检测记录确认是否已经提交。");
  pendingSubmit = null;
  history.replaceState(null, "", `${location.pathname}#run/${id}`);
  await historyList(id);
  notice("检测已提交。");
}
for (const id of ["accounts", "questions", "parallel"])
  $(id).addEventListener("change", updateStart);
$("history").addEventListener("change", () => selectRun($("history").value));
action($("loginForm"), "submit", async () => {
  const form = $("loginForm").elements;
  const body = {
    email: form.email.value.trim(),
    password: form.password.value,
  };
  form.password.value = "";
  await api("/api/login", body);
  await initialize();
  if (session.needs_2fa) $("twoForm").elements.code.focus();
});
for (const [id, path] of [
  ["twoForm", "login2fa"],
  ["stepForm", "stepup"],
])
  action($(id), "submit", async () => {
    const field = $(id).elements.code,
      code = field.value;
    field.value = "";
    await api(`/api/${path}`, { code });
    if (id === "twoForm") await initialize();
    else {
      $("verify").open = false;
      notice("二次验证已完成。");
    }
  });
action($("logout"), "click", async () => {
  await api("/api/logout", {});
  await initialize();
  notice("已退出。");
});
action($("refreshAccounts"), "click", accounts);
action($("reloadResults"), "click", async () => {
  const previousRevision = revision;
  await historyList();
  if (viewed && revision === previousRevision) await poll(revision);
});
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
