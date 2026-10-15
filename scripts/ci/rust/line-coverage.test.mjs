import assert from "node:assert/strict";
import { join, resolve } from "node:path";
import test from "node:test";
import { meetsMinimum, summarizeCoverage } from "./line-coverage.mjs";

const root = resolve("coverage-fixture");

function metadata(...names) {
  return {
    workspace_members: names,
    packages: names.map((name) => ({ id: name, name, manifest_path: join(root, name, "Cargo.toml") })),
  };
}

function report(...files) {
  return {
    data: [{
      files: files.map(([filename, count, covered]) => ({ filename, summary: { lines: { count, covered } } })),
      totals: { lines: {
        count: files.reduce((sum, [, count]) => sum + count, 0),
        covered: files.reduce((sum, [, , covered]) => sum + covered, 0),
      } },
    }],
  };
}

test("a covered large package cannot mask another package below its floor", () => {
  const summary = summarizeCoverage(report(
    [join(root, "core", "src", "lib.rs"), 100, 100],
    [join(root, "adapter", "src", "lib.rs"), 20, 8],
  ), metadata("core", "adapter"));
  assert.equal(meetsMinimum(summary.workspace, 60), true);
  assert.equal(meetsMinimum(summary.packages[1], 50), false);
});

test("package coverage is weighted by lines across files, without per-file floors", () => {
  const summary = summarizeCoverage(report(
    [join(root, "core", "src", "lib.rs"), 90, 50],
    [join(root, "core", "src", "recovery.rs"), 10, 0],
  ), metadata("core"));
  assert.deepEqual(summary.packages, [{ name: "core", total: 100, covered: 50 }]);
  assert.equal(meetsMinimum(summary.packages[0], 50), true);
  assert.equal(meetsMinimum(summary.workspace, 60), false);
  assert.equal(meetsMinimum({ total: 100000, covered: 49999 }, 50), false);
});

test("nested workspace packages and similarly prefixed siblings retain separate ownership", () => {
  const summary = summarizeCoverage(report(
    [join(root, "src", "lib.rs"), 10, 10],
    [join(root, "a", "src", "lib.rs"), 10, 5],
    [join(root, "ab", "src", "lib.rs"), 10, 1],
  ), metadata(".", "a", "ab"));
  assert.deepEqual(summary.packages, [
    { name: ".", total: 10, covered: 10 },
    { name: "a", total: 10, covered: 5 },
    { name: "ab", total: 10, covered: 1 },
  ]);
});

test("empty packages are exempt but untested production lines fail", () => {
  const summary = summarizeCoverage(report(
    [join(root, "core", "src", "lib.rs"), 10, 0],
  ), metadata("core", "empty"));
  assert.equal(meetsMinimum(summary.packages[0], 50), false);
  assert.deepEqual(summary.packages[1], { name: "empty", total: 0, covered: 0 });
  assert.equal(meetsMinimum(summary.packages[1], 50), true);
});

test("unowned source and incomplete reports fail instead of bypassing package checks", () => {
  assert.throws(() => summarizeCoverage(report(
    [join(root, "core-other", "src", "lib.rs"), 10, 10],
  ), metadata("core")), /outside all workspace packages/);
  const incomplete = report([join(root, "core", "src", "lib.rs"), 10, 10]);
  incomplete.data[0].files = [];
  assert.throws(() => summarizeCoverage(incomplete, metadata("core")), /does not match workspace totals/);
  assert.throws(() => summarizeCoverage(report(
    [join(root, "core", "src", "lib.rs"), 10, 11],
  ), metadata("core")), /Invalid LLVM line coverage counts/);
});
