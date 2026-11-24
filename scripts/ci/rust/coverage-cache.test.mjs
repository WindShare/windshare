import assert from "node:assert/strict";
import { existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import {
  buildCacheStamp,
  CACHE_DIR_TAG,
  commitCoverageCacheStamp,
  evaluateCacheReuse,
  parseCacheStamp,
  prepareCoverageCache,
  readCacheStamp,
  STAMP_FILENAME,
} from "./coverage-cache.mjs";

const RUSTC_1100 = "rustc 1.100.0 (0ef1bc386 2026-11-09)\r\nhost: x86_64-pc-windows-msvc\r\n";
const CARGO_1100 = "cargo 1.100.0 (3acc79c0c 2026-11-05)\r\n";
const LLVM_COV_091 = "cargo-llvm-cov 0.9.1\r\n";

function metadata(packages) {
  return {
    workspace_members: packages.map((pkg) => pkg.id),
    packages,
  };
}

function baseStamp(overrides = {}) {
  return buildCacheStamp({
    rustcVersion: RUSTC_1100,
    cargoVersion: CARGO_1100,
    llvmCovVersion: LLVM_COV_091,
    metadata: metadata([
      {
        id: "path+file:///workspace-a/crates/relay-core#0.1.0",
        name: "windshare-relay-core",
        targets: [
          { name: "windshare_relay_core", kind: ["lib"] },
          { name: "build-script-build", kind: ["custom-build"] },
        ],
      },
      {
        id: "path+file:///workspace-a/crates/relay-native#0.1.0",
        name: "windshare-relay-native",
        targets: [
          { name: "windshare_relay_native", kind: ["lib"] },
          { name: "relay-native", kind: ["bin"] },
          { name: "lifecycle_tests", kind: ["test"] },
        ],
      },
    ]),
    ...overrides,
  });
}

test("stamp normalizes versions, omits build scripts, and is checkout-path independent", () => {
  const fromPathA = baseStamp();
  const fromPathB = buildCacheStamp({
    rustcVersion: RUSTC_1100.replace(/\r\n/g, "\n"),
    cargoVersion: CARGO_1100.replace(/\r\n/g, "\n"),
    llvmCovVersion: LLVM_COV_091.trim(),
    metadata: metadata([
      {
        id: "path+file:///different-checkout/crates/relay-native#0.1.0",
        name: "windshare-relay-native",
        targets: [
          { name: "lifecycle_tests", kind: ["test"] },
          { name: "relay-native", kind: ["bin"] },
          { name: "windshare_relay_native", kind: ["lib"] },
        ],
      },
      {
        id: "path+file:///different-checkout/crates/relay-core#0.1.0",
        name: "windshare-relay-core",
        targets: [{ name: "windshare_relay_core", kind: ["lib"] }],
      },
    ]),
  });
  assert.deepEqual(fromPathA, fromPathB);
  assert.deepEqual(fromPathA.packages, ["windshare-relay-core", "windshare-relay-native"]);
  assert.deepEqual(fromPathA.targets, [
    "windshare-relay-core:windshare_relay_core:lib",
    "windshare-relay-native:lifecycle_tests:test",
    "windshare-relay-native:relay-native:bin",
    "windshare-relay-native:windshare_relay_native:lib",
  ]);
});

test("parseCacheStamp rejects malformed JSON, unknown schemas, and incomplete fields", () => {
  const valid = baseStamp();
  assert.deepEqual(parseCacheStamp(JSON.stringify(valid)), valid);
  assert.equal(parseCacheStamp("{not json"), null);
  assert.equal(parseCacheStamp(JSON.stringify({ ...valid, schemaVersion: 999 })), null);
  assert.equal(parseCacheStamp(JSON.stringify({ ...valid, rustcVersion: "   " })), null);
  assert.equal(parseCacheStamp(JSON.stringify({ ...valid, packages: [] })), null);
  assert.equal(parseCacheStamp(JSON.stringify({ ...valid, targets: [123] })), null);
});

test("evaluateCacheReuse resets legacy, tagless, toolchain-drifted, and shrinking caches", () => {
  const current = baseStamp();
  assert.equal(
    evaluateCacheReuse({ directoryExists: false, hasCacheDirTag: false, previousStamp: null, currentStamp: current }).action,
    "fresh",
  );
  assert.deepEqual(
    evaluateCacheReuse({ directoryExists: true, hasCacheDirTag: false, previousStamp: current, currentStamp: current }),
    { action: "reset", reason: "missing_cache_dir_tag", details: {} },
  );
  assert.deepEqual(
    evaluateCacheReuse({ directoryExists: true, hasCacheDirTag: true, previousStamp: null, currentStamp: current }),
    { action: "reset", reason: "missing_or_invalid_stamp", details: {} },
  );
  assert.deepEqual(
    evaluateCacheReuse({
      directoryExists: true,
      hasCacheDirTag: true,
      previousStamp: { ...current, rustcVersion: "rustc 1.99.0", cargoVersion: "cargo 1.99.0" },
      currentStamp: current,
    }),
    { action: "reset", reason: "toolchain_changed", details: { changedFields: ["rustcVersion", "cargoVersion"] } },
  );
  assert.deepEqual(
    evaluateCacheReuse({
      directoryExists: true,
      hasCacheDirTag: true,
      previousStamp: {
        ...current,
        packages: [...current.packages, "windshare-old-crate"],
        targets: [...current.targets, "windshare-relay-native:retired-bin:bin"],
      },
      currentStamp: current,
    }),
    {
      action: "reset",
      reason: "workspace_targets_removed",
      details: {
        removedPackages: ["windshare-old-crate"],
        removedTargets: ["windshare-relay-native:retired-bin:bin"],
      },
    },
  );
});

test("evaluateCacheReuse preserves dependency cache on matching and strictly growing workspaces", () => {
  const previous = baseStamp();
  assert.deepEqual(
    evaluateCacheReuse({ directoryExists: true, hasCacheDirTag: true, previousStamp: previous, currentStamp: previous }),
    { action: "reuse", reason: "stamp_matched", shouldUpdateStamp: false, details: { addedPackages: [], addedTargets: [] } },
  );
  const grown = {
    ...previous,
    packages: [...previous.packages, "windshare-new-crate"].sort(),
    targets: [...previous.targets, "windshare-new-crate:windshare_new_crate:lib"].sort(),
  };
  assert.deepEqual(
    evaluateCacheReuse({ directoryExists: true, hasCacheDirTag: true, previousStamp: previous, currentStamp: grown }),
    {
      action: "reuse",
      reason: "stamp_matched",
      shouldUpdateStamp: true,
      details: {
        addedPackages: ["windshare-new-crate"],
        addedTargets: ["windshare-new-crate:windshare_new_crate:lib"],
      },
    },
  );
});

test("prepareCoverageCache wipes polluted legacy cache, commits stamp after CACHEDIR.TAG, and reuses deps", () => {
  const root = mkdtempSync(join(tmpdir(), "windshare-coverage-cache-"));
  const coverageDirectory = join(root, ".target", "coverage");
  try {
    const current = baseStamp();
    // Simulate a polluted pre-stamp cache with stale pre-1.100 debug/deps binaries.
    mkdirSync(join(coverageDirectory, "debug", "deps"), { recursive: true });
    writeFileSync(join(coverageDirectory, CACHE_DIR_TAG), "Signature: 8a477f597d28d172789f06886806bc55\n");
    writeFileSync(join(coverageDirectory, "debug", "deps", "windshare_relay_cloudflare-stale.exe"), "stale");

    const logs = [];
    const first = prepareCoverageCache(coverageDirectory, current, { log: (line) => logs.push(JSON.parse(line)) });
    assert.equal(first.action, "reset");
    assert.equal(first.reason, "missing_or_invalid_stamp");
    assert.equal(existsSync(coverageDirectory), false);
    assert.equal(logs.length, 1);
    assert.equal(logs[0].event, "rust_coverage.cache_reset");
    assert.equal(logs[0].reason, "missing_or_invalid_stamp");

    // Without Cargo's CACHEDIR.TAG, stamp commit refuses to leave an untagged directory behind.
    mkdirSync(coverageDirectory, { recursive: true });
    assert.equal(commitCoverageCacheStamp(coverageDirectory, current), false);
    assert.equal(existsSync(join(coverageDirectory, STAMP_FILENAME)), false);

    // Once Cargo initializes CACHEDIR.TAG and builds deps, the stamp persists and subsequent runs reuse deps.
    writeFileSync(join(coverageDirectory, CACHE_DIR_TAG), "Signature: 8a477f597d28d172789f06886806bc55\n");
    mkdirSync(join(coverageDirectory, "debug", "build", "serde"), { recursive: true });
    writeFileSync(join(coverageDirectory, "debug", "build", "serde", "dep.rlib"), "cached-dep");
    assert.equal(commitCoverageCacheStamp(coverageDirectory, current), true);
    assert.deepEqual(readCacheStamp(coverageDirectory), current);

    const second = prepareCoverageCache(coverageDirectory, current, { log: (line) => logs.push(JSON.parse(line)) });
    assert.equal(second.action, "reuse");
    assert.equal(second.shouldUpdateStamp, false);
    assert.equal(logs.length, 1);
    assert.equal(existsSync(join(coverageDirectory, "debug", "build", "serde", "dep.rlib")), true);

    // Removing a target triggers a reset so orphaned binaries cannot pollute later reports.
    const shrunk = {
      ...current,
      targets: current.targets.filter((target) => !target.includes(":lifecycle_tests:")),
    };
    const third = prepareCoverageCache(coverageDirectory, shrunk, { log: (line) => logs.push(JSON.parse(line)) });
    assert.equal(third.action, "reset");
    assert.equal(third.reason, "workspace_targets_removed");
    assert.deepEqual(third.details.removedTargets, ["windshare-relay-native:lifecycle_tests:test"]);
    assert.equal(existsSync(coverageDirectory), false);
    assert.equal(logs.length, 2);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
