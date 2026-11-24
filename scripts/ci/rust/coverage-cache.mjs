import { existsSync, readFileSync, renameSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";

export const STAMP_FILENAME = ".windshare-coverage-stamp.json";
export const STAMP_SCHEMA_VERSION = 1;
export const CACHE_DIR_TAG = "CACHEDIR.TAG";

const BUILD_SCRIPT_KIND = "custom-build";
const VERSION_FIELDS = ["rustcVersion", "cargoVersion", "llvmCovVersion"];
// NTFS directory wipes can encounter brief indexer/antivirus locks on large target trees.
const REMOVE_RETRY_OPTIONS = { maxRetries: 5, retryDelay: 100 };

function normalizeVersion(output, label) {
  if (typeof output !== "string") throw new Error(`Missing ${label} version output`);
  const normalized = output.replace(/\r\n/g, "\n").trim();
  if (!normalized) throw new Error(`Empty ${label} version output`);
  return normalized;
}

function isNonEmptyStringArray(value) {
  return Array.isArray(value) && value.every((item) => typeof item === "string" && item.length > 0);
}

export function collectWorkspaceInventory(metadata) {
  if (!Array.isArray(metadata?.packages) || !Array.isArray(metadata?.workspace_members)
      || metadata.workspace_members.length === 0) {
    throw new Error("Missing Cargo workspace packages");
  }
  const byId = new Map(metadata.packages.map((pkg) => [pkg.id, pkg]));
  const packages = new Set();
  const targets = new Set();
  for (const id of metadata.workspace_members) {
    const pkg = byId.get(id);
    if (!pkg || typeof pkg.name !== "string" || !pkg.name || !Array.isArray(pkg.targets)) {
      throw new Error(`Missing Cargo workspace package metadata: ${id}`);
    }
    packages.add(pkg.name);
    for (const target of pkg.targets) {
      if (!target || typeof target.name !== "string" || !target.name || !Array.isArray(target.kind)) {
        throw new Error(`Invalid Cargo target metadata in package ${pkg.name}`);
      }
      // Build scripts are excluded from coverage reports by default, so adding or
      // removing a build.rs does not leave stale instrumented binaries behind.
      const kinds = target.kind.filter((kind) => kind !== BUILD_SCRIPT_KIND);
      if (kinds.length === 0) continue;
      targets.add(`${pkg.name}:${target.name}:${[...kinds].sort().join("+")}`);
    }
  }
  return {
    packages: [...packages].sort(),
    targets: [...targets].sort(),
  };
}

export function buildCacheStamp({ rustcVersion, cargoVersion, llvmCovVersion, metadata }) {
  const { packages, targets } = collectWorkspaceInventory(metadata);
  return {
    schemaVersion: STAMP_SCHEMA_VERSION,
    rustcVersion: normalizeVersion(rustcVersion, "rustc"),
    cargoVersion: normalizeVersion(cargoVersion, "cargo"),
    llvmCovVersion: normalizeVersion(llvmCovVersion, "cargo-llvm-cov"),
    packages,
    targets,
  };
}

export function parseCacheStamp(raw) {
  if (typeof raw !== "string") return null;
  let parsed;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return null;
  }
  if (!parsed || typeof parsed !== "object" || parsed.schemaVersion !== STAMP_SCHEMA_VERSION) {
    return null;
  }
  for (const field of VERSION_FIELDS) {
    if (typeof parsed[field] !== "string" || !parsed[field].trim()) return null;
  }
  if (!isNonEmptyStringArray(parsed.packages) || parsed.packages.length === 0
      || !isNonEmptyStringArray(parsed.targets)) {
    return null;
  }
  return {
    schemaVersion: STAMP_SCHEMA_VERSION,
    rustcVersion: parsed.rustcVersion,
    cargoVersion: parsed.cargoVersion,
    llvmCovVersion: parsed.llvmCovVersion,
    packages: [...new Set(parsed.packages)].sort(),
    targets: [...new Set(parsed.targets)].sort(),
  };
}

function difference(left, right) {
  const rightSet = new Set(right);
  return left.filter((item) => !rightSet.has(item));
}

export function evaluateCacheReuse({ directoryExists, hasCacheDirTag, previousStamp, currentStamp }) {
  if (!directoryExists) {
    return { action: "fresh", reason: "cache_absent", details: {} };
  }
  if (!hasCacheDirTag) {
    return { action: "reset", reason: "missing_cache_dir_tag", details: {} };
  }
  if (!previousStamp) {
    return { action: "reset", reason: "missing_or_invalid_stamp", details: {} };
  }
  const changedFields = VERSION_FIELDS.filter((field) => previousStamp[field] !== currentStamp[field]);
  if (changedFields.length > 0) {
    return { action: "reset", reason: "toolchain_changed", details: { changedFields } };
  }
  const removedPackages = difference(previousStamp.packages, currentStamp.packages);
  const removedTargets = difference(previousStamp.targets, currentStamp.targets);
  if (removedPackages.length > 0 || removedTargets.length > 0) {
    return {
      action: "reset",
      reason: "workspace_targets_removed",
      details: { removedPackages, removedTargets },
    };
  }
  const addedPackages = difference(currentStamp.packages, previousStamp.packages);
  const addedTargets = difference(currentStamp.targets, previousStamp.targets);
  return {
    action: "reuse",
    reason: "stamp_matched",
    shouldUpdateStamp: addedPackages.length > 0 || addedTargets.length > 0,
    details: { addedPackages, addedTargets },
  };
}

export function readCacheStamp(coverageDirectory) {
  const stampPath = join(coverageDirectory, STAMP_FILENAME);
  if (!existsSync(stampPath)) return null;
  try {
    return parseCacheStamp(readFileSync(stampPath, "utf8"));
  } catch {
    return null;
  }
}

export function prepareCoverageCache(coverageDirectory, currentStamp, { log = console.log } = {}) {
  const directoryExists = existsSync(coverageDirectory);
  const hasCacheDirTag = directoryExists && existsSync(join(coverageDirectory, CACHE_DIR_TAG));
  const previousStamp = directoryExists ? readCacheStamp(coverageDirectory) : null;
  const decision = evaluateCacheReuse({ directoryExists, hasCacheDirTag, previousStamp, currentStamp });
  if (decision.action === "reset") {
    log(JSON.stringify({
      event: "rust_coverage.cache_reset",
      coverageDirectory,
      reason: decision.reason,
      ...decision.details,
    }));
    // Remove the stamp first so an interrupted wipe cannot leave a matching
    // stamp over partially deleted artifacts.
    rmSync(join(coverageDirectory, STAMP_FILENAME), { force: true, ...REMOVE_RETRY_OPTIONS });
    rmSync(coverageDirectory, { recursive: true, force: true, ...REMOVE_RETRY_OPTIONS });
  }
  return decision;
}

export function commitCoverageCacheStamp(coverageDirectory, currentStamp) {
  // Never create a stamp inside a directory without Cargo's CACHEDIR.TAG;
  // otherwise a later `cargo clean` refuses to touch the directory.
  if (!existsSync(join(coverageDirectory, CACHE_DIR_TAG))) return false;
  const stampPath = join(coverageDirectory, STAMP_FILENAME);
  const tempPath = `${stampPath}.${process.pid}.tmp`;
  writeFileSync(tempPath, `${JSON.stringify(currentStamp, null, 2)}\n`, "utf8");
  renameSync(tempPath, stampPath);
  return true;
}
