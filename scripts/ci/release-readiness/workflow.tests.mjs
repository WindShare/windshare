import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import test from "node:test";
import { fileURLToPath } from "node:url";

const WORKFLOW_PATH = fileURLToPath(
  new URL("../../../.github/workflows/release-readiness.yml", import.meta.url),
);
const CURRENT_COMMIT_WORKFLOW_PATH = fileURLToPath(
  new URL("../../../.github/workflows/current-commit.yml", import.meta.url),
);
const workflow = readFileSync(WORKFLOW_PATH, "utf8").replace(/\r\n?/g, "\n");
const lines = workflow.split("\n");

function occurrenceCount(source, value) {
  return source.split(value).length - 1;
}

function assertLine(source, expected) {
  assert.ok(
    source.split("\n").includes(expected),
    "missing exact workflow line: " + expected.trim(),
  );
}

function workflowJobs() {
  const jobsLine = lines.indexOf("jobs:");
  assert.notEqual(jobsLine, -1, "release workflow must define jobs");

  const starts = [];
  for (let index = jobsLine + 1; index < lines.length; index += 1) {
    const match = /^  ([a-z0-9-]+):$/.exec(lines[index]);
    if (match !== null) starts.push({ id: match[1], index });
  }

  return new Map(
    starts.map(({ id, index }, position) => {
      const end = starts[position + 1]?.index ?? lines.length;
      return [id, lines.slice(index, end).join("\n")];
    }),
  );
}

function jobNeeds(job) {
  const jobLines = job.split("\n");
  const index = jobLines.findIndex((line) => line.startsWith("    needs:"));
  if (index === -1) return [];

  const scalar = /^    needs: ([a-z0-9-]+)$/.exec(jobLines[index]);
  if (scalar !== null) return [scalar[1]];

  const needs = [];
  for (let cursor = index + 1; cursor < jobLines.length; cursor += 1) {
    const match = /^      - ([a-z0-9-]+)$/.exec(jobLines[cursor]);
    if (match === null) break;
    needs.push(match[1]);
  }
  return needs;
}

function jobPermissions(job) {
  const jobLines = job.split("\n");
  const empty = jobLines.indexOf("    permissions: {}");
  if (empty !== -1) return {};

  const index = jobLines.indexOf("    permissions:");
  assert.notEqual(index, -1, "every release job must declare permissions");

  const permissions = {};
  for (let cursor = index + 1; cursor < jobLines.length; cursor += 1) {
    const match = /^      ([a-z-]+): ([a-z]+)$/.exec(jobLines[cursor]);
    if (match === null) break;
    permissions[match[1]] = match[2];
  }
  return permissions;
}

function jobOutputKeys(job) {
  const jobLines = job.split("\n");
  const index = jobLines.indexOf("    outputs:");
  if (index === -1) return [];

  const outputs = [];
  for (let cursor = index + 1; cursor < jobLines.length; cursor += 1) {
    const match = /^      ([a-z0-9_]+):/.exec(jobLines[cursor]);
    if (match === null) break;
    outputs.push(match[1]);
  }
  return outputs;
}

function namedStep(job, name) {
  const jobLines = job.split("\n");
  const marker = "      - name: " + name;
  const index = jobLines.indexOf(marker);
  assert.notEqual(index, -1, "missing step: " + name);

  let end = jobLines.length;
  for (let cursor = index + 1; cursor < jobLines.length; cursor += 1) {
    if (/^      - /.test(jobLines[cursor])) {
      end = cursor;
      break;
    }
  }
  return jobLines.slice(index, end).join("\n");
}

const jobs = workflowJobs();
const targetSHA = "${{ needs.validate-target.outputs.target_sha }}";
const defaultBranch = "${{ needs.validate-target.outputs.default_branch }}";
const selectionKeys = [
  "ci_run_id",
  "ci_run_attempt",
  "browser_run_id",
  "browser_run_attempt",
  "browser_artifact_id",
  "stability_run_id",
  "stability_run_attempt",
  "stability_linux_artifact_id",
  "stability_windows_artifact_id",
];

test("release readiness is a six-job manual evidence consumer", () => {
  assert.deepEqual([...jobs.keys()], [
    "validate-target",
    "resolve-evidence",
    "verify-evidence",
    "stability-history",
    "core-release",
    "release-verdict",
  ]);

  const header = workflow.slice(0, workflow.indexOf("jobs:"));
  assert.match(
    header,
    /on:\n  workflow_dispatch:\n\npermissions: {}\n\n$/,
  );
  assert.equal(occurrenceCount(header, "workflow_dispatch:"), 1);
  assert.doesNotMatch(
    header,
    /pull_request:|push:|schedule:|workflow_call:/,
  );

  assert.equal(occurrenceCount(workflow, "    runs-on: ubuntu-latest"), 6);
  assert.equal(existsSync(CURRENT_COMMIT_WORKFLOW_PATH), false);
  assert.doesNotMatch(workflow, /current-commit/i);
  assert.doesNotMatch(workflow, /self-hosted/);
  assert.doesNotMatch(workflow, /^\s+environment:/m);
  assert.doesNotMatch(workflow, /id-token:\s*write/);
  assert.doesNotMatch(workflow, /^\s+uses:\s*\.\/\.github\/workflows\//m);

  const actionReferences = [
    ...workflow.matchAll(/^\s+(?:- )?uses: ([^\s]+)$/gm),
  ].map((match) => match[1]);
  assert.deepEqual([...new Set(actionReferences)].sort(), [
    "actions/checkout@v7",
    "actions/download-artifact@v8",
    "actions/setup-go@v6",
    "actions/setup-node@v6",
    "actions/upload-artifact@v7",
  ]);
});

test("permissions and needs keep metadata access narrow and evidence proofs parallel", () => {
  assert.deepEqual(jobPermissions(jobs.get("validate-target")), {});
  assert.deepEqual(jobPermissions(jobs.get("resolve-evidence")), {
    actions: "read",
    contents: "read",
  });
  assert.deepEqual(jobPermissions(jobs.get("verify-evidence")), {
    actions: "read",
    contents: "read",
  });
  assert.deepEqual(jobPermissions(jobs.get("stability-history")), {
    actions: "read",
    contents: "read",
  });
  assert.deepEqual(jobPermissions(jobs.get("core-release")), {
    contents: "read",
  });
  assert.deepEqual(jobPermissions(jobs.get("release-verdict")), {});

  assert.deepEqual(jobNeeds(jobs.get("validate-target")), []);
  assert.deepEqual(jobNeeds(jobs.get("resolve-evidence")), ["validate-target"]);
  assert.deepEqual(jobNeeds(jobs.get("verify-evidence")), [
    "validate-target",
    "resolve-evidence",
  ]);
  assert.deepEqual(jobNeeds(jobs.get("stability-history")), ["validate-target"]);
  assert.deepEqual(jobNeeds(jobs.get("core-release")), ["validate-target"]);
  assert.deepEqual(jobNeeds(jobs.get("release-verdict")), [
    "validate-target",
    "resolve-evidence",
    "verify-evidence",
    "stability-history",
    "core-release",
  ]);
});

test("target outputs exist only after protected default-branch and SHA validation", () => {
  const validator = jobs.get("validate-target");
  assert.deepEqual(jobOutputKeys(validator), ["target_sha", "default_branch"]);
  assert.equal(occurrenceCount(validator, "actions/checkout@"), 0);
  assertLine(
    validator,
    "      target_sha: ${{ steps.target.outputs.target_sha }}",
  );
  assertLine(
    validator,
    "      default_branch: ${{ steps.target.outputs.default_branch }}",
  );

  const eventCheck = validator.indexOf(
    '[[ "$TARGET_EVENT" == "workflow_dispatch" ]]',
  );
  const refCheck = validator.indexOf(
    '[[ "$TARGET_REF" == "refs/heads/$TARGET_DEFAULT_BRANCH" ]]',
  );
  const protectedCheck = validator.indexOf(
    '[[ "$TARGET_REF_PROTECTED" == "true" ]]',
  );
  const shaCheck = validator.indexOf(
    '[[ "$TARGET_SHA" =~ ^[0-9a-f]{40}$ ]]',
  );
  const exportTarget = validator.indexOf(
    "printf 'target_sha=%s\\ndefault_branch=%s\\n'",
  );
  assert.ok(eventCheck >= 0 && eventCheck < refCheck);
  assert.ok(refCheck < protectedCheck);
  assert.ok(protectedCheck < shaCheck);
  assert.ok(shaCheck < exportTarget);
  assert.equal(occurrenceCount(validator, "${{ github.sha }}"), 1);
});

test("every repository checkout is explicitly bound to the validated target", () => {
  const checkoutJobs = [
    "resolve-evidence",
    "verify-evidence",
    "stability-history",
    "core-release",
  ];
  assert.equal(occurrenceCount(workflow, "uses: actions/checkout@v7"), 4);
  assert.equal(occurrenceCount(workflow, "          ref: " + targetSHA), 4);
  assert.equal(occurrenceCount(workflow, "ref: ${{ github.sha }}"), 0);

  for (const jobID of checkoutJobs) {
    const job = jobs.get(jobID);
    assert.equal(occurrenceCount(job, "uses: actions/checkout@v7"), 1);
    assertLine(job, "          persist-credentials: false");
    assertLine(job, "          ref: " + targetSHA);
  }
  assert.equal(occurrenceCount(jobs.get("release-verdict"), "actions/checkout@"), 0);
});

test("the second resolver pass rejects drift from all nine scalar selections", () => {
  const resolver = jobs.get("resolve-evidence");
  const verifier = jobs.get("verify-evidence");

  assert.deepEqual(jobOutputKeys(resolver), selectionKeys);
  assert.equal(
    occurrenceCount(workflow, "node scripts/ci/release-readiness/resolver.mjs"),
    2,
  );
  assert.equal(occurrenceCount(workflow, "--github-output"), 1);
  assertLine(resolver, "          DEFAULT_BRANCH: " + defaultBranch);
  assertLine(resolver, "          TARGET_SHA: " + targetSHA);

  for (const key of selectionKeys) {
    const expression =
      "${{ needs.resolve-evidence.outputs." + key + " }}";
    const flag = "--expect-" + key.replaceAll("_", "-");
    assert.equal(
      occurrenceCount(verifier, flag + ' "' + expression + '"'),
      1,
      "missing drift guard for " + key,
    );
  }

  assert.match(
    verifier,
    /--output "\$RESOLUTION_PATH"[\s\S]*--expect-ci-run-id/,
  );
  assert.equal(
    occurrenceCount(
      verifier,
      "          RESOLUTION_PATH: ${{ runner.temp }}/release-readiness-resolution.json",
    ),
    2,
  );
  assert.doesNotMatch(verifier, /RESOLUTION_PATH: .*test-results/);
  assert.equal(occurrenceCount(resolver, "actions/upload-artifact@"), 0);
  assert.equal(occurrenceCount(verifier, "actions/upload-artifact@"), 0);
});

test("selected artifacts retain their producer-owned verification formats", () => {
  const verifier = jobs.get("verify-evidence");
  const browserDownload = namedStep(
    verifier,
    "download the selected final browser artifact",
  );
  assertLine(
    browserDownload,
    "          artifact-ids: ${{ needs.resolve-evidence.outputs.browser_artifact_id }}",
  );
  assertLine(
    browserDownload,
    "          github-token: ${{ secrets.GITHUB_TOKEN }}",
  );
  assertLine(browserDownload, "          repository: ${{ github.repository }}");
  assertLine(
    browserDownload,
    "          run-id: ${{ needs.resolve-evidence.outputs.browser_run_id }}",
  );
  assertLine(
    browserDownload,
    "          path: ${{ github.workspace }}/test-results",
  );
  assertLine(browserDownload, "          digest-mismatch: error");
  assert.doesNotMatch(browserDownload, /^          (name|pattern):/m);
  assert.equal(occurrenceCount(workflow, "actions/download-artifact@v8"), 1);

  const rawDownloads = namedStep(
    verifier,
    "download the selected raw stability archives",
  );
  assert.match(
    rawDownloads,
    /repos\/\$GITHUB_REPOSITORY\/actions\/artifacts/,
  );
  assert.match(rawDownloads, /\$artifact_api\/\$artifact_id\/zip/);
  assert.match(rawDownloads, /maximum_stability_archive_bytes=65536/);
  assert.match(rawDownloads, /stability-linux\.zip/);
  assert.match(rawDownloads, /stability-windows\.zip/);
  assert.doesNotMatch(rawDownloads, /unzip|Expand-Archive|Compress-Archive/);

  const contentVerification = namedStep(
    verifier,
    "verify downloaded evidence without repository or identity tokens",
  );
  assertLine(contentVerification, "          GITHUB_TOKEN: ''");
  assertLine(contentVerification, "          GH_TOKEN: ''");
  assertLine(contentVerification, "          ACTIONS_RUNTIME_TOKEN: ''");
  assertLine(contentVerification, "          ACTIONS_ID_TOKEN_REQUEST_URL: ''");
  assertLine(contentVerification, "          ACTIONS_ID_TOKEN_REQUEST_TOKEN: ''");
  assert.doesNotMatch(contentVerification, /secrets\.GITHUB_TOKEN/);
  for (const flag of [
    "--repository",
    "--target-sha",
    "--resolution",
    "--browser-root",
    "--stability-linux-archive",
    "--stability-windows-archive",
  ]) {
    assert.equal(occurrenceCount(contentVerification, flag), 1);
  }
  assert.match(contentVerification, /node --experimental-strip-types/);
  assert.doesNotMatch(verifier, /ci[_-]artifact/i);
});

test("history and core artifact checks remain independent exact-target proofs", () => {
  const history = jobs.get("stability-history");
  assertLine(history, "        continue-on-error: true");
  assert.match(history, /--target-sha "\$TARGET_SHA"/);
  assertLine(
    history,
    "          name: stability-release-verdict-" +
      targetSHA +
      "-${{ github.run_id }}-${{ github.run_attempt }}",
  );
  assertLine(history, "        if: ${{ always() }}");
  assert.match(
    history,
    /if: \$\{\{ always\(\) && steps\.reducer\.outcome != 'success' \}\}/,
  );

  const coreRelease = jobs.get("core-release");
  assert.match(coreRelease, /git rev-parse HEAD/);
  assert.match(coreRelease, /git status --porcelain=v1 --untracked-files=all/);
  assertLine(coreRelease, "          go-version-file: core/go.mod");
  assertLine(coreRelease, "          cache: false");
  assert.equal(occurrenceCount(coreRelease, "run: make core-release"), 1);
  assert.equal(occurrenceCount(coreRelease, "download-artifact"), 0);
  assert.equal(occurrenceCount(coreRelease, "upload-artifact"), 0);
});

test("the always-running verdict rejects every failed job or missing selection", () => {
  const verdict = jobs.get("release-verdict");
  assertLine(verdict, "    name: Release Readiness Verdict");
  assertLine(verdict, "    if: ${{ always() }}");
  assert.match(verdict, /require_success validate-target/);
  assert.match(verdict, /require_success resolve-evidence/);
  assert.match(verdict, /require_success verify-evidence/);
  assert.match(verdict, /require_success stability-history/);
  assert.match(verdict, /require_success core-release/);
  assert.match(verdict, /\^\[0-9a-f\]\{40\}\$/);
  assert.match(verdict, /\^\[1-9\]\[0-9\]\*\$/);

  for (const key of selectionKeys) {
    assert.match(
      verdict,
      new RegExp(
        "needs\\.resolve-evidence\\.outputs\\." + key.replaceAll("_", "\\_"),
      ),
    );
  }
});
