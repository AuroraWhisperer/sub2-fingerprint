import { el, loadBank, renderRun, date, active } from "./results.js";

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
async function syncSession() {
  const previous = session;
  const current = await api("/api/session");
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
  if (
    (previous.logged_in || previous.needs_2fa) &&
    !current.logged_in &&
    !current.needs_2fa
  )
    notice("登录已失效，请重新登录。", true);
  return current.logged_in;
}
async function failure(error) {
  notice(error.message || "操作失败，请重试。", true);
  if (String(error.code || "").startsWith("STEP_UP") && session.logged_in) {
    $("verify").open = true;
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
  const expires = session.logged_in && date(session.expires_at);
  show("sessionInfo", Boolean(expires));
  $("sessionInfo").textContent = expires
    ? `${session.remembered ? "此浏览器保持登录至" : "本次登录有效至"} ${expires}`
    : "";
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
  const parallel = $("parallel");
  const limit = [2, 3].includes(session.parallel) ? session.parallel : 1;
  for (const option of parallel.options)
    option.disabled = Number(option.value) > limit;
  if (!parallel.value || parallel.selectedOptions[0].disabled)
    parallel.value = "1";
  if (!session.logged_in) {
    stopPoll();
    ++revision;
    viewed = "";
    lastRender = "";
    pendingSubmit = null;
    readyAccounts = readyQuestions = false;
    for (const id of ["accounts", "questions", "result"])
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
function selectRun(id) {
  if (id && !uuid.test(id)) throw new Error("任务编号异常，请刷新本次结果。");
  stopPoll();
  ++revision;
  viewed = id;
  lastRender = "";
  $("result").replaceChildren(
    el(
      "p",
      id
        ? "正在读取本次结果…"
        : "完成检测后，在这里查看每个账号的答案与归因概率。",
      "empty",
    ),
  );
  show("cancel", false);
  if (id) return poll(revision);
}
async function currentRun() {
  const version = revision;
  const { runs } = await api("/api/runs");
  if (!session.logged_in || revision !== version) return;
  const latest = runs[0]?.id || "";
  if (latest && viewed === latest) return poll(version);
  return selectRun(latest);
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
      )
        $("verify").open = true;
    }
    show("cancel", active(run.state));
    if (active(run.state)) pollTimer = setTimeout(() => poll(version), 3000);
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
  await currentRun();
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
  if (!(await syncSession())) return;
  if (payload.parallel !== Number($("parallel").value)) {
    notice("服务器并发上限已变化，请按当前选项重新开始检测。", true);
    return;
  }
  if (typeof session.epoch !== "string" || !session.epoch)
    throw new Error("无法验证检测服务版本，请刷新页面后重试。");
  // A retry of an unanswered submit belongs to its original server process.
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
      error.message =
        "检测服务已重启，上次请求不能自动重发。如需新的检测，请再次点击“开始检测”。";
    if (error.status && error.status < 500 && error.code !== "CSRF")
      pendingSubmit = null;
    throw error;
  }
  if (pendingSubmit !== attempt) return;
  if (id !== attempt.id)
    throw new Error("任务编号异常，请刷新本次结果确认是否已经提交。");
  pendingSubmit = null;
  notice("检测已提交。");
  await selectRun(id);
}
for (const id of ["accounts", "questions", "parallel"])
  $(id).addEventListener("change", updateStart);
action($("loginForm"), "submit", async () => {
  // A login form can outlive the anonymous session when the service sleeps.
  session = await api("/api/session");
  if (session.logged_in || session.needs_2fa) {
    await initialize();
    return;
  }
  const form = $("loginForm").elements;
  const body = {
    email: form.email.value.trim(),
    password: form.password.value,
    remember: form.remember.checked,
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
  session = {};
  applySession();
  await initialize();
  notice("已退出。");
});
action($("refreshAccounts"), "click", accounts);
action($("reloadResults"), "click", currentRun);
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
