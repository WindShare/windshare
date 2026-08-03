import assert from 'node:assert/strict'
import {
  constants as fsConstants,
  fstatSync,
  mkdtempSync,
  openSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { resolve } from 'node:path'

import {
  browserNetworkEntryFailureRecord,
  isAnonymousDescriptorMetadata,
  readMintedOidcEnvelope,
  runBrowserNetworkEntry,
} from '../../network-entry.mjs'

const root = resolve(import.meta.dirname, '..', '..', '..', '..', '..')
let matrixAuthorityResolved = false
await assert.rejects(runBrowserNetworkEntry([], {
  get runMatrixCli() {
    matrixAuthorityResolved = true
    throw new Error('missing-authority execution must not resolve the matrix CLI authority')
  },
}), /requires an explicit scheduled execution/u)
assert.equal(matrixAuthorityResolved, false)

const forwarded = []
assert.equal(await runBrowserNetworkEntry([
  'execute',
  '--mode',
  'scheduled',
  '--manifest',
  'manifest.json',
], {
  runMatrixCli: async (arguments_) => {
    forwarded.push(arguments_)
    return 7
  },
}), 7)
assert.deepEqual(forwarded, [[
  'execute',
  '--mode',
  'scheduled',
  '--manifest',
  'manifest.json',
]])

await assert.rejects(
  runBrowserNetworkEntry(['aggregate', '--manifest', 'manifest.json'], {
    runMatrixCli: async () => assert.fail('aggregate must not become full-matrix evidence'),
  }),
  /accepts only scheduled execution/u,
)

let configuredCalls = 0
await assert.rejects(
  runBrowserNetworkEntry(['execute', '--runtime-config', 'invalid.json'], {
    runMatrixCli: async () => {
      configuredCalls += 1
      throw new Error('configured-runtime-invalid')
    },
  }),
  /configured-runtime-invalid/u,
)
assert.equal(configuredCalls, 1)
assert.deepEqual(browserNetworkEntryFailureRecord(), {
  schemaVersion: 'windshare.browser-network-matrix.local-entry/v1',
  component: 'browser-network-entry',
  outcome: 'failed',
  blocking: true,
  failureCode: 'scheduled-network-execution-failed',
})

const envelope = {
  protocolVersion: 'windshare.browser-network-matrix.minted-oidc/v1',
  audience: 'windshare-browser-network-matrix',
  requestOrigin: 'https://pipelines.actions.githubusercontent.com',
  requestPath: '/oidc/token',
  requestQuery: '?api-version=2.0',
  assertion: 'header.payload.signature',
}
assert.equal(isAnonymousDescriptorMetadata({
  mode: fsConstants.S_IFIFO,
  isFIFO: () => false,
  isSocket: () => false,
}), true, 'Windows anonymous-pipe mode must not depend on unsupported Stats predicates')

const regularRoot = mkdtempSync(resolve(tmpdir(), 'windshare-minted-oidc-file-'))
try {
  const regularPath = resolve(regularRoot, 'authority.json')
  writeFileSync(regularPath, JSON.stringify(envelope), { encoding: 'utf8', flag: 'wx' })
  const regularDescriptor = openSync(regularPath, 'r')
  assert.equal(isAnonymousDescriptorMetadata(fstatSync(regularDescriptor)), false)
  assert.throws(
    () => readMintedOidcEnvelope(regularDescriptor),
    /must arrive through an anonymous descriptor/u,
  )
  assert.throws(() => fstatSync(regularDescriptor), (cause) => cause?.code === 'EBADF')
} finally {
  rmSync(regularRoot, { recursive: true, force: true })
}

const brokerSource = readFileSync(resolve(root, 'scripts/ci/browsergate/oidc-network-broker.mjs'), 'utf8')
assert.match(brokerSource, /network-entry-bundle\.mjs/u)
assert.match(brokerSource, /network-completion-bundle\.mjs/u)
assert.match(brokerSource, /requireExactPreparedInventory\(preparedDirectory\)/u)
assert.match(brokerSource, /observed\.size === expected\.size/u)
assert.match(brokerSource, /mkdtempSync/u)
assert.match(brokerSource, /writePrivateFile\(join\(runtimeRoot/u)
assert.match(brokerSource, /spawn\(process\.execPath/u)
assert.match(brokerSource, /runtimeBundle: join\(runtimeRoot, 'network-entry-bundle\.mjs'\)/u)
assert.match(brokerSource, /folded === 'GITHUB_ENV' \|\| folded === 'GITHUB_PATH' \|\| folded === 'PATH'/u)
assert.doesNotMatch(brokerSource, /makeauthority|browser-network\.sh|browser-network\.ps1/u)
const captureIndex = brokerSource.indexOf('launch = capturePreparedLaunchAuthority(process.env)')
const mintIndex = brokerSource.indexOf('minted = await mintOidcAssertion(captured, audience)')
const executeIndex = brokerSource.indexOf('executeNetworkChild(launch, minted, audience, stableOperationId)')
assert.ok(captureIndex >= 0 && mintIndex > captureIndex && executeIndex > mintIndex,
  'verified private inputs must be retained before minting or launching network authority')

const makefile = readFileSync(resolve(root, 'Makefile'), 'utf8')
const prGates = makefile.match(/^override CI_GATES := (.+)$/mu)?.[1]?.split(/\s+/u) ?? []
const fullGates = makefile.match(/^override CI_FULL_GATES := (.+)$/mu)?.[1]?.split(/\s+/u) ?? []
assert.equal(prGates.includes('browser-network'), false)
assert.deepEqual(fullGates, [...prGates, 'browser'])
assert.equal(fullGates.filter((gate) => gate === 'browser').length, 1)
assert.equal(fullGates.includes('browser-network'), false)
assert.match(makefile, /^browser: authority-context browser-local browser-network$/mu)
assert.match(makefile, /^browser-network: authority-context$/mu)
assert.match(makefile, /scripts\/ci\/linux\/browser-network\.sh/u)
assert.doesNotMatch(makefile, /BROWSER_NETWORK_RUNTIME_CONFIG|BROWSER_NETWORK_HELPER_PARENT/u)
assert.doesNotMatch(makefile, /^network\s*:/mu)

const workflow = readFileSync(resolve(root, '.github/workflows/current-commit.yml'), 'utf8')
assert.match(workflow, /shell: \/bin\/bash --noprofile --norc -p -euo pipefail \{0\}/u)
assert.match(workflow, /exec "\$RUNNER_TOOL_CACHE\/node\/24\.16\.0\/x64\/bin\/node"/u)
assert.doesNotMatch(workflow, /shell:\s+node\b/u)
assert.doesNotMatch(workflow, /web-node-modules\.tar|(?:^|\s)tar\s+-[a-z]*[xf]/mu)
for (const startupName of [
  'BASH_ENV', 'ENV', 'LD_AUDIT', 'LD_LIBRARY_PATH', 'LD_PRELOAD', 'NODE_EXTRA_CA_CERTS',
  'NODE_OPTIONS', 'NODE_PATH', 'NODE_TLS_REJECT_UNAUTHORIZED', 'OPENSSL_CONF',
  'SSL_CERT_DIR', 'SSL_CERT_FILE', 'SSLKEYLOGFILE',
]) {
  assert.match(workflow, new RegExp(`^\\s+${startupName}: ''$`, 'mu'))
  assert.match(brokerSource, new RegExp(`^\\s+'${startupName}',$`, 'mu'))
}
assert.match(workflow, /^\s+PATH: \/usr\/local\/sbin:\/usr\/local\/bin:\/usr\/sbin:\/usr\/bin:\/sbin:\/bin$/mu)
assert.doesNotMatch(workflow, /prepared-artifact-digest|artifact-digest/u)

const producerSource = readFileSync(
  resolve(root, 'scripts/ci/browsergate/build-protected-network-inputs.mjs'),
  'utf8',
)
assert.match(producerSource, /requireExactPreparedInventory\(outputDirectory, scheduledProfileNames\)/u)
assert.match(producerSource, /network-entry-bundle\.mjs/u)
assert.match(producerSource, /network-completion-bundle\.mjs/u)
assert.doesNotMatch(producerSource, /node_modules\.tar|(?:^|\s)tar\s+-[a-z]*[xf]/mu)
