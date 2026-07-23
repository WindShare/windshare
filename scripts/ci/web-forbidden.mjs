import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs'
import { dirname, extname, relative, resolve, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../..')
const WEB_ROOT = resolve(REPOSITORY_ROOT, 'web')
const SOURCE_ROOT = resolve(WEB_ROOT, 'src')
const PRODUCTION_ENTRY = resolve(SOURCE_ROOT, 'main.tsx')
const SOURCE_ONLY = process.argv.includes('--source-only')

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

const FORBIDDEN_SOURCE_PATTERNS = [
  /\bValidatedManifestV1\b/u,
  /\bManifestEntry\b/u,
  /\bPackedLayout\b/u,
  /\bTransferPlan\b/u,
  /\bPlanId\b/u,
  /\bChunkSet\b/u,
  /\bChunkIndex\b/u,
  /\bsealedManifest\b/u,
  /\bSelectionPageWindow\b/u,
  /\bmanifestFingerprint\b/u,
  /\bMAX_SEALED_MANIFEST_BYTES\b/u,
  /\bCIPHER_SUITE_V1\b/u,
  /\bSuiteAESGCM\b/u,
  /\bSUITE_AES_GCM\b/u,
  /windshare-v1/u,
  /\/v1\/ws/u,
  /\bhostileSender\b/u,
  /hostile-sender/u,
]

const FORBIDDEN_VECTOR_NAMES = [
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
]

const FORBIDDEN_STYLE_PATTERNS = [
  /\.status-planning\b/u,
  /\.status-preparing-output\b/u,
  /\.status-reconnecting\b/u,
  /\.status-ready\b/u,
  /\.reconnect-message\b/u,
  /--entry-depth\b/u,
]

const violations = []
for (const path of FORBIDDEN_PATHS) {
  if (obsoletePathExists(resolve(REPOSITORY_ROOT, path))) violations.push(`obsolete path exists: ${path}`)
}

const sourceFiles = filesUnder(SOURCE_ROOT).filter(isTypeScript)
const webTypeScript = [
  ...filesUnder(resolve(WEB_ROOT, 'src')),
  ...filesUnder(resolve(WEB_ROOT, 'test')),
  ...filesUnder(resolve(WEB_ROOT, 'e2e')),
].filter(isTypeScript)
for (const file of webTypeScript) {
  const source = readFileSync(file, 'utf8')
  for (const pattern of FORBIDDEN_SOURCE_PATTERNS) recordMatches(file, source, pattern)
  for (const name of FORBIDDEN_VECTOR_NAMES) {
    if (source.includes(name)) violations.push(`${portable(file)} references retired vector ${name}`)
  }
}
for (const file of filesUnder(SOURCE_ROOT).filter((path) => path.endsWith('.css'))) {
  const source = readFileSync(file, 'utf8')
  for (const pattern of FORBIDDEN_STYLE_PATTERNS) recordMatches(file, source, pattern)
}

const production = productionGraph(PRODUCTION_ENTRY)
for (const file of production) {
  const path = portable(file)
  if (FORBIDDEN_PATHS.some((forbidden) => path === forbidden || path.startsWith(`${forbidden}/`))) {
    violations.push(`production graph reaches obsolete path: ${path}`)
  }
}

let bundleFiles = []
if (!SOURCE_ONLY) {
  const distribution = resolve(WEB_ROOT, 'dist')
  if (!existsSync(distribution)) violations.push('web/dist is missing; run the gate after the production build')
  else {
    bundleFiles = filesUnder(distribution).filter((file) => ['.css', '.html', '.js'].includes(extname(file)))
    for (const file of bundleFiles) {
      const source = readFileSync(file, 'utf8')
      for (const pattern of FORBIDDEN_SOURCE_PATTERNS) recordMatches(file, source, pattern)
      for (const pattern of FORBIDDEN_STYLE_PATTERNS) recordMatches(file, source, pattern)
    }
  }
}

if (violations.length > 0) {
  for (const violation of [...new Set(violations)].sort()) console.error(`web-forbidden: ${violation}`)
  process.exitCode = 1
} else {
  const bundle = SOURCE_ONLY ? 'source-only' : `${bundleFiles.length} bundle files`
  console.log(
    `web-forbidden: PASS (${sourceFiles.length} source files, ` +
    `${production.size} production dependencies, ${bundle})`,
  )
}

function productionGraph(entry) {
  const visited = new Set()
  const pending = [entry]
  while (pending.length > 0) {
    const file = pending.pop()
    if (file === undefined || visited.has(file)) continue
    visited.add(file)
    const source = readFileSync(file, 'utf8')
    for (const specifier of relativeSpecifiers(source)) {
      const dependency = resolveSource(dirname(file), specifier)
      if (dependency === undefined) {
        violations.push(`${portable(file)} has unresolved relative import ${specifier}`)
      } else if (dependency === SOURCE_ROOT || dependency.startsWith(`${SOURCE_ROOT}${sep}`)) {
        pending.push(dependency)
      }
    }
  }
  return visited
}

function relativeSpecifiers(source) {
  const specifiers = []
  for (const match of source.matchAll(/(?:from\s*|import\s*)['"](\.[^'"]+)['"]/gu)) {
    if (match[1] !== undefined) specifiers.push(match[1])
  }
  for (const match of source.matchAll(/import\s*\(\s*['"](\.[^'"]+)['"]\s*\)/gu)) {
    if (match[1] !== undefined) specifiers.push(match[1])
  }
  return specifiers
}

function resolveSource(parent, specifier) {
  const base = resolve(parent, specifier)
  for (const candidate of [
    base,
    `${base}.ts`,
    `${base}.tsx`,
    resolve(base, 'index.ts'),
    resolve(base, 'index.tsx'),
  ]) {
    if (existsSync(candidate) && statSync(candidate).isFile()) return candidate
  }
  return undefined
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

function isTypeScript(file) {
  return file.endsWith('.ts') || file.endsWith('.tsx')
}

function recordMatches(file, source, pattern) {
  const match = pattern.exec(source)
  if (match === null) return
  const line = source.slice(0, match.index).split('\n').length
  violations.push(`${portable(file)}:${line} contains retired ${pattern.source}`)
}

function portable(file) {
  return relative(REPOSITORY_ROOT, file).replaceAll('\\', '/')
}
