import { dirname, isAbsolute, relative, sep } from "node:path";

function readCounts(lines) {
  if (!lines || !Number.isSafeInteger(lines.count) || !Number.isSafeInteger(lines.covered)
      || lines.count < 0 || lines.covered < 0 || lines.covered > lines.count) {
    throw new Error("Invalid LLVM line coverage counts");
  }
  return { total: lines.count, covered: lines.covered };
}

function addCounts(destination, counts) {
  destination.total += counts.total;
  destination.covered += counts.covered;
}

function ownsFile(root, filename) {
  const path = relative(root, filename);
  return path !== ".." && !path.startsWith(`..${sep}`) && !isAbsolute(path);
}

export function summarizeCoverage(report, metadata) {
  if (!Array.isArray(report.data) || report.data.length === 0) throw new Error("Missing LLVM coverage data");
  if (!Array.isArray(metadata.packages) || !Array.isArray(metadata.workspace_members) || metadata.workspace_members.length === 0) {
    throw new Error("Missing Cargo workspace packages");
  }
  const byId = new Map(metadata.packages.map((pkg) => [pkg.id, pkg]));
  const packages = metadata.workspace_members.map((id) => {
    const pkg = byId.get(id);
    if (!pkg || !isAbsolute(pkg.manifest_path)) throw new Error(`Missing Cargo package manifest: ${id}`);
    return { name: pkg.name, root: dirname(pkg.manifest_path), total: 0, covered: 0 };
  });
  // Nested workspace packages own their sources even when a parent package's
  // manifest sits at the workspace root. Directory prefixes alone are ambiguous.
  const ownership = [...packages].sort((a, b) => b.root.length - a.root.length);
  const workspace = { total: 0, covered: 0 };
  for (const entry of report.data) {
    const reported = readCounts(entry.totals?.lines);
    if (!Array.isArray(entry.files)) throw new Error("Missing LLVM per-file coverage");
    const counted = { total: 0, covered: 0 };
    for (const file of entry.files) {
      if (!isAbsolute(file.filename)) throw new Error(`Coverage source path must be absolute: ${file.filename}`);
      const owner = ownership.find((pkg) => ownsFile(pkg.root, file.filename));
      // Failing on unowned source prevents custom target paths from silently
      // escaping a package floor; measured sources must live inside their package.
      if (!owner) throw new Error(`Coverage source is outside all workspace packages: ${file.filename}`);
      const counts = readCounts(file.summary?.lines);
      addCounts(owner, counts);
      addCounts(counted, counts);
    }
    if (counted.total !== reported.total || counted.covered !== reported.covered) {
      throw new Error("LLVM file coverage does not match workspace totals");
    }
    addCounts(workspace, counted);
  }
  return { workspace, packages: packages.map(({ name, total, covered }) => ({ name, total, covered })) };
}

export function meetsMinimum({ covered, total }, minimum) {
  // Compare counts before rounding percentages for display, especially at 50%.
  return total === 0 || 100 * covered >= minimum * total;
}
