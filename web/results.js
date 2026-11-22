import { analyzeGlobalOutputs } from "./fingerprint-core.js";

const bankHash =
  "f267d0513aa06981d024ec5394746fceddf44e4e8a75fa6bf8304255f4b32d6d";
let bankPromise;
export function loadBank() {
  if (!bankPromise)
    bankPromise = readBank().catch((error) => {
      bankPromise = null;
      throw error;
    });
  return bankPromise;
}
async function readBank() {
  const response = await fetch("/unified_bank.json", {
    credentials: "same-origin",
  });
  if (!response.ok) throw new Error("无法读取指纹库。请刷新页面后重试。");
  const bytes = await response.arrayBuffer();
  const sum = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
  const hash = Array.from(sum, (byte) =>
    byte.toString(16).padStart(2, "0"),
  ).join("");
  if (hash !== bankHash) throw new Error("指纹库校验失败，请检查安装包。");
  return JSON.parse(new TextDecoder().decode(bytes));
}

// Only a completed response paired with its original question enters calibration.
export function scoreAccount(rows, questions, bank) {
  const completed = rows.filter(
    (row) => row.status === "done" && questions.has(row.question_id),
  );
  if (!completed.length) return null;
  const outputs = completed.map((row) => ({
    text: row.text,
    expected_count: questions.get(row.question_id).expected_count,
  }));
  try {
    const score = analyzeGlobalOutputs(outputs, bank);
    return {
      score,
      accepted: new Map(
        score.diagnostics.map((d, i) => [completed[i].question_id, d.accepted]),
      ),
    };
  } catch (error) {
    if (!String(error.message).startsWith("没有可用回答")) throw error;
    return {
      score: null,
      accepted: new Map(completed.map((row) => [row.question_id, false])),
    };
  }
}

export function accountReports(rows, questions, bank, state = "done") {
  const canScore = bank && !active(state);
  const groups = new Map();
  for (const row of rows) {
    if (!groups.has(row.account_id)) groups.set(row.account_id, []);
    groups.get(row.account_id).push(row);
  }
  return Array.from(groups, ([id, answers]) => ({
    id,
    rows: answers,
    result: canScore ? scoreAccount(answers, questions, bank) : null,
  }));
}

export function el(tag, text, className) {
  const node = document.createElement(tag);
  if (text != null) node.textContent = text;
  if (className) node.className = className;
  return node;
}
function details(title, key) {
  const node = el("details");
  node.dataset.detail = key;
  node.append(el("summary", title));
  return node;
}
const labels = {
  queued: "排队中",
  waiting_capacity: "等待账号空位",
  running: "检测中",
  done: "已完成",
  failed: "失败",
  cancelled: "已取消",
  interrupted: "已中断",
};
export const label = (state) => labels[state] || state || "未知";
export const active = (state) =>
  state === "running" || state === "queued" || state === "waiting_capacity";
export function date(value) {
  if (!value || String(value).startsWith("0001-01-01")) return "";
  const time = new Date(value);
  return Number.isNaN(time.getTime())
    ? ""
    : time.toLocaleString("zh-CN", { hour12: false });
}
function percentage(fraction) {
  const value = fraction * 100;
  if (value > 0 && value < 0.01) return "<0.01%";
  if (value > 99.99 && value < 100) return ">99.99%";
  return value.toLocaleString("zh-CN", { maximumFractionDigits: 2 }) + "%";
}
function probabilityList(score, key) {
  const group = details(`全部 ${score.results.length} 个候选模型`, key);
  const list = el("dl", null, "probs");
  for (const candidate of score.results) {
    list.append(
      el("dt", candidate.display_name),
      el("dd", percentage(candidate.probability)),
    );
  }
  group.append(list);
  return group;
}
function appendScore(section, result, total) {
  const score = result?.score;
  if (!score) {
    section.append(el("p", "没有可用于归因的有效答案。", "small"));
    return;
  }
  const line = el("div", null, "metrics");
  const winner = el("span", `最可能：${score.prediction_name} `);
  winner.append(el("strong", percentage(score.probability)));
  line.append(winner);
  if (score.prediction !== "gpt-6-astra") {
    const astra = score.results.find((item) => item.model === "gpt-6-astra");
    if (astra) {
      const astraline = el("span", "Astra ");
      astraline.append(el("strong", percentage(astra.probability)));
      line.append(astraline);
    }
  }
  section.append(
    line,
    el("p", `本次计入 ${score.used_outputs}/${total} 道题。`, "small"),
  );
}
function answer(row, question, accepted) {
  const node = details(
    `挑战 ${question?.number ?? "?"} · ${label(row.status)}${accepted === false ? " · 输出无效" : ""}`,
    `r-${row.account_id}-${row.question_id}`,
  );
  node.classList.add("answer");
  if (row.error) {
    const diagnostic = row.code ? `${row.error} · ${row.code}` : row.error;
    node.append(el("p", diagnostic, "small warning"));
  }
  if (row.model) node.append(el("p", `上游返回标签：${row.model}`, "small"));
  const timing = [date(row.started), date(row.ended)]
    .filter(Boolean)
    .join(" — ");
  if (timing) node.append(el("p", timing, "small"));
  if (row.request_id)
    node.append(el("p", `检测编号 ${row.request_id}`, "small"));
  if (question) node.append(el("h4", "题目"), el("pre", question.prompt));
  if (row.text) node.append(el("h4", "原始答案"), el("pre", row.text));
  return node;
}
export function renderRun(container, run, bank, bankVersion) {
  const opened = new Set(
    Array.from(
      container.querySelectorAll("details[open]"),
      (item) => item.dataset.detail,
    ),
  );
  const questions = new Map(run.questions.map((q) => [q.id, q]));
  const out = document.createDocumentFragment();
  const complete = run.rows.filter((row) => !active(row.status)).length;
  const summary = el("div", null, "summary");
  summary.append(
    el("h3", run.state === "done" && run.rows.every((row) => row.status === "failed")
      ? "本次检测失败" : label(run.state)),
    el("span", `已结束 ${complete}/${run.rows.length} 个请求`, "small"),
  );
  out.append(summary);
  const compatible = Boolean(
    run.bank_version && run.bank_version === bankVersion && bank,
  );
  const pending = active(run.state);
  if (!compatible)
    out.append(
      el(
        "p",
        "当前评分库与记录版本不一致或不可用，请查看原始答案。",
        "warning",
      ),
    );
  else if (pending)
    out.append(el("p", "本批检测结束后计算各账号的归因概率。", "small"));
  for (const { id, rows, result } of accountReports(
    run.rows,
    questions,
    compatible ? bank : null,
    run.state,
  )) {
    const section = el("article", null, "account-result");
    section.append(el("h3", `${rows[0].name || "账号"} · #${id}`));
    if (compatible && !pending) appendScore(section, result, rows.length);
    if (result?.score) section.append(probabilityList(result.score, `p-${id}`));
    for (const row of rows) {
      const q = questions.get(row.question_id);
      section.append(
        answer(row, q, result?.accepted.get(row.question_id)),
      );
    }
    out.append(section);
  }
  out.append(
    el(
      "p",
      "概率仅比较指纹库内的候选，描述本次请求的输出。轻度是请求档位。",
      "footnote report-note",
    ),
  );
  container.replaceChildren(out);
  for (const item of container.querySelectorAll("details"))
    item.open = opened.has(item.dataset.detail);
}
