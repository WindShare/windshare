import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

import { runBrowserNetworkEntry } from '../../network-entry.mjs'

const root = resolve(import.meta.dirname, '..', '..', '..', '..', '..')
const secretSentinels = Object.freeze({
  ACTIONS_ID_TOKEN_REQUEST_TOKEN: 'network-entry-secret-token-must-not-leak',
  ACTIONS_ID_TOKEN_REQUEST_URL: 'https://secret.invalid/?value=network-entry-secret-url-must-not-leak',
})
const profileResult = (profileId, executionMode, authorityId) => ({
  profileId,
  executionMode,
  authorityId,
  prerequisiteOutcome: 'unavailable',
  failureCode: 'authority-not-provisioned',
  expectedSamples: 15,
  observedSamples: 0,
  profileOutcome: 'not-executed',
})
const noConfigEvidence = {
  schemaVersion: 'windshare.browser-network-matrix.local-entry/v1',
  component: 'browser-network-entry',
  reportingSemantics: 'observational-nonblocking',
  outcome: 'not-executed',
  blocking: false,
  reason: 'external-authorities-not-provisioned',
  counts: { expectedSamples: 60, observedSamples: 0 },
  profileResults: [
    profileResult('scheduled-public-stun', 'scheduled', 'public-stun-external-fixture'),
    profileResult('scheduled-restricted-udp', 'scheduled', 'restricted-udp-external-fixture'),
    profileResult('scheduled-coturn', 'scheduled', 'coturn-external-fixture'),
    profileResult('manual-real-nat', 'manual', 'real-nat-external-fixture'),
  ],
  nextStep:
    'Build helpers into an explicit new absolute directory, then pass an explicit execute or aggregate command.',
}
const canonicalNoConfigEvidence = `${JSON.stringify(noConfigEvidence)}\n`
const summaries = []
let matrixAuthorityResolved = false
assert.equal(await runBrowserNetworkEntry([], {
  write: (encoded) => summaries.push(encoded),
  get runMatrixCli() {
    matrixAuthorityResolved = true
    throw new Error('no-config execution must not resolve the matrix CLI authority')
  },
}), 0)
assert.equal(matrixAuthorityResolved, false)
assert.deepEqual(summaries, [canonicalNoConfigEvidence])
assert.deepEqual(JSON.parse(summaries[0]), noConfigEvidence)
for (const secret of Object.values(secretSentinels)) assert.equal(summaries[0].includes(secret), false)

const forwarded = []
assert.equal(await runBrowserNetworkEntry([
  'aggregate',
  '--manifest',
  'manifest.json',
], {
  write: () => assert.fail('explicit commands must not emit no-config evidence'),
  runMatrixCli: async (arguments_) => {
    forwarded.push(arguments_)
    return 7
  },
}), 7)
assert.deepEqual(forwarded, [[
  'aggregate',
  '--manifest',
  'manifest.json',
]])

let configuredCalls = 0
await assert.rejects(
  runBrowserNetworkEntry(['execute', '--runtime-config', 'invalid.json'], {
    write: () => assert.fail('configured execution must not emit no-config evidence'),
    runMatrixCli: async () => {
      configuredCalls += 1
      throw new Error('configured-runtime-invalid')
    },
  }),
  /configured-runtime-invalid/u,
)
assert.equal(configuredCalls, 1)

for (const relativePath of [
  'scripts/ci/browser-network.ps1',
  'scripts/ci/browser-network.sh',
]) {
  const source = readFileSync(resolve(root, relativePath), 'utf8')
  assert.match(source, /browsergate[\\/]network-entry\.mjs/u)
  assert.doesNotMatch(source, /docker|podman|container image|oci\b/iu)
}

const makefile = readFileSync(resolve(root, 'Makefile'), 'utf8')
assert.match(
  makefile,
  /LOCAL_ENTRYPOINTS := check browser-contract browser-stability browser-network workflow-lint/u,
)
