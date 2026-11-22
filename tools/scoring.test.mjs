import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { analyzeGlobalOutputs } from "../web/fingerprint-core.js";
import { accountReports, scoreAccount } from "../web/results.js";

const asset = (name) => readFile(new URL(`../web/${name}`, import.meta.url));
const bankBytes = await asset("unified_bank.json");
const bank = JSON.parse(bankBytes);
const questions = new Map([
  ["q1", { expected_count: 218 }],
  ["q2", { expected_count: 223 }],
  ["q3", { expected_count: 233 }],
]);

// Synthetic number sequences keep the suite offline and independent of any account.
function sample(account, id, stride) {
  const length = questions.get(id).expected_count;
  const text = Array.from(
    { length },
    (_, i) => ((i * stride + length) % 355) + 1,
  ).join(",");
  return { account_id: account, question_id: id, status: "done", text };
}
function upstream(rows) {
  return analyzeGlobalOutputs(
    rows.map((row) => ({
      text: row.text,
      expected_count: questions.get(row.question_id).expected_count,
    })),
    bank,
  );
}

const a = [sample(10, "q1", 7), sample(10, "q2", 11), sample(10, "q3", 13)];
const b = [sample(20, "q1", 71), sample(20, "q2", 5)];

test("vendored bank and scorer match the pinned ModelTrace revision", async () => {
  const hashes = [
    [
      bankBytes,
      "f267d0513aa06981d024ec5394746fceddf44e4e8a75fa6bf8304255f4b32d6d",
    ],
    [
      await asset("fingerprint-core.js"),
      "83fa5bd611e18f8339122582335123c8ea168ed242298bb31f4e363abeeb6e4a",
    ],
  ];
  for (const [bytes, digest] of hashes)
    assert.equal(createHash("sha256").update(bytes).digest("hex"), digest);
});

test("one, two and three answers match all of the original scorer outputs", () => {
  for (let count = 1; count <= 3; count++) {
    const rows = a.slice(0, count);
    const result = scoreAccount(rows, questions, bank).score;
    assert.deepEqual(result, upstream(rows));
    assert.equal(result.used_outputs, count);
    assert.equal(result.calibration.queries, String(count));
    assert.ok(
      Math.abs(result.results.reduce((sum, r) => sum + r.probability, 0) - 1) <
        1e-12,
    );
  }
});

test("failed, unfinished, unknown and refused answers cannot increase the calibrated sample size", () => {
  const refused = { ...a[2], text: "无法提供数字。" };
  const failed = { ...a[1], status: "failed" };
  const running = { ...a[1], status: "running" };
  const unknown = { ...a[1], question_id: "unrecognized" };
  const result = scoreAccount(
    [a[0], failed, running, unknown, refused],
    questions,
    bank,
  );
  assert.deepEqual(result.score, upstream([a[0], refused]));
  assert.deepEqual(
    result.accepted,
    new Map([
      ["q1", true],
      ["q3", false],
    ]),
  );
  assert.equal(result.score.used_outputs, 1);
  assert.equal(scoreAccount([failed, running, unknown], questions, bank), null);
  assert.deepEqual(scoreAccount([refused], questions, bank), {
    score: null,
    accepted: new Map([["q3", false]]),
  });
});

test("interleaved accounts reuse question IDs without pooling each other’s answers", () => {
  const failedB = { ...b[1], status: "failed" };
  const cancelledC = { ...a[0], account_id: 30, status: "cancelled" };
  const rows = [a[0], b[0], cancelledC, failedB, a[1]];
  const reports = new Map(
    accountReports(rows, questions, bank).map((report) => [report.id, report]),
  );
  assert.equal(reports.size, 3);
  assert.deepEqual(reports.get(10).rows, [a[0], a[1]]);
  assert.deepEqual(reports.get(20).rows, [b[0], failedB]);
  assert.deepEqual(reports.get(10).result.score, upstream([a[0], a[1]]));
  assert.deepEqual(reports.get(20).result.score, upstream([b[0]]));
  assert.equal(reports.get(30).result, null);
  assert.notDeepEqual(
    reports.get(10).result.score.results,
    reports.get(20).result.score.results,
  );
  assert.ok(
    accountReports(rows, questions, null).every(
      (report) => report.result === null,
    ),
  );
});

test("parallel reports wait for the whole batch and match answers to the original account and question", () => {
  const thirdB = sample(20, "q3", 17);
  const failedB = { ...b[1], status: "failed", code: "TIMEOUT", text: "" };
  const rows = [a[2], thirdB, a[0], b[0], a[1], failedB];
  for (const status of ["waiting_capacity", "running", "done"]) {
    const pendingRows = rows.map((row) =>
      row === thirdB ? { ...row, status } : row,
    );
    const pending = accountReports(pendingRows, questions, bank, "running");
    assert.ok(pending.every(({ result }) => result === null));
    assert.deepEqual(pending.find(({ id }) => id === 10).rows, [
      a[2],
      a[0],
      a[1],
    ]);
  }

  const reports = new Map(
    accountReports(rows, questions, bank, "done").map((report) => [
      report.id,
      report,
    ]),
  );
  const first = reports.get(10);
  const second = reports.get(20);
  assert.deepEqual(first.result.score, upstream([a[2], a[0], a[1]]));
  assert.deepEqual(second.result.score, upstream([thirdB, b[0]]));
  assert.deepEqual([...first.result.accepted.keys()], ["q3", "q1", "q2"]);
  assert.deepEqual([...second.result.accepted.keys()], ["q3", "q1"]);
  assert.equal(
    `${first.result.score.used_outputs}/${first.rows.length}`,
    "3/3",
  );
  assert.equal(
    `${second.result.score.used_outputs}/${second.rows.length}`,
    "2/3",
  );
  assert.equal(
    second.rows.find((row) => row.question_id === "q2").code,
    "TIMEOUT",
  );
});
