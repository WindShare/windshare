import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs'
import { dirname, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { productionGraph } from './web-source-graph.mjs'

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../..')
const WEB_ROOT = resolve(REPOSITORY_ROOT, 'web')
const SOURCE_ROOT = resolve(WEB_ROOT, 'src')
const PRODUCTION_ENTRY = resolve(SOURCE_ROOT, 'main.tsx')

const FORBIDDEN_PATHS = [
  'web/src/manifest',
  'web/src/download',
  'web/src/contracts/index.ts',
  'web/src/contracts/link.ts',
  'web/src/contracts/manifest.ts',
  'web/src/contracts/selection.ts',
  'web/src/contracts/sink.ts',
  'web/src/crypto/capability-link.ts',
  'web/src/crypto/chunk-opener.ts',
  'web/src/crypto/key-derivation.ts',
  'web/src/connectivity/receiver-policy.ts',
  'web/src/session/channel-entry.ts',
  'web/src/session/channel-settlement.ts',
  'web/src/session/cleanup-failure.ts',
  'web/src/session/completion.ts',
  'web/src/session/delivery.ts',
  'web/src/session/demand.ts',
  'web/src/session/frame.ts',
  'web/src/session/lifetime.ts',
  'web/src/session/model.ts',
  'web/src/session/reassembly.ts',
  'web/src/session/receive-options.ts',
  'web/src/session/receive-ownership.ts',
  'web/src/session/receive.ts',
  'web/src/transport/relay/channel.ts',
  'web/src/transport/relay/endpoint.ts',
  'web/src/transport/relay/outbox.ts',
  'web/src/transport/relay/protocol.ts',
  'web/src/transport/relay/receiver.ts',
  'web/src/transport/relay/retry-timing.ts',
  'web/src/transport/relay/socket.ts',
  'web/src/ui/browser-gateway.ts',
  'web/src/ui/browser-output.ts',
  'web/src/ui/capability-source.ts',
  'web/src/ui/controller.ts',
  'web/src/ui/model.ts',
  'web/src/ui/ReceiverApp.tsx',
  'web/src/ui/selection-window.ts',
  'web/src/ui/v2-paused-tasks.ts',
  'web/src/transfer/output-selection.ts',
  'web/src/transfer/job.ts',
  'web/src/output/browser/paused-task-lifecycle.ts',
  'web/test/manifest',
  'web/test/download',
  'web/test/contracts',
  'web/test/crypto/capability-link.test.ts',
  'web/test/crypto/chunk-opener.test.ts',
  'web/test/crypto/key-derivation.test.ts',
  'web/test/browser/c1-crypto.spec.ts',
  'web/test/browser/c3-download.spec.ts',
  'web/test/browser/c4-app.spec.ts',
  'web/test/browser/c4-harness.ts',
  'web/test/browser/d4-connectivity.spec.ts',
  'web/test/browser/d4-harness.ts',
  'web/test/browser/d4-real-stack.spec.ts',
  'web/test/connectivity/gateway-integration.test.ts',
  'web/test/connectivity/gateway-retry.test.ts',
  'web/test/connectivity/receiver-policy.test.ts',
  'web/test/connectivity/session-integration.test.ts',
  'web/test/session/browser-performance.bench.ts',
  'web/test/session/download-integration.test.ts',
  'web/test/session/frame.test.ts',
  'web/test/session/helpers.ts',
  'web/test/session/performance-browser.test.ts',
  'web/test/session/performance-browser.ts',
  'web/test/session/performance.playwright.config.ts',
  'web/test/session/reassembly.test.ts',
  'web/test/session/receive-ownership.test.ts',
  'web/test/session/receive.test.ts',
  'web/test/session/scheduler.bench.ts',
  'web/test/transport/browser-socket.test.ts',
  'web/test/transport/channel-conformance.test.ts',
  'web/test/transport/helpers.ts',
  'web/test/transport/protocol.test.ts',
  'web/test/transport/receiver.test.ts',
  'web/test/transfer/job.test.ts',
  'web/test/output/job-integration.test.ts',
  'web/test/protocol/v2-production-boundary.test.ts',
  'web/test/ui/browser-gateway-ownership.test.ts',
  'web/test/ui/browser-gateway.test.ts',
  'web/test/ui/browser-output.test.ts',
  'web/test/ui/capability-source.test.ts',
  'web/test/ui/controller.test.ts',
  'web/test/ui/model.test.ts',
  'web/test/vectors.test.ts',
  'web/e2e/fixtures/browser-socket.ts',
  'web/e2e/fixtures/browser.ts',
  'web/e2e/fixtures/m1c-path.ts',
  'web/e2e/fixtures/process.ts',
  'web/e2e/fixtures/test.ts',
  'web/e2e/fixtures/hostile-sender',
  'web/e2e/m1c-real-path.spec.ts',
  'web/e2e/real-stack.spec.ts',
  'web/e2e/security.spec.ts',
  'web/e2e/streaming-zip.spec.ts',
]

const RETIRED_VECTOR_PATHS = [
  'chunk-seal.json',
  'frame-codec.json',
  'geometry.json',
  'keyderiv.json',
  'link.json',
  'manifest-seal.json',
  'relay-endpoint.json',
  'relay-envelope.json',
  'relay-signaling.json',
  'transfer-plan.json',
].map((name) => `core/testvectors/${name}`)

const RETIRED_PRODUCTION_SYMBOLS = [
  ['TransferIntent', /\bTransferIntent\b/u],
  ['TransferIntentDigest', /\bTransferIntentDigest\b/u],
  ['V2OutputIntent', /\bV2OutputIntent\b/u],
  ['knownSingleFile', /\bknownSingleFile\b/u],
  ['OutputSelectionShape', /\bOutputSelectionShape\b/u],
  ['OutputAcquisitionIntent', /\bOutputAcquisitionIntent\b/u],
  ['exportPartial', /\bexportPartial\b/u],
  ['PausedTaskDescriptorV1', /\bPausedTaskDescriptorV1\b/u],
  ['completed-partial', /completed-partial/u],
]

// These are shipped composition edges, not symbol conventions. Requiring them
// catches a deep module that remains tested but is no longer reachable from the
// browser entry point.
const REQUIRED_PRODUCTION_DEPENDENCIES = [
  'web/src/session/v2-runtime.ts',
  'web/src/transport/relay/v2-receiver.ts',
  'web/src/transfer/v2-job.ts',
  'web/src/transfer/settlement/persistent-execution.ts',
  'web/src/transfer/settlement/v2-plan-authority.ts',
  'web/src/preview/v2-preview.ts',
  'web/src/preview/mp4-range.ts',
  'web/src/ui/v2-output.ts',
  'web/src/ui/v2-browser-receive-composition.ts',
  'web/src/output/browser/indexeddb-repository.ts',
  'web/src/output/browser/indexeddb-resume-state.ts',
  'web/src/output/resume/authority.ts',
  'web/src/output/resume/descriptor.ts',
  'web/src/output/resume/reopen-authority.ts',
  'web/src/output/resume/workspace-continuation.ts',
  'web/src/output/file-system-access/session.ts',
  'web/src/output/file-system-access/settlement.ts',
  'web/src/output/file-system-access/compatible-name/restoration/windows-v1.ps1',
  'web/src/output/origin-private/session.ts',
  'web/src/output/origin-private/workflow.ts',
  'web/src/output/origin-private/zip-exporter.ts',
  'web/src/output/portable/preparation.ts',
  'web/src/output/portable/packaged-handoff.ts',
  'web/src/output/streams/zip-spool.ts',
]

// Every production file in these deep decision modules must be reachable. This
// prevents a new policy or lifecycle authority from existing only in tests while
// a shallower composition silently keeps the retired behavior.
const REQUIRED_PRODUCTION_MODULE_ROOTS = [
  'web/src/transfer/projection',
  'web/src/output/planning',
  'web/src/output/workspace',
  'web/src/output/zip-layout',
]

const violations = []
for (const path of [...FORBIDDEN_PATHS, ...RETIRED_VECTOR_PATHS]) {
  if (obsoletePathExists(resolve(REPOSITORY_ROOT, path))) {
    violations.push(`retired path exists: ${path}`)
  }
}

const { dependencies: production, unresolved } = productionGraph(PRODUCTION_ENTRY, SOURCE_ROOT)
for (const missing of unresolved) {
  violations.push(`${portable(missing.importer)} has unresolved relative import ${missing.specifier}`)
}
const productionPaths = new Set([...production].map(portable))
const requiredProductionPaths = new Set(REQUIRED_PRODUCTION_DEPENDENCIES)
for (const root of REQUIRED_PRODUCTION_MODULE_ROOTS) {
  const absoluteRoot = resolve(REPOSITORY_ROOT, root)
  const moduleFiles = existsSync(absoluteRoot) && statSync(absoluteRoot).isDirectory()
    ? filesUnder(absoluteRoot).filter(isProductionTypeScript)
    : []
  if (moduleFiles.length === 0) {
    violations.push(`required production module has no TypeScript source: ${root}`)
  }
  for (const file of moduleFiles) {
    requiredProductionPaths.add(portable(file))
  }
}

for (const forbidden of FORBIDDEN_PATHS.filter((path) => path.startsWith('web/src/'))) {
  if ([...productionPaths].some((path) => path === forbidden || path.startsWith(`${forbidden}/`))) {
    violations.push(`production graph reaches retired path: ${forbidden}`)
  }
}
for (const file of production) {
  if (!isProductionTypeScript(file)) continue
  const source = readFileSync(file, 'utf8')
  for (const [name, pattern] of RETIRED_PRODUCTION_SYMBOLS) {
    if (pattern.test(source)) {
      violations.push(`production graph contains retired symbol ${name} in ${portable(file)}`)
    }
  }
}
for (const required of requiredProductionPaths) {
  if (!productionPaths.has(required)) {
    violations.push(`production graph does not reach ${required}`)
  }
}

if (violations.length > 0) {
  for (const violation of [...new Set(violations)].sort()) console.error(`web-retired-graph: ${violation}`)
  process.exitCode = 1
} else {
  console.log(
    `web-retired-graph: PASS (${FORBIDDEN_PATHS.length + RETIRED_VECTOR_PATHS.length} exact retired paths absent; ` +
    `${RETIRED_PRODUCTION_SYMBOLS.length} retired symbols absent; ${production.size} production dependencies; ` +
    `${requiredProductionPaths.size} required edges present)`,
  )
}

function filesUnder(root) {
  if (!existsSync(root)) return []
  const files = []
  for (const entry of readdirSync(root, { withFileTypes: true })) {
    const path = resolve(root, entry.name)
    if (entry.isDirectory()) files.push(...filesUnder(path))
    else if (entry.isFile()) files.push(path)
  }
  return files
}

function obsoletePathExists(path) {
  if (!existsSync(path)) return false
  return statSync(path).isDirectory() ? filesUnder(path).length > 0 : true
}

function isProductionTypeScript(file) {
  const path = portable(file)
  const isTypeScript = (file.endsWith('.ts') || file.endsWith('.tsx')) && !file.endsWith('.d.ts')
  const isTest = /(?:^|\/)(?:__tests__|test|tests)\//u.test(path) || /\.(?:bench|spec|test)\.tsx?$/u.test(path)
  return isTypeScript && !isTest
}

function portable(file) {
  return relative(REPOSITORY_ROOT, file).replaceAll('\\', '/')
}
