import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { fileURLToPath } from "node:url";

const WORKFLOW_PATH = fileURLToPath(
  new URL("../../../.github/workflows/stability.yml", import.meta.url),
);
const workflow = readFileSync(WORKFLOW_PATH, "utf8").replace(/\r\n?/g, "\n");
const lines = workflow.split("\n");

function workflowJobs() {
  const jobsLine = lines.indexOf("jobs:");
  assert.notEqual(jobsLine, -1, "stability workflow must define jobs");

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

function occurrenceCount(source, value) {
  return source.split(value).length - 1;
}

function assertLine(source, expected) {
  assert.ok(
    source.split("\n").includes(expected),
    `missing exact workflow line: ${expected.trim()}`,
  );
}

const jobs = workflowJobs();
const validatedTarget = "${{ needs.validate-target.outputs.target_sha }}";
const artifactRunSuffix = "-${{ github.run_id }}-${{ github.run_attempt }}";

test("stability keeps the native job identities behind one cheap validator", () => {
  assert.deepEqual([...jobs.keys()], [
    "validate-target",
    "linux-integration-stability",
    "windows-integration-stability",
  ]);

  const validator = jobs.get("validate-target");
  assertLine(validator, "    name: Validate default-branch target");
  assertLine(validator, "    runs-on: ubuntu-latest");
  assertLine(validator, "    timeout-minutes: 5");
  assertLine(validator, "    permissions: {}");
  assert.equal(occurrenceCount(validator, "actions/checkout@"), 0);

  assertLine(
    jobs.get("linux-integration-stability"),
    "    name: Native integration stability (Linux)",
  );
  assertLine(
    jobs.get("windows-integration-stability"),
    "    name: Native integration stability (Windows)",
  );
});

test("validate-target proves the default ref and canonical SHA before exporting it", () => {
  const validator = jobs.get("validate-target");
  assertLine(
    validator,
    "      target_sha: ${{ steps.target.outputs.target_sha }}",
  );
  assertLine(validator, "          TARGET_REF: ${{ github.ref }}");
  assertLine(
    validator,
    "          TARGET_DEFAULT_REF: refs/heads/${{ github.event.repository.default_branch }}",
  );
  assertLine(validator, "          TARGET_SHA: ${{ github.sha }}");

  const refCheck = validator.indexOf(
    '[[ "$TARGET_REF" == "$TARGET_DEFAULT_REF" ]]',
  );
  const shaCheck = validator.indexOf(
    '[[ "$TARGET_SHA" =~ ^[0-9a-f]{40}$ ]]',
  );
  const exportTarget = validator.indexOf(
    `printf 'target_sha=%s\\n' "$TARGET_SHA" >> "$GITHUB_OUTPUT"`,
  );
  assert.ok(refCheck >= 0 && refCheck < shaCheck);
  assert.ok(shaCheck < exportTarget);
  assert.equal(occurrenceCount(validator, "${{ github.sha }}"), 1);
  assert.doesNotMatch(workflow, /github\.ref_protected/);
});

for (const contract of [
  {
    id: "linux-integration-stability",
    entrypoint: "bash scripts/ci/linux/integration.sh",
    artifactPrefix: "stability-integration-linux",
  },
  {
    id: "windows-integration-stability",
    entrypoint: "./scripts/ci/windows/integration.ps1",
    artifactPrefix: "stability-integration-windows",
  },
]) {
  test(`${contract.id} consumes only the validated target identity`, () => {
    const job = jobs.get(contract.id);
    assertLine(job, "    needs: validate-target");
    assert.equal(occurrenceCount(job, "uses: actions/checkout@"), 1);
    assertLine(job, `          ref: ${validatedTarget}`);
    assert.equal(occurrenceCount(job, "${{ github.sha }}"), 0);
    assertLine(job, `          --commit-sha "${validatedTarget}"`);
    assertLine(
      job,
      `          name: ${contract.artifactPrefix}-${validatedTarget}${artifactRunSuffix}`,
    );
  });

  test(`${contract.id} executes its integration entrypoint exactly once`, () => {
    const job = jobs.get(contract.id);
    assert.equal(
      occurrenceCount(job, "node scripts/ci/stability/result.mjs run"),
      1,
    );
    assert.equal(
      occurrenceCount(job, `--entrypoint "${contract.entrypoint}"`),
      1,
    );
    assert.equal(occurrenceCount(job, "--suite integration"), 1);
  });
}

test("the workflow has one structured integration execution per native OS", () => {
  assert.equal(
    occurrenceCount(workflow, "node scripts/ci/stability/result.mjs run"),
    2,
  );
  assert.equal(
    occurrenceCount(workflow, '--entrypoint "bash scripts/ci/linux/integration.sh"'),
    1,
  );
  assert.equal(
    occurrenceCount(
      workflow,
      '--entrypoint "./scripts/ci/windows/integration.ps1"',
    ),
    1,
  );
});
