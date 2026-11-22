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
  const response = await fetch(
    new URL("unified_bank.json", document.baseURI),
    {
      credentials: "same-origin",
    },
  );
  if (!response.ok) throw new Error("无法读取指纹库。");
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
const labels = {
  queued: "排队中",
  waiting_capacity: "等待空位",
  running: "请求中",
  done: "已完成",
  failed: "失败",
  cancelled: "已取消",
  interrupted: "已中断",
};
export const label = (state) => labels[state] || state || "未知";
export const active = (state) =>
  state === "running" || state === "queued" || state === "waiting_capacity";

function renderProgress(run) {
  const progress = document.getElementById("runProgress");
  const bar = document.getElementById("progressBar");
  const labelNode = document.getElementById("progressLabel");
  const detailNode = document.getElementById("progressDetail");
  if (!progress || !bar || !labelNode || !detailNode) return;

  const total = run.rows.length;
  const complete = run.rows.filter((row) => !active(row.status)).length;
  const inFlight = run.rows.filter((row) => row.status === "running").length;
  const percentage = total ? Math.round((complete / total) * 100) : 0;
  const isPending = active(run.state);
  const visualPct = isPending
    ? total
      ? Math.max(
          6,
          Math.min(96, Math.round(((complete + inFlight * 0.35) / total) * 100)),
        )
      : 8
    : percentage;

  const runningRow = run.rows.find((row) => row.status === "running");
  const waitingRow = run.rows.find((row) => row.status === "waiting_capacity");
  const allFailed =
    run.state === "done" &&
    total > 0 &&
    run.rows.every((row) => row.status === "failed");

  let progressLabel = label(run.state);
  if (isPending) {
    if (runningRow) {
      const acctName = runningRow.name || `#${runningRow.account_id}`;
      progressLabel = `正在请求 ${acctName}…`;
    } else if (waitingRow) {
      const acctName = waitingRow.name || `#${waitingRow.account_id}`;
      progressLabel = `${acctName} 等待并发空位…`;
    } else {
      progressLabel = "正在调度队列…";
    }
  } else if (allFailed) {
    progressLabel = "检测失败";
  } else if (run.state === "done") {
    progressLabel = "检测完成";
  } else if (run.state === "cancelled") {
    progressLabel = "已取消";
  } else if (run.state === "interrupted") {
    progressLabel = "已中断";
  }

  const stateKey = allFailed ? "failed" : run.state;
  labelNode.textContent = progressLabel;
  detailNode.textContent = isPending
    ? inFlight > 0
      ? `${complete}/${total} · ${inFlight} 进行中`
      : `${complete}/${total}`
    : `${complete} / ${total}`;
  progress.dataset.state = stateKey;
  bar.setAttribute("aria-valuenow", String(percentage));
  bar.setAttribute("aria-valuetext", `${complete} / ${total}`);
  bar.dataset.state = stateKey;
  bar.firstElementChild.style.width = `${visualPct}%`;
  progress.hidden = false;
}

function percentage(fraction) {
  const value = fraction * 100;
  if (value > 0 && value < 0.01) return "<0.01%";
  if (value > 99.99 && value < 100) return ">99.99%";
  return value.toLocaleString("zh-CN", { maximumFractionDigits: 2 }) + "%";
}

function probabilityList(score) {
  const group = el("div", null, "detail-box probs-box");
  const sum = el("div", null, "detail-summary");
  sum.append(
    el("span", "候选模型分布"),
    el("span", `${score.results.length} 个候选`, "detail-meta"),
  );
  group.append(sum);

  const list = el("div", null, "probs");
  for (const candidate of score.results) {
    const row = el(
      "div",
      null,
      `prob-row${candidate.probability >= 0.01 ? " notable" : ""}`,
    );
    const track = el("span", null, "prob-bar");
    const fill = el("span");
    fill.style.width = `${Math.max(candidate.probability * 100, 0.4)}%`;
    track.append(fill);
    row.append(
      el("span", candidate.display_name, "prob-name"),
      track,
      el("span", percentage(candidate.probability), "prob-val"),
    );
    list.append(row);
  }
  group.append(list);
  return group;
}

function buildBadges(result, rows, pending, compatible) {
  const wrap = el("div", null, "account-badges");
  if (pending) {
    const done = rows.filter((r) => !active(r.status)).length;
    const isRunning = rows.some((r) => r.status === "running");
    const isWaiting =
      !isRunning && rows.some((r) => r.status === "waiting_capacity");
    if (done === rows.length) {
      wrap.append(
        el("span", `已返回 ${done}/${rows.length}`, "sample-pill good"),
      );
    } else if (isRunning) {
      wrap.append(
        el("span", `请求中 ${done}/${rows.length}`, "sample-pill active"),
      );
    } else if (isWaiting) {
      wrap.append(
        el("span", `等待空位 ${done}/${rows.length}`, "sample-pill warning"),
      );
    } else {
      wrap.append(el("span", `排队中 ${done}/${rows.length}`, "sample-pill"));
    }
    return wrap;
  }
  if (!compatible) return wrap;
  const score = result?.score;
  if (!score) {
    wrap.append(el("span", "无有效输出", "sample-pill warning"));
    return wrap;
  }
  const primary = el("span", null, "pred-badge");
  primary.append(
    el("span", score.prediction_name, "pred-name"),
    el("strong", percentage(score.probability)),
  );
  wrap.append(primary);
  if (score.prediction !== "gpt-6-astra") {
    const astra = score.results.find((item) => item.model === "gpt-6-astra");
    if (astra) {
      const sub = el("span", null, "pred-badge secondary");
      sub.append(
        el("span", "Astra", "pred-name"),
        el("strong", percentage(astra.probability)),
      );
      wrap.append(sub);
    }
  }
  wrap.append(
    el("span", `有效 ${score.used_outputs}/${rows.length}`, "sample-pill"),
  );
  return wrap;
}

function answer(row, question, accepted) {
  const node = el("div", null, "detail-box answer");
  const sum = el("div", null, "detail-summary");
  sum.append(el("span", `挑战 ${question?.number ?? "?"}`, "ans-title"));

  const meta = el("span", null, "detail-meta");
  if (row.model) {
    meta.append(el("span", row.model, "model-tag"));
  }
  const statusText =
    accepted === false ? "输出无效" : label(row.status);
  const statusTone =
    accepted === false || row.status === "failed"
      ? "bad"
      : row.status === "done"
        ? "good"
        : row.status === "running"
          ? "active"
          : "";
  meta.append(el("span", statusText, `state ${statusTone}`.trim()));
  sum.append(meta);
  node.append(sum);

  const body = el("div", null, "answer-body");
  if (row.error) {
    const diagnostic = row.code ? `${row.error} (${row.code})` : row.error;
    body.append(el("p", diagnostic, "ans-error"));
  }
  if (row.text) {
    body.append(el("pre", row.text));
  } else if (!row.error) {
    const waitCopy =
      row.status === "running"
        ? "正在等待上游模型返回输出…"
        : row.status === "waiting_capacity"
          ? "账号并发已满，正在等待空位…"
          : row.status === "queued"
            ? "等待前序请求完成后发起…"
            : "暂无输出";
    body.append(el("p", waitCopy, "footnote"));
  }
  node.append(body);
  return node;
}

export function renderRun(container, run, bank, bankVersion) {
  const questions = new Map(run.questions.map((q) => [q.id, q]));
  const out = document.createDocumentFragment();
  renderProgress(run);

  const compatible = Boolean(
    run.bank_version && run.bank_version === bankVersion && bank,
  );
  const pending = active(run.state);
  if (!run.bank_version || run.bank_version !== bankVersion) {
    out.append(
      el("p", "指纹库与当前批次版本不一致，仅显示原始输出。", "warning-banner"),
    );
  }

  const list = el("div", null, "account-list");
  for (const { id, rows, result } of accountReports(
    run.rows,
    questions,
    compatible ? bank : null,
    run.state,
  )) {
    const section = el("article", null, "account-result");
    const head = el("div", null, "account-head");
    const title = el("div", null, "account-title");
    title.append(
      el("h3", rows[0].name || "账号"),
      el("span", `#${id}`, "acct-id"),
    );
    head.append(title, buildBadges(result, rows, pending, compatible));
    section.append(head);

    if (result?.score) {
      section.append(probabilityList(result.score));
    }
    const answersWrap = el("div", null, "answers-grid");
    for (const row of rows) {
      const q = questions.get(row.question_id);
      answersWrap.append(
        answer(row, q, result?.accepted.get(row.question_id)),
      );
    }
    section.append(answersWrap);
    list.append(section);
  }
  out.append(list);
  container.replaceChildren(out);
}
