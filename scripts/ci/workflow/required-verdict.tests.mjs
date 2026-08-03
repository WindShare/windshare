import assert from "node:assert/strict";
import test from "node:test";

import {
  evaluateRequiredVerdict,
  VALIDATION_JOB_IDS,
} from "./required-verdict.mjs";

function results(conclusion) {
  return Object.fromEntries(VALIDATION_JOB_IDS.map((jobID) => [jobID, conclusion]));
}

test("the validation set matches the stable ordinary CI job contract", () => {
  assert.deepEqual(VALIDATION_JOB_IDS, [
    "linux-preflight",
    "windows-preflight",
    "linux-race",
    "windows-race",
    "linux-coverage",
    "golden-vectors",
    "core-release",
    "web",
    "browser-preflight",
    "linux-browser-process",
    "windows-browser-process",
    "windows-browser-smoke",
  ]);
});

test("required validation passes only when every validation job succeeds", () => {
  const verdict = evaluateRequiredVerdict({
    changesResult: "success",
    validationRequired: "true",
    validationResults: results("success"),
  });
  assert.equal(verdict.ok, true);
  assert.equal(verdict.expected_result, "success");
  assert.deepEqual(verdict.failures, []);
});

test("documentation-only validation passes only when every validation job skips", () => {
  const verdict = evaluateRequiredVerdict({
    changesResult: "success",
    validationRequired: "false",
    validationResults: results("skipped"),
  });
  assert.equal(verdict.ok, true);
  assert.equal(verdict.expected_result, "skipped");
});

test("a failed detector, missing output, or unexpected conclusion fails closed", () => {
  const failedChanges = evaluateRequiredVerdict({
    changesResult: "failure",
    validationRequired: "true",
    validationResults: results("success"),
  });
  assert.equal(failedChanges.ok, false);
  assert.match(failedChanges.failures.join("\n"), /changes concluded/);

  const missingOutput = evaluateRequiredVerdict({
    changesResult: "success",
    validationRequired: "",
    validationResults: results("skipped"),
  });
  assert.equal(missingOutput.ok, false);
  assert.match(missingOutput.failures.join("\n"), /validation_required/);

  const cancelledJobResults = results("success");
  cancelledJobResults["windows-race"] = "cancelled";
  const cancelledJob = evaluateRequiredVerdict({
    changesResult: "success",
    validationRequired: "true",
    validationResults: cancelledJobResults,
  });
  assert.equal(cancelledJob.ok, false);
  assert.match(cancelledJob.failures.join("\n"), /windows-race.*cancelled/);
});

test("missing, extra, and unexpectedly selected jobs fail closed", () => {
  const missingJobResults = results("skipped");
  delete missingJobResults.web;
  const missingJob = evaluateRequiredVerdict({
    changesResult: "success",
    validationRequired: "false",
    validationResults: missingJobResults,
  });
  assert.equal(missingJob.ok, false);
  assert.match(missingJob.failures.join("\n"), /web concluded undefined/);

  const extraJobResults = { ...results("success"), legacy: "success" };
  const extraJob = evaluateRequiredVerdict({
    changesResult: "success",
    validationRequired: "true",
    validationResults: extraJobResults,
  });
  assert.equal(extraJob.ok, false);
  assert.match(extraJob.failures.join("\n"), /unexpected validation jobs: legacy/);

  const selectedDocumentationJob = results("skipped");
  selectedDocumentationJob.web = "success";
  const selectedDocumentation = evaluateRequiredVerdict({
    changesResult: "success",
    validationRequired: "false",
    validationResults: selectedDocumentationJob,
  });
  assert.equal(selectedDocumentation.ok, false);
  assert.match(selectedDocumentation.failures.join("\n"), /web.*expected "skipped"/);
});
