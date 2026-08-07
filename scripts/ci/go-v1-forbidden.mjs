import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { spawnSync } from 'node:child_process'

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..')
const violations = []

function filesUnder(directory, predicate = () => true) {
  const files = []
  for (const entry of readdirSync(directory, { withFileTypes: true })) {
    if (entry.name === '.git' || entry.name === 'node_modules' || entry.name === 'tmp') continue
    const path = join(directory, entry.name)
    if (entry.isDirectory()) files.push(...filesUnder(path, predicate))
    else if (entry.isFile() && predicate(entry.name)) files.push(path)
  }
  return files
}

function goFilesUnder(directory) {
  return filesUnder(directory, (name) => name.endsWith('.go'))
}

const productionGoRoots = [
  'core',
  'cmd',
  'connectivity',
  'e2e',
  'internal',
  'relay',
  'transport',
]

function productionGoFiles() {
  return productionGoRoots.flatMap((root) => {
    const absolute = resolve(REPOSITORY_ROOT, root)
    return existsSync(absolute) ? goFilesUnder(absolute) : []
  })
}

function isOneShotCleaner(path) {
  const display = relative(REPOSITORY_ROOT, path).replaceAll('\\', '/')
  // A cleaner is deliberately allowed to name the retired namespace it removes.
  // It is not a runtime reader: the production graph must keep it behind an
  // ownership check and the one-shot cleanup boundary.
  return display.startsWith('core/osfs/internal/checkpointcleaner/') && display.endsWith('.go')
}

const retiredTransferIdentifiers = [
  /\bOpenSelection\s*\(/u,
  /\bV2TransferJob\b/u,
  /\bV2OutputSelectionPlan\b/u,
  /\bV2OutputSelection\b/u,
  /\bCanonicalSelectionV1\b/u,
  /\bResumeIntent\b/u,
  /\bResumeNamespaceName\b/u,
  /\bResumeStateLegacyUntrusted\b/u,
  /\bresumeIntentText\b/u,
  /windshare\/output-resume-intent\/v3/u,
  /\.wsresume-output/u,
  /\bSchemaVersion\s*=\s*uint32\(3\)/u,
  /\bListResumeState\b/u,
  /\bDiscardResumeState\b/u,
  /\bResumeState(?:Inventory|Summary|Ref|Kind)\b/u,
  /\bResumeSessionLifecycle\b/u,
  /\bDiscardSettlement\b/u,
  /\bopenOutputSession\b/u,
  /\boutputSessionOpenAttempt\b/u,
  /\bscanOutputV3FileNamespace\b/u,
  /\badoptFileNamespaceSnapshot\b/u,
  /\bpreviewPrivateTree\b/u,
  /\bresumeSessionListing\b/u,
  /\binspectDiscardCheckpointNamespace\b/u,
  /\bauthorizeLocklessDiscard\b/u,
  /\brecoverTerminalSession\b/u,
  /\bsettleTerminalCheckpointNamespace\b/u,
]

// The incremental scheduler must never regain a terminal whole-tree barrier or
// an all-directory admission ledger. Scan tests as well as production so the
// deleted behavior cannot become the specification for a second architecture.
const retiredSelectionSpoolIdentifiers = [
  /\b(?:selectionSpool(?:Checkpoint)?|newSelectionSpool)\b/u,
  /\bselectionDirectoryReference\b/u,
  /\bselectionPlan\w*\b/u,
  /\bselectionClaim\w*\b/u,
  /\bdiscoverSelection\s*\(/u,
  /\bfinalizeSelectionDirectories\s*\(/u,
  /\bsetSelection\s*\(/u,
  /\bnewOutputSelectionFromPlan\s*\(/u,
  /\boutputSelectionPlan\b/u,
  /\bErrSelectionPlan(?:Budget|State)\b/u,
  /\bmaximumSelectionClaims\b/u,
  /\bplannedDirectory\b/u,
  /\bTransfer(?:SelectionFrozen|GenerationAdmitted)\b/u,
  /\bdirectoryAdmissionKey\b/u,
  /\badmittedDirectories\b/u,
  /\badmissions\s+map\s*\[string\]DirectoryAdmission\b/u,
]

const retiredTransferFiles = [
  'core/transfer/resume_intent.go',
  'core/transfer/selection_external_sort.go',
  'core/transfer/selection_plan_codec.go',
  'core/transfer/selection_spool.go',
  'core/transfer/selection_spool_test.go',
  'core/transfer/job_traversal_boundaries_test.go',
  'core/transfer/output_selection_plan_test.go',
  'core/osfs/internal/outputruntime/resume_legacy.go',
  'core/osfs/internal/outputruntime/resume_list.go',
  'core/osfs/internal/outputruntime/resume_discard.go',
  'core/osfs/internal/outputruntime/namespace_recovery.go',
  'core/osfs/internal/outputruntime/session_terminal.go',
]

for (const path of retiredTransferFiles) {
  if (existsSync(resolve(REPOSITORY_ROOT, path))) {
    violations.push(`retired transfer source still exists: ${path}`)
  }
}

for (const path of goFilesUnder(resolve(REPOSITORY_ROOT, 'core', 'transfer'))) {
  const source = readFileSync(path, 'utf8')
  const display = relative(REPOSITORY_ROOT, path).replaceAll('\\', '/')
  for (const identifier of retiredSelectionSpoolIdentifiers) {
    if (identifier.test(source)) {
      violations.push(`retired whole-tree transfer identifier ${identifier.source} in ${display}`)
    }
  }
}

// These packages encode the retired manifest/plan/chunk protocol. Checking the
// physical trees prevents dead or build-tagged copies from surviving outside the
// production dependency graph and becoming a second architecture.
const retiredTrees = [
  'core/chunk',
  'core/layout',
  'core/manifest',
  'core/share',
  'relay/admission',
  'relay/forward',
  'transport/relay',
]

for (const packageRoot of retiredTrees) {
  const absolute = resolve(REPOSITORY_ROOT, packageRoot)
  if (!existsSync(absolute)) continue
  for (const path of filesUnder(absolute)) {
    const display = relative(REPOSITORY_ROOT, path).replaceAll('\\', '/')
    violations.push(`retired source tree contains a file: ${display}`)
  }
}

// These roots host v2 subpackages, so only direct Go files are forbidden.
const retiredDirectPackageRoots = [
  'connectivity',
  'core/session',
  'relay/protocol',
  'relay/signaling',
]

for (const packageRoot of retiredDirectPackageRoots) {
  const absolute = resolve(REPOSITORY_ROOT, packageRoot)
  if (!existsSync(absolute)) continue
  const directGoFiles = readdirSync(absolute)
    .filter((name) => statSync(join(absolute, name)).isFile() && name.endsWith('.go'))
  for (const name of directGoFiles) {
    violations.push(`retired package root contains Go source: ${packageRoot}/${name}`)
  }
}

const retiredImports = [
  'github.com/windshare/windshare/connectivity',
  'github.com/windshare/windshare/core/chunk',
  'github.com/windshare/windshare/core/layout',
  'github.com/windshare/windshare/core/manifest',
  'github.com/windshare/windshare/core/session',
  'github.com/windshare/windshare/core/share',
  'github.com/windshare/windshare/relay/admission',
  'github.com/windshare/windshare/relay/forward',
  'github.com/windshare/windshare/relay/protocol',
  'github.com/windshare/windshare/relay/signaling',
  'github.com/windshare/windshare/transport/relay',
]

const retiredIdentifiers = [
  /\bSealedManifest\b/u,
  /\bPlanID\b/u,
  /\bTransferPlan\b/u,
  /\bChunkSet\b/u,
  /\bChunkToRanges\b/u,
  /\bBitfield\b/u,
  /\bEncodeManifestFrame\b/u,
  /\bBinTypeManifest\b/u,
  /\bReserveManifestBytes\b/u,
  /\bMaxManifestBytes\b/u,
  /\bManifestIdentityValidator\b/u,
  /\bSplitBlockCT\b/u,
  /\bMaxBlockPayload\b/u,
  /\bSuiteAESGCM\b/u,
  /\bNewShareID\b/u,
  /\blink\.ShareIDBytes\b/u,
  /\/v1\/ws\b/u,
  /\bProtocolVersion\s*=\s*['"]v1['"]/u,
]

for (const path of goFilesUnder(REPOSITORY_ROOT)) {
  const source = readFileSync(path, 'utf8')
  const display = relative(REPOSITORY_ROOT, path).replaceAll('\\', '/')
  if (/\bv1fixtures\b/u.test(source)) {
    violations.push(`retired v1fixtures build path in ${display}`)
  }
  if (
    display.startsWith('core/link/') &&
    (/\bShareIDBytes\b/u.test(source) || (!display.endsWith('_test.go') && /\b0x01\b/u.test(source)))
  ) {
    violations.push(`retired suite-01 link representation in ${display}`)
  }
  for (const importPath of retiredImports) {
    if (source.includes(`"${importPath}"`)) {
      violations.push(`retired import ${importPath} in ${display}`)
    }
  }
  for (const identifier of retiredIdentifiers) {
    if (identifier.test(source)) {
      violations.push(`retired v1 identifier ${identifier.source} in ${display}`)
    }
  }
}

// The incremental output contract has one production authority. Keep retired
// terminal-selection names out of both reachable and dead production copies so
// a build tag cannot quietly restore the old barrier. Tests may describe the
// old contract while they are being rewritten; one-shot cleaners are the only
// production exception because they must recognise old records in order to
// remove them without importing their ranges.
for (const path of productionGoFiles()) {
  const display = relative(REPOSITORY_ROOT, path).replaceAll('\\', '/')
  if (display.endsWith('_test.go') || isOneShotCleaner(path)) continue
  const source = readFileSync(path, 'utf8')
  for (const identifier of retiredTransferIdentifiers) {
    if (identifier.test(source)) {
      violations.push(`retired transfer identifier ${identifier.source} in ${display}`)
    }
  }
}

const forbiddenProductionDependencies = new Set([
  'github.com/windshare/windshare/connectivity',
  'github.com/windshare/windshare/core/chunk',
  'github.com/windshare/windshare/core/layout',
  'github.com/windshare/windshare/core/manifest',
  'github.com/windshare/windshare/core/session',
  'github.com/windshare/windshare/core/share',
  'github.com/windshare/windshare/relay/admission',
  'github.com/windshare/windshare/relay/forward',
  'github.com/windshare/windshare/relay/protocol',
  'github.com/windshare/windshare/relay/signaling',
  'github.com/windshare/windshare/transport/relay',
])

for (const tagArguments of [[], ['-tags=v1fixtures']]) {
  const result = spawnSync(
    process.env.WINDSHARE_GO_EXECUTABLE ?? 'go',
    ['list', ...tagArguments, '-deps', '-f', '{{.ImportPath}}', './cmd/windshare', './relay/cmd/wsrelay'],
    { cwd: REPOSITORY_ROOT, encoding: 'utf8' },
  )
  const label = tagArguments.length === 0 ? 'default' : 'all-tag'
  if (result.status !== 0) {
    violations.push(`${label} production dependency graph did not compile: ${result.stderr.trim()}`)
    continue
  }
  for (const dependency of result.stdout.split(/\r?\n/u).filter(Boolean)) {
    if (forbiddenProductionDependencies.has(dependency)) {
      violations.push(`${label} production dependency graph contains ${dependency}`)
    }
  }
}

// The sender CLI is the production composition root. Keeping these dependencies
// mandatory prevents a refactor from leaving a well-tested P2P island that no
// shipped binary can reach.
const requiredSenderDependencies = new Set([
  'github.com/windshare/windshare/connectivity/v2peer',
  'github.com/windshare/windshare/connectivity/v2signal',
  'github.com/windshare/windshare/transport/webrtc',
])

for (const tagArguments of [[], ['-tags=v1fixtures']]) {
  const result = spawnSync(
    process.env.WINDSHARE_GO_EXECUTABLE ?? 'go',
    ['list', ...tagArguments, '-deps', '-f', '{{.ImportPath}}', './cmd/windshare'],
    { cwd: REPOSITORY_ROOT, encoding: 'utf8' },
  )
  const label = tagArguments.length === 0 ? 'default' : 'all-tag'
  if (result.status !== 0) {
    violations.push(`${label} sender dependency graph did not compile: ${result.stderr.trim()}`)
    continue
  }
  const dependencies = new Set(result.stdout.split(/\r?\n/u).filter(Boolean))
  for (const dependency of requiredSenderDependencies) {
    if (!dependencies.has(dependency)) {
      violations.push(`${label} sender dependency graph does not reach ${dependency}`)
    }
  }
}

if (violations.length > 0) {
  for (const violation of violations) console.error(`go-v1-forbidden: ${violation}`)
  process.exit(1)
}

console.log('go-v1-forbidden: PASS (retired roots and transfer namespaces absent; sender P2P and default/all-tag production graphs enforced)')
