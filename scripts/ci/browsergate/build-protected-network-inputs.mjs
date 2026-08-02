import { createHash } from 'node:crypto'
import {
  appendFileSync,
  chmodSync,
  copyFileSync,
  existsSync,
  lstatSync,
  mkdirSync,
  opendirSync,
  readFileSync,
  realpathSync,
  writeFileSync,
} from 'node:fs'
import { createRequire } from 'node:module'
import { basename, join, resolve } from 'node:path'
import { pathToFileURL } from 'node:url'

const PREPARED_INPUT_SCHEMA = 'windshare.browser-network-matrix.prepared-input/v1'
const EXPECTED_NODE_VERSION = '24.16.0'
const MAXIMUM_PREPARED_INPUT_BYTES = 256 * 1024 * 1024
const SHA256_PATTERN = /^[a-f0-9]{64}$/u
const CHECKOUT_SHA_PATTERN = /^[a-f0-9]{40}$/u
const DECIMAL_ID_PATTERN = /^[1-9][0-9]{0,19}$/u
const root = resolve(import.meta.dirname, '..', '..', '..')

const outputDirectory = requiredEnvironment('BROWSER_NETWORK_PREPARED_DIRECTORY')
const helperDirectory = requiredEnvironment('BROWSER_NETWORK_HELPER_DIRECTORY')
const checkoutSha = requiredEnvironment('GITHUB_SHA', CHECKOUT_SHA_PATTERN)
const runnerTemporary = requireCanonicalDirectory(
  requiredEnvironment('RUNNER_TEMP'),
  'runner temporary directory',
)
const githubRunId = requiredEnvironment('GITHUB_RUN_ID', DECIMAL_ID_PATTERN)
const githubRunAttempt = requiredEnvironment('GITHUB_RUN_ATTEMPT', DECIMAL_ID_PATTERN)
if (outputDirectory !== join(root, 'test-results', 'browser-network-prepared')) {
  throw new Error('prepared output directory is outside its producer checkout')
}
if (helperDirectory !== join(
  runnerTemporary,
  `windshare-browser-network-helpers-${githubRunId}-${githubRunAttempt}`,
)) throw new Error('prepared helper directory is outside its workflow authority')
const resultsRoot = join(root, 'test-results')
if (!existsSync(resultsRoot)) mkdirSync(resultsRoot, { recursive: false, mode: 0o700 })
requireCanonicalDirectory(resultsRoot, 'prepared output parent')
requireNewDirectory(outputDirectory)
requireCanonicalDirectory(helperDirectory, 'helper directory')
mkdirSync(outputDirectory, { recursive: false, mode: 0o700 })

const requireFromWeb = createRequire(pathToFileURL(join(root, 'web/package.json')))
const { build } = await import(pathToFileURL(requireFromWeb.resolve('vite')).href)
const bundlePath = await buildBundle(
  'scripts/ci/browsergate/network-entry.mjs',
  'network-entry-bundle.mjs',
)
const completionBundlePath = await buildBundle(
  'scripts/ci/browsergate/network-completion.mjs',
  'network-completion-bundle.mjs',
)

const builtManifest = parseBuiltHelperManifest(join(helperDirectory, 'helper-manifest.json'))
const publisherSource = helperPath(builtManifest, 'artifact-publisher')
const processOwnerSource = helperPath(builtManifest, 'test-process-owner')
const publisherPath = join(outputDirectory, 'browsermatrixpublish')
const processOwnerPath = join(outputDirectory, 'testprocessowner')
copyFileSync(publisherSource, publisherPath)
copyFileSync(processOwnerSource, processOwnerPath)
chmodSync(publisherPath, 0o700)
chmodSync(processOwnerPath, 0o700)

const brokerPath = join(root, 'scripts/ci/browsergate/oidc-network-broker.mjs')
const preparedBrokerPath = join(outputDirectory, 'oidc-network-broker.mjs')
copyFileSync(brokerPath, preparedBrokerPath)
chmodSync(preparedBrokerPath, 0o500)
const scheduledRoot = join(root, 'testdata/browser-network-matrix')
const scheduledManifestPath = join(scheduledRoot, 'scheduled-hard.manifest.v2.json')
const preparedScheduledManifestPath = join(outputDirectory, 'scheduled-hard.manifest.v2.json')
copyFileSync(scheduledManifestPath, preparedScheduledManifestPath)
const scheduledProfileNames = Object.freeze([
  'scheduled-coturn.v2.json',
  'scheduled-public-stun.v2.json',
  'scheduled-restricted-udp.v2.json',
])
const preparedProfilesDirectory = join(outputDirectory, 'profiles')
mkdirSync(preparedProfilesDirectory, { recursive: false, mode: 0o700 })
for (const profileName of scheduledProfileNames) {
  copyFileSync(
    join(scheduledRoot, 'profiles', profileName),
    join(preparedProfilesDirectory, profileName),
  )
}
const manifest = Object.freeze({
  schemaVersion: PREPARED_INPUT_SCHEMA,
  checkoutSha,
  nodeVersion: EXPECTED_NODE_VERSION,
  broker: fileIdentity(preparedBrokerPath, 'oidc-network-broker.mjs'),
  runtimeBundle: fileIdentity(bundlePath, basename(bundlePath)),
  completionBundle: fileIdentity(completionBundlePath, basename(completionBundlePath)),
  scheduledManifest: fileIdentity(
    preparedScheduledManifestPath,
    basename(preparedScheduledManifestPath),
  ),
  scheduledProfiles: Object.freeze(scheduledProfileNames.map((profileName) =>
    fileIdentity(join(preparedProfilesDirectory, profileName), `profiles/${profileName}`))),
  publisherHelper: fileIdentity(publisherPath, basename(publisherPath)),
  processOwner: fileIdentity(processOwnerPath, basename(processOwnerPath)),
})
const manifestBytes = Buffer.from(`${JSON.stringify(manifest)}\n`, 'utf8')
writeFileSync(
  join(outputDirectory, 'producer-manifest.json'),
  manifestBytes,
  { encoding: 'utf8', flag: 'wx', mode: 0o600 },
)
requireExactPreparedInventory(outputDirectory, scheduledProfileNames)
const manifestSha256 = createHash('sha256').update(manifestBytes).digest('hex')
publishWorkflowOutput('producer_manifest_sha256', manifestSha256)
process.stdout.write(`${JSON.stringify({
  schemaVersion: PREPARED_INPUT_SCHEMA,
  outcome: 'completed',
  outputDirectory,
  producerManifestSha256: manifestSha256,
})}\n`)

async function buildBundle(sourceRelativePath, outputFileName) {
  await build({
    configFile: false,
    logLevel: 'error',
    root,
    plugins: [{
      name: 'windshare-browser-network-optional-bidi-boundary',
      resolveId(source) {
        return source.startsWith('chromium-bidi/lib/cjs/')
          ? '\0windshare-unsupported-playwright-bidi'
          : undefined
      },
      load(id) {
        if (id !== '\0windshare-unsupported-playwright-bidi') return undefined
        // The matrix launches local browser servers and never requests Playwright's
        // optional WebDriver-BiDi-over-CDP bridge. A bundled fail-closed boundary
        // avoids carrying an undeclared package or a latent runtime require.
        return `
          const unsupported = () => { throw new Error('optional Playwright BiDi bridge is unavailable') }
          export const BidiServer = Object.freeze({ createAndStart: unsupported })
          export class MapperCdpConnection { constructor() { unsupported() } }
        `
      },
    }],
    build: {
      emptyOutDir: false,
      minify: false,
      outDir: outputDirectory,
      ssr: join(root, ...sourceRelativePath.split('/')),
      target: 'node24',
      rolldownOptions: {
        output: {
          entryFileNames: outputFileName,
          codeSplitting: false,
        },
      },
    },
    ssr: { noExternal: true },
  })
  return join(outputDirectory, outputFileName)
}

function parseBuiltHelperManifest(path) {
  const bytes = readFileSync(path)
  const text = new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  const value = JSON.parse(text)
  if (text !== `${JSON.stringify(value)}\n` || typeof value !== 'object' || value === null) {
    throw new Error('built helper manifest is not canonical JSON')
  }
  const keys = Object.keys(value)
  if (
    keys.join(',') !== 'schemaVersion,platform,architecture,helpers' ||
    value.schemaVersion !== 'windshare.browser-network-matrix.helper-build/v2' ||
    value.platform !== 'linux' || value.architecture !== 'amd64' ||
    !Array.isArray(value.helpers) || value.helpers.length !== 2
  ) throw new Error('built helper manifest is invalid')
  return value
}

function helperPath(manifest, role) {
  const matches = manifest.helpers.filter((entry) => entry?.role === role)
  if (matches.length !== 1 || Object.keys(matches[0]).join(',') !== 'role,path') {
    throw new Error(`built helper role is invalid: ${role}`)
  }
  const expectedName = role === 'artifact-publisher' ? 'browsermatrixpublish' : 'testprocessowner'
  const path = requireCanonicalFile(matches[0].path, role)
  if (path !== join(helperDirectory, expectedName)) {
    throw new Error(`built helper path is outside its prepared directory: ${role}`)
  }
  return path
}

function fileIdentity(path, fileName) {
  const canonical = requireCanonicalFile(path, fileName)
  const bytes = readFileSync(canonical)
  if (bytes.byteLength === 0 || bytes.byteLength > MAXIMUM_PREPARED_INPUT_BYTES) {
    throw new Error(`prepared input has an invalid byte length: ${fileName}`)
  }
  const sha256 = createHash('sha256').update(bytes).digest('hex')
  if (!SHA256_PATTERN.test(sha256)) throw new Error(`prepared input digest is invalid: ${fileName}`)
  return Object.freeze({ fileName, byteLength: bytes.byteLength, sha256 })
}

function publishWorkflowOutput(name, value) {
  const outputPath = process.env.GITHUB_OUTPUT
  if (outputPath === undefined) return
  const canonical = requireCanonicalFile(outputPath, 'GitHub workflow output')
  appendFileSync(canonical, `${name}=${value}\n`, { encoding: 'utf8' })
}

function requireNewDirectory(value) {
  const canonical = resolve(value)
  if (canonical !== value) throw new Error('prepared output directory must be canonical')
  const parent = requireCanonicalDirectory(resolve(canonical, '..'), 'prepared output parent')
  if (join(parent, basename(canonical)) !== canonical) {
    throw new Error('prepared output directory escapes its parent')
  }
  try {
    lstatSync(canonical)
    throw new Error('prepared output directory must be new')
  } catch (cause) {
    if (cause?.code !== 'ENOENT') throw cause
  }
}

function requireCanonicalDirectory(value, label) {
  const canonical = resolve(value)
  const metadata = lstatSync(canonical)
  if (!metadata.isDirectory() || metadata.isSymbolicLink() || realpathSync(canonical) !== canonical) {
    throw new Error(`${label} is not a canonical real directory`)
  }
  return canonical
}

function requireCanonicalFile(value, label) {
  const canonical = resolve(value)
  const metadata = lstatSync(canonical)
  if (!metadata.isFile() || metadata.isSymbolicLink() || realpathSync(canonical) !== canonical) {
    throw new Error(`${label} is not a canonical real file`)
  }
  return canonical
}

function requireExactPreparedInventory(directory, profileNames) {
  requireExactDirectoryEntries(directory, new Map([
    ['browsermatrixpublish', 'file'],
    ['network-completion-bundle.mjs', 'file'],
    ['network-entry-bundle.mjs', 'file'],
    ['oidc-network-broker.mjs', 'file'],
    ['producer-manifest.json', 'file'],
    ['profiles', 'directory'],
    ['scheduled-hard.manifest.v2.json', 'file'],
    ['testprocessowner', 'file'],
  ]), 'prepared network input')
  const profiles = join(directory, 'profiles')
  requireCanonicalDirectory(profiles, 'prepared profile directory')
  requireExactDirectoryEntries(
    profiles,
    new Map(profileNames.map((name) => [name, 'file'])),
    'prepared profile',
  )
}

function requireExactDirectoryEntries(directory, expected, label) {
  const authority = opendirSync(directory)
  const observed = new Map()
  try {
    while (true) {
      const entry = authority.readSync()
      if (entry === null) break
      // Reject at N+1 so a hostile build cannot turn validation into an unbounded scan.
      if (observed.size === expected.size) throw new Error(`${label} inventory contains an extra entry`)
      observed.set(entry.name, entry)
    }
  } finally {
    authority.closeSync()
  }
  if (observed.size !== expected.size) throw new Error(`${label} inventory is incomplete`)
  for (const [name, kind] of expected) {
    const entry = observed.get(name)
    if (
      entry === undefined || entry.isSymbolicLink() ||
      kind === 'file' && !entry.isFile() || kind === 'directory' && !entry.isDirectory()
    ) throw new Error(`${label} inventory entry is invalid: ${name}`)
  }
}

function requiredEnvironment(name, pattern) {
  const value = process.env[name]
  if (typeof value !== 'string' || value.length === 0 || value.includes('\0')) {
    throw new Error(`${name} is unavailable`)
  }
  if (pattern !== undefined && !pattern.test(value)) throw new Error(`${name} is invalid`)
  return value
}
