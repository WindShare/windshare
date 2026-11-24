import { spawnSync } from "node:child_process";
import { readdirSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { buildCacheStamp, commitCoverageCacheStamp, prepareCoverageCache } from "./coverage-cache.mjs";
import { meetsMinimum, summarizeCoverage } from "./line-coverage.mjs";

const MAX_REPORT_BYTES = 32 * 1024 * 1024;
const [workspace, workspaceMinimumArgument, packageMinimumArgument, jobs] = process.argv.slice(2);
const workspaceMinimum = Number(workspaceMinimumArgument);
const packageMinimum = Number(packageMinimumArgument);
if (!workspace || [workspaceMinimum, packageMinimum].some((value) => !Number.isFinite(value) || value < 0 || value > 100)
    || !/^[1-9]\d*$/.test(jobs ?? "")) {
  throw new Error("Usage: check-coverage.mjs <workspace> <workspace-minimum-percent> <package-minimum-percent> <build-jobs>");
}

const cwd = resolve(workspace);
const coverageDirectory = join(cwd, ".target", "coverage");
const env = {
  ...process.env,
  CARGO_LLVM_COV_SETUP: "no",
  CARGO_LLVM_COV_DENY_WARNINGS: "1",
  CARGO_LLVM_COV_TARGET_DIR: coverageDirectory,
};

function run(command, args, capture = false) {
  const result = spawnSync(command, args, {
    cwd,
    env,
    encoding: "utf8",
    stdio: capture ? ["ignore", "pipe", "pipe"] : "inherit",
    maxBuffer: MAX_REPORT_BYTES,
  });
  if (result.error) throw result.error;
  return result;
}

function requireSuccess(result) {
  if (result.status !== 0) {
    if (result.stdout) process.stdout.write(result.stdout);
    if (result.stderr) process.stderr.write(result.stderr);
    process.exit(result.status ?? 1);
  }
  return result.stdout?.trim();
}

function cargo(args, capture = false) {
  return run("rustup", ["run", "stable", "cargo", "llvm-cov", ...args], capture);
}

function hasEmptyProfile() {
  const profiles = readdirSync(coverageDirectory).filter((name) => name.endsWith(".profdata"));
  if (profiles.length !== 1) return false;
  const targetLibdir = requireSuccess(run("rustup", ["run", "stable", "rustc", "--print", "target-libdir"], true));
  const executable = process.platform === "win32" ? "llvm-profdata.exe" : "llvm-profdata";
  const profile = run(join(dirname(targetLibdir), "bin", executable), ["show", join(coverageDirectory, profiles[0])], true);
  return profile.status === 0 && /^Total functions:\s+0\r?$/m.test(profile.stdout);
}

const metadata = JSON.parse(requireSuccess(run("rustup", [
  "run", "stable", "cargo", "metadata", "--no-deps", "--format-version", "1", "--locked",
], true)));
const rustcVersion = requireSuccess(run("rustup", ["run", "stable", "rustc", "-vV"], true));
const cargoVersion = requireSuccess(run("rustup", ["run", "stable", "cargo", "--version"], true));
const llvmCovVersion = requireSuccess(cargo(["--version"], true));
const cacheStamp = buildCacheStamp({ rustcVersion, cargoVersion, llvmCovVersion, metadata });

// `cargo llvm-cov clean --workspace` only cleans current workspace members in
// the active Cargo layout, leaving pre-1.100 debug/deps binaries or removed
// packages/targets behind for `cargo llvm-cov report` to scan. Reset on
// toolchain or target-removal drift; otherwise keep third-party dep builds.
const cacheDecision = prepareCoverageCache(coverageDirectory, cacheStamp);
if (cacheDecision.action === "reuse") {
  requireSuccess(cargo(["clean", "--workspace"]));
  if (cacheDecision.shouldUpdateStamp) {
    commitCoverageCacheStamp(coverageDirectory, cacheStamp);
  }
}

const testRun = cargo(["--workspace", "--locked", "--jobs", jobs, "--no-report"]);
if (cacheDecision.action !== "reuse") {
  commitCoverageCacheStamp(coverageDirectory, cacheStamp);
}
requireSuccess(testRun);
const report = cargo(["report", "--json", "--summary-only"], true);

// LLVM cannot export an entirely empty binary. Only accept that specific case
// after tests passed and LLVM independently confirms zero instrumented functions.
if (report.status !== 0 && report.stderr.includes("no coverage data found") && hasEmptyProfile()) {
  console.log(`Rust coverage: no coverable lines (minimums: workspace ${workspaceMinimum}%, each package ${packageMinimum}%).`);
  process.exit(0);
}

const coverage = JSON.parse(requireSuccess(report));
const summary = summarizeCoverage(coverage, metadata);

function checkMinimum(label, counts, minimum) {
  if (counts.total === 0) {
    console.log(`${label}: no coverable lines; minimum ${minimum}% applies when code exists.`);
  } else {
    console.log(`${label}: ${(100 * counts.covered / counts.total).toFixed(2)}% (${counts.covered}/${counts.total}); minimum ${minimum}%.`);
  }
  return meetsMinimum(counts, minimum);
}

let passed = checkMinimum("Rust workspace line coverage", summary.workspace, workspaceMinimum);
for (const pkg of summary.packages) {
  if (!checkMinimum(`Rust package ${pkg.name} line coverage`, pkg, packageMinimum)) passed = false;
}
process.exitCode = passed ? 0 : 1;
