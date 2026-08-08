import { existsSync, readdirSync, statSync } from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { spawnSync } from 'node:child_process'

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..')
const violations = []

function filesUnder(directory) {
  const files = []
  for (const entry of readdirSync(directory, { withFileTypes: true })) {
    if (entry.name === '.git' || entry.name === 'node_modules' || entry.name === 'tmp') continue
    const path = join(directory, entry.name)
    if (entry.isDirectory()) files.push(...filesUnder(path))
    else if (entry.isFile()) files.push(path)
  }
  return files
}

// These exact surfaces were retired inside packages that remain. Listing paths
// rather than identifiers keeps the check structural: a new type may reuse
// ordinary domain language without being mistaken for the deleted architecture.
const retiredFiles = [
  'core/content/coverage_wave2_test.go',
  'core/transfer/output_selection.go',
  'core/transfer/resume_intent.go',
  'core/transfer/selection_external_sort.go',
  'core/transfer/selection_plan_codec.go',
  'core/transfer/selection_spool.go',
  'core/transfer/selection_spool_test.go',
  'core/transfer/job_traversal_boundaries_test.go',
  'core/transfer/output_selection_plan_test.go',
  'core/osfs/internal/outputlinux/linux_output_selection_metadata.go',
  'core/osfs/internal/outputwindows/metadata_admission.go',
  'core/osfs/internal/outputwindows/coverage_edges_wave2_test.go',
  'core/osfs/internal/outputwindows/coverage_edges_wave3_test.go',
  'core/osfs/internal/outputwindows/coverage_edges_wave4_test.go',
  'core/osfs/internal/outputwindows/coverage_edges_wave5_test.go',
  'core/osfs/internal/outputruntime/ancestry.go',
  'core/osfs/internal/outputruntime/file_prepare.go',
  'core/osfs/internal/outputruntime/file_publication.go',
  'core/osfs/internal/outputruntime/file_reconcile.go',
  'core/osfs/internal/outputruntime/file_recovery.go',
  'core/osfs/internal/outputruntime/file_recovery_actions.go',
  'core/osfs/internal/outputruntime/file_recovery_support.go',
  'core/osfs/internal/outputruntime/file_retirement.go',
  'core/osfs/internal/outputruntime/file_settlement.go',
  'core/osfs/internal/outputruntime/file_transfer.go',
  'core/osfs/internal/outputruntime/incremental_admission.go',
  'core/osfs/internal/outputruntime/incremental_checkpoint.go',
  'core/osfs/internal/outputruntime/incremental_checkpoint_load.go',
  'core/osfs/internal/outputruntime/incremental_session.go',
  'core/osfs/internal/outputruntime/inventory.go',
  'core/osfs/internal/outputruntime/namespace_recovery.go',
  'core/osfs/internal/outputruntime/portable_platform_identity_test.go',
  'core/osfs/internal/outputruntime/resume_discard.go',
  'core/osfs/internal/outputruntime/resume_legacy.go',
  'core/osfs/internal/outputruntime/resume_list.go',
  'core/osfs/internal/outputruntime/selection_tree.go',
  'core/osfs/internal/outputruntime/session_open.go',
  'core/osfs/internal/outputruntime/session_state.go',
  'core/osfs/internal/outputruntime/session_terminal.go',
]

for (const path of retiredFiles) {
  if (existsSync(resolve(REPOSITORY_ROOT, path))) {
    violations.push(`retired source still exists: ${path}`)
  }
}

// Whole retired packages are checked physically so dead or build-tagged copies
// cannot survive outside the dependency graph as a second implementation.
const retiredTrees = [
  'core/chunk',
  'core/layout',
  'core/manifest',
  'core/share',
  'core/osfs/internal/outputnamespace',
  'core/osfs/internal/resumestate',
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

// These roots host current subpackages, so only direct Go files are retired.
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

const forbiddenProductionDependencies = new Set([
  'github.com/windshare/windshare/connectivity',
  'github.com/windshare/windshare/core/chunk',
  'github.com/windshare/windshare/core/layout',
  'github.com/windshare/windshare/core/manifest',
  'github.com/windshare/windshare/core/osfs/internal/outputnamespace',
  'github.com/windshare/windshare/core/osfs/internal/resumestate',
  'github.com/windshare/windshare/core/session',
  'github.com/windshare/windshare/core/share',
  'github.com/windshare/windshare/relay/admission',
  'github.com/windshare/windshare/relay/forward',
  'github.com/windshare/windshare/relay/protocol',
  'github.com/windshare/windshare/relay/signaling',
  'github.com/windshare/windshare/transport/relay',
])

// The sender is the production composition root for native transfer and resume.
// Requiring these deep modules prevents a refactor from leaving a tested island
// that no shipped binary reaches.
const requiredSenderDependencies = new Set([
  'github.com/windshare/windshare/connectivity/v2peer',
  'github.com/windshare/windshare/connectivity/v2signal',
  'github.com/windshare/windshare/core/osfs/internal/checkpointstore',
  'github.com/windshare/windshare/core/osfs/internal/directoryauthority',
  'github.com/windshare/windshare/core/osfs/internal/fileexecution',
  'github.com/windshare/windshare/core/osfs/internal/outputsession',
  'github.com/windshare/windshare/core/osfs/internal/resumeauthority',
  'github.com/windshare/windshare/transport/webrtc',
])

for (const tagArguments of [[], ['-tags=v1fixtures']]) {
  const label = tagArguments.length === 0 ? 'default' : 'all-tag'
  const sender = productionDependencies(tagArguments, './cmd/windshare', `${label} sender`)
  if (sender !== undefined) {
    rejectForbiddenDependencies(sender, `${label} sender`)
    for (const dependency of requiredSenderDependencies) {
      if (!sender.has(dependency)) {
        violations.push(`${label} sender production graph does not reach ${dependency}`)
      }
    }
  }

  const relay = productionDependencies(tagArguments, './relay/cmd/wsrelay', `${label} relay`)
  if (relay !== undefined) rejectForbiddenDependencies(relay, `${label} relay`)
}

if (violations.length > 0) {
  for (const violation of violations) console.error(`go-retired-graph: ${violation}`)
  process.exit(1)
}

console.log(
  'go-retired-graph: PASS (exact retired paths absent; native output/resume and sender P2P production edges present)',
)

function productionDependencies(tagArguments, entryPoint, label) {
  const result = spawnSync(
    process.env.WINDSHARE_GO_EXECUTABLE ?? 'go',
    ['list', ...tagArguments, '-deps', '-f', '{{.ImportPath}}', entryPoint],
    { cwd: REPOSITORY_ROOT, encoding: 'utf8' },
  )
  if (result.status !== 0) {
    violations.push(`${label} production dependency graph did not compile: ${result.stderr.trim()}`)
    return undefined
  }
  return new Set(result.stdout.split(/\r?\n/u).filter(Boolean))
}

function rejectForbiddenDependencies(dependencies, label) {
  for (const dependency of dependencies) {
    if (forbiddenProductionDependencies.has(dependency)) {
      violations.push(`${label} production graph contains ${dependency}`)
    }
  }
}
