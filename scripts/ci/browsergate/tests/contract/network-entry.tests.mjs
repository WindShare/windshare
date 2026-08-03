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

const workflow = readFileSync(resolve(root, '.github/workflows/browser-full.yml'), 'utf8')
assert.deepEqual(workflowJobNames(workflow), [
  'validate-target',
  'full-browser-network-prepare',
  'full-browser-network',
  'full-browser',
])
assert.match(workflow, /^\s{2}workflow_dispatch:$/mu)
assert.match(workflow, /^\s{2}schedule:$/mu)
assert.doesNotMatch(workflow, /^\s{2}(?:pull_request|push|workflow_call):/mu)
assert.match(workflow, /TARGET_REF_PROTECTED: \$\{\{ github\.ref_protected \}\}/u)
assert.match(workflow, /TARGET_DEFAULT_REF: refs\/heads\/\$\{\{ github\.event\.repository\.default_branch \}\}/u)
assert.match(workflow, /\[\[ "\$TARGET_SHA" =~ \^\[0-9a-f\]\{40\}\$ \]\]/u)
assert.equal(literalCount(workflow, 'id-token: write'), 1)
assert.equal(literalCount(workflow, 'environment: browser-network-matrix'), 1)
assert.equal(literalCount(workflow, '- self-hosted'), 2)
assert.equal(literalCount(workflow, 'run: make browser'), 1)
assert.equal(literalCount(workflow, 'WINDSHARE_TARGET_SHA: ${{ github.sha }}'), 1)
assert.match(
  workflow,
  /name: browser-full-\$\{\{ github\.sha \}\}-\$\{\{ github\.run_id \}\}-\$\{\{ github\.run_attempt \}\}/u,
)
assert.doesNotMatch(
  workflow,
  /current-commit|makeauthority|ci-full|browser-contract|browser-generated|browser-preflight/u,
)
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

const prepareJob = workflowJobSource(workflow, 'full-browser-network-prepare')
const capabilityJob = workflowJobSource(workflow, 'full-browser-network')
for (const [label, job, expectedActions] of [
  ['prepared-byte producer', prepareJob, [
    'actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1',
    'actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16',
    'pnpm/action-setup@b0f76dfb45f55f8421693e4803ac7bb65143bd34',
    'actions/setup-node@249970729cb0ef3589644e2896645e5dc5ba9c38',
    'actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a',
  ]],
  ['OIDC capability', capabilityJob, [
    'actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1',
    'actions/setup-node@249970729cb0ef3589644e2896645e5dc5ba9c38',
    'actions/download-artifact@3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c',
    'actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a',
  ]],
]) {
  const actions = workflowActions(job)
  assert.deepEqual(actions, expectedActions, `${label} action provenance changed`)
  for (const action of actions) {
    assert.match(action, /^[^@]+@[a-f0-9]{40}$/u, `${label} action is not full-SHA pinned: ${action}`)
  }
}
assert.equal(literalCount(capabilityJob, 'id-token: write'), 1)
assert.equal(literalCount(capabilityJob, 'environment: browser-network-matrix'), 1)
assert.doesNotMatch(
  workflowJobSource(workflow, 'full-browser-network-prepare'),
  /id-token: write|environment: browser-network-matrix/u,
)
assert.doesNotMatch(
  workflowJobSource(workflow, 'full-browser'),
  /id-token: write|environment: browser-network-matrix/u,
)

const producerSource = readFileSync(
  resolve(root, 'scripts/ci/browsergate/build-protected-network-inputs.mjs'),
  'utf8',
)
assert.match(producerSource, /checkoutSha = requiredEnvironment\('GITHUB_SHA', CHECKOUT_SHA_PATTERN\)/u)
assert.match(producerSource, /requireExactPreparedInventory\(outputDirectory, scheduledProfileNames\)/u)
assert.match(producerSource, /network-entry-bundle\.mjs/u)
assert.match(producerSource, /network-completion-bundle\.mjs/u)
assert.doesNotMatch(producerSource, /node_modules\.tar|(?:^|\s)tar\s+-[a-z]*[xf]/mu)

const completionSource = readFileSync(
  resolve(root, 'scripts/ci/browsergate/network-completion.mjs'),
  'utf8',
)
assert.match(completionSource, /takeEnvironment\(environment, 'WINDSHARE_TARGET_SHA'\)/u)
assert.doesNotMatch(completionSource, /WINDSHARE_CORE_ARTIFACT_COMMIT_SHA/u)

function workflowJobNames(source) {
  const jobsStart = source.indexOf('\njobs:\n')
  assert.notEqual(jobsStart, -1, 'workflow jobs mapping is unavailable')
  return [...source.slice(jobsStart + 1).matchAll(/^  ([a-z][a-z0-9-]*):$/gmu)]
    .map((match) => match[1])
}

function workflowJobSource(source, jobName) {
  const startToken = `  ${jobName}:\n`
  const start = source.indexOf(startToken)
  assert.notEqual(start, -1, `workflow job is unavailable: ${jobName}`)
  const remainder = source.slice(start + startToken.length)
  const next = remainder.search(/^  [a-z][a-z0-9-]*:$/mu)
  return next === -1 ? remainder : remainder.slice(0, next)
}

function workflowActions(jobSource) {
  return [...jobSource.matchAll(/^[ \t]+(?:- )?uses: ([^\s]+)(?:[ \t]+#.*)?$/gmu)]
    .map((match) => match[1])
}

function literalCount(source, literal) {
  return source.split(literal).length - 1
}
