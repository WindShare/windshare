import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

export const VALIDATION_JOB_IDS = Object.freeze([
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

export function evaluateRequiredVerdict({
  changesResult,
  validationRequired,
  validationResults,
}) {
  const failures = [];
  if (changesResult !== "success") {
    failures.push(`changes concluded ${JSON.stringify(changesResult)}, expected "success"`);
  }

  if (validationRequired !== "true" && validationRequired !== "false") {
    failures.push(
      `validation_required was ${JSON.stringify(validationRequired)}, expected "true" or "false"`,
    );
  }

  if (
    validationResults === null ||
    Array.isArray(validationResults) ||
    typeof validationResults !== "object"
  ) {
    failures.push("validation results must be an object");
    return { ok: false, expected_result: null, failures };
  }

  const unexpectedJobIDs = Object.keys(validationResults).filter(
    (jobID) => !VALIDATION_JOB_IDS.includes(jobID),
  );
  if (unexpectedJobIDs.length > 0) {
    failures.push(`unexpected validation jobs: ${unexpectedJobIDs.join(", ")}`);
  }

  const expectedResult =
    validationRequired === "true"
      ? "success"
      : validationRequired === "false"
        ? "skipped"
        : null;

  for (const jobID of VALIDATION_JOB_IDS) {
    const actualResult = validationResults[jobID];
    if (actualResult !== expectedResult) {
      failures.push(
        `${jobID} concluded ${JSON.stringify(actualResult)}, expected ${JSON.stringify(expectedResult)}`,
      );
    }
  }

  return {
    ok: failures.length === 0,
    expected_result: expectedResult,
    failures,
  };
}

function parseValidationResults(serialized) {
  if (!serialized) {
    throw new Error("VALIDATION_RESULTS_JSON is required");
  }
  return JSON.parse(serialized);
}

function run() {
  const verdict = evaluateRequiredVerdict({
    changesResult: process.env.CHANGES_RESULT ?? "",
    validationRequired: process.env.VALIDATION_REQUIRED ?? "",
    validationResults: parseValidationResults(process.env.VALIDATION_RESULTS_JSON),
  });
  console.log(
    JSON.stringify({
      component: "ordinary-ci-required-verdict",
      operation_id: `required-verdict-${process.env.GITHUB_SHA ?? "unknown"}`,
      validation_required: process.env.VALIDATION_REQUIRED ?? "",
      expected_result: verdict.expected_result,
      verdict: verdict.ok ? "pass" : "fail",
      failures: verdict.failures,
    }),
  );
  if (!verdict.ok) {
    process.exitCode = 1;
  }
}

const invokedPath = process.argv[1] ? pathToFileURL(resolve(process.argv[1])).href : "";
if (invokedPath === import.meta.url) {
  try {
    run();
  } catch (error) {
    console.error(
      JSON.stringify({
        component: "ordinary-ci-required-verdict",
        verdict: "verdict-failed",
        error: error instanceof Error ? error.message : String(error),
      }),
    );
    process.exitCode = 1;
  }
}
