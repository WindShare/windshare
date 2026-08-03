import { execFileSync } from "node:child_process";
import { appendFileSync } from "node:fs";
import { resolve } from "node:path";
import { TextDecoder } from "node:util";
import { pathToFileURL } from "node:url";

const COMMIT_SHA_PATTERN = /^[0-9a-f]{40}$/;
const ZERO_COMMIT_SHA_PATTERN = /^0{40}$/;
const MAX_DIFF_OUTPUT_BYTES = 16 * 1024 * 1024;
const OUTPUT_KEYS = Object.freeze([
  "target_sha",
  "diff_base_sha",
  "diff_head_sha",
  "range_kind",
  "documentation_only",
  "validation_required",
]);

function requireCommitSHA(value, label) {
  if (!COMMIT_SHA_PATTERN.test(value)) {
    throw new Error(`${label} must be an exact lowercase 40-hex commit SHA`);
  }
  return value;
}

function fullValidation(targetSHA, reason) {
  return {
    target_sha: targetSHA,
    diff_base_sha: targetSHA,
    diff_head_sha: targetSHA,
    range_kind: "all",
    documentation_only: "false",
    validation_required: "true",
    reason,
    changed_path_count: 0,
  };
}

export function selectChangeRange(context) {
  const targetSHA = requireCommitSHA(context.targetSHA, "target_sha");

  if (context.eventName === "workflow_dispatch") {
    return fullValidation(targetSHA, "manual-dispatch");
  }

  if (context.eventName === "pull_request") {
    if (
      !COMMIT_SHA_PATTERN.test(context.pullRequestBaseSHA) ||
      !COMMIT_SHA_PATTERN.test(context.pullRequestHeadSHA)
    ) {
      return fullValidation(targetSHA, "invalid-pull-request-range");
    }
    return {
      target_sha: targetSHA,
      diff_base_sha: context.pullRequestBaseSHA,
      diff_head_sha: context.pullRequestHeadSHA,
      range_kind: "three-dot",
    };
  }

  if (context.eventName === "push") {
    if (
      !COMMIT_SHA_PATTERN.test(context.pushBeforeSHA) ||
      ZERO_COMMIT_SHA_PATTERN.test(context.pushBeforeSHA)
    ) {
      return fullValidation(targetSHA, "invalid-push-range");
    }
    return {
      target_sha: targetSHA,
      diff_base_sha: context.pushBeforeSHA,
      diff_head_sha: targetSHA,
      range_kind: "two-dot",
    };
  }

  return fullValidation(targetSHA, "unrecognized-event");
}

function decodeNullTerminatedFields(output) {
  const bytes = Buffer.isBuffer(output) ? output : Buffer.from(output);
  if (bytes.length === 0) {
    return [];
  }
  if (bytes.at(-1) !== 0) {
    throw new Error("git name-status output is not NUL terminated");
  }

  const decoder = new TextDecoder("utf-8", { fatal: true });
  const fields = [];
  let fieldStart = 0;
  for (let index = 0; index < bytes.length; index += 1) {
    if (bytes[index] !== 0) {
      continue;
    }
    fields.push(decoder.decode(bytes.subarray(fieldStart, index)));
    fieldStart = index + 1;
  }
  return fields;
}

export function parseNameStatusZ(output) {
  const fields = decodeNullTerminatedFields(output);
  if (fields.length === 0) {
    return [];
  }

  const paths = [];
  for (let index = 0; index < fields.length; ) {
    const status = fields[index];
    index += 1;
    if (!/^[A-Z][0-9]*$/.test(status)) {
      throw new Error(`invalid git name-status token: ${JSON.stringify(status)}`);
    }

    const pathCount = status.startsWith("R") || status.startsWith("C") ? 2 : 1;
    if (index + pathCount > fields.length) {
      throw new Error(`git name-status record ${status} is missing a path`);
    }
    for (let pathIndex = 0; pathIndex < pathCount; pathIndex += 1) {
      const path = fields[index];
      index += 1;
      if (path.length === 0) {
        throw new Error(`git name-status record ${status} contains an empty path`);
      }
      paths.push(path);
    }
  }
  return paths;
}

export function isDocumentationPath(path) {
  return path.startsWith("docs/") || path.endsWith(".md");
}

export function deriveChangeDecision(context, loadChangedPaths) {
  const range = selectChangeRange(context);
  if (range.range_kind === "all") {
    return range;
  }

  let changedPaths;
  try {
    changedPaths = loadChangedPaths(range);
  } catch (error) {
    return fullValidation(
      range.target_sha,
      `range-unavailable: ${error instanceof Error ? error.message : String(error)}`,
    );
  }
  if (!Array.isArray(changedPaths) || changedPaths.length === 0) {
    return fullValidation(range.target_sha, "empty-or-invalid-diff");
  }

  const documentationOnly = changedPaths.every(isDocumentationPath);
  return {
    ...range,
    documentation_only: documentationOnly ? "true" : "false",
    validation_required: documentationOnly ? "false" : "true",
    reason: documentationOnly ? "documentation-only" : "product-change",
    changed_path_count: changedPaths.length,
  };
}

function assertCommitAvailable(commitSHA) {
  execFileSync("git", ["cat-file", "-e", `${commitSHA}^{commit}`], {
    stdio: ["ignore", "ignore", "pipe"],
  });
}

export function loadChangedPathsFromGit(range) {
  assertCommitAvailable(range.diff_base_sha);
  assertCommitAvailable(range.diff_head_sha);

  const separator = range.range_kind === "three-dot" ? "..." : "..";
  const revisionRange = `${range.diff_base_sha}${separator}${range.diff_head_sha}`;
  const output = execFileSync(
    "git",
    [
      "diff",
      "--no-ext-diff",
      "--no-textconv",
      "--name-status",
      "-z",
      "--find-renames",
      revisionRange,
      "--",
    ],
    {
      encoding: "buffer",
      maxBuffer: MAX_DIFF_OUTPUT_BYTES,
      stdio: ["ignore", "pipe", "pipe"],
    },
  );
  return parseNameStatusZ(output);
}

export function contractOutputs(decision) {
  return Object.fromEntries(OUTPUT_KEYS.map((key) => [key, decision[key]]));
}

function writeGitHubOutputs(outputs, outputPath) {
  if (!outputPath) {
    throw new Error("GITHUB_OUTPUT is required");
  }
  const lines = OUTPUT_KEYS.map((key) => `${key}=${outputs[key]}`).join("\n");
  appendFileSync(outputPath, `${lines}\n`, "utf8");
}

function contextFromEnvironment(environment) {
  return {
    eventName: environment.CI_EVENT_NAME ?? "",
    targetSHA: environment.CI_TARGET_SHA ?? "",
    pullRequestBaseSHA: environment.CI_PR_BASE_SHA ?? "",
    pullRequestHeadSHA: environment.CI_PR_HEAD_SHA ?? "",
    pushBeforeSHA: environment.CI_PUSH_BEFORE_SHA ?? "",
  };
}

function run() {
  const context = contextFromEnvironment(process.env);
  const decision = deriveChangeDecision(context, loadChangedPathsFromGit);
  const outputs = contractOutputs(decision);
  console.log(
    JSON.stringify({
      component: "ordinary-ci-change-detector",
      operation_id: `changes-${decision.target_sha}`,
      event_name: context.eventName,
      range_kind: decision.range_kind,
      changed_path_count: decision.changed_path_count,
      decision: decision.reason,
      validation_required: decision.validation_required,
    }),
  );
  writeGitHubOutputs(outputs, process.env.GITHUB_OUTPUT);
}

const invokedPath = process.argv[1] ? pathToFileURL(resolve(process.argv[1])).href : "";
if (invokedPath === import.meta.url) {
  try {
    run();
  } catch (error) {
    console.error(
      JSON.stringify({
        component: "ordinary-ci-change-detector",
        decision: "detector-failed",
        error: error instanceof Error ? error.message : String(error),
      }),
    );
    process.exitCode = 1;
  }
}
