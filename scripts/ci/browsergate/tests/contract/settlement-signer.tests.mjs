import assert from 'node:assert/strict'
import { join, resolve } from 'node:path'

import {
  canonicalSampleCommandSha256,
  createProcessSettlementSigner,
} from '../../process/settlement-signer.mjs'
import { verifyProcessSettlementAttestations } from '../../../../../web/scripts/browser-evidence/artifact/settlement-receipt.ts'
import { browserRunPolicy } from '../../../../../web/scripts/browser-evidence/run-policy.ts'

const NOW_UNIX_MS = 1_800_000_000_000
const CHECKOUT_SHA = 'b'.repeat(40)
const RESULT_BYTES = Buffer.from('{"resultStatus":"final-valid"}\n', 'utf8')
const REPOSITORY_ROOT = resolve(import.meta.dirname, '..', '..', '..', '..', '..')
const OUTPUT_ROOT = resolve('settlement-test-output')
const DRIVER_PATH = join(REPOSITORY_ROOT, 'web', 'scripts', 'browser-evidence', 'sample-driver.ts')
const PLAYWRIGHT_PATH = join(REPOSITORY_ROOT, 'web', 'node_modules', '@playwright', 'test', 'cli.js')
const command = Object.freeze({
  repository: Object.freeze({ root: REPOSITORY_ROOT, checkoutSha: CHECKOUT_SHA }),
  driver: Object.freeze({
    node: process.execPath,
    source: DRIVER_PATH,
    cwd: REPOSITORY_ROOT,
    environment: Object.freeze({ LANG: 'C.UTF-8', PATH: 'injected-path' }),
  }),
  identity: Object.freeze({
    runId: 'settlement-test-run',
    operationId: 'main-chromium-sample-1',
    scenario: 'browser-sample-main-chromium-1',
    runPolicy: browserRunPolicy('blocking'),
    suite: 'main',
    browser: 'chromium',
    sampleIndex: 1,
    checkoutSha: CHECKOUT_SHA,
  }),
  topology: Object.freeze({
    topologyId: 'test-topology',
    profilePath: resolve('profile.json'),
    profileSha256: 'e'.repeat(64),
    resolutionPath: resolve('resolution.json'),
    resolutionSha256: 'f'.repeat(64),
  }),
  runtime: Object.freeze({
    manifest: resolve('runtime.json'),
    processOwner: Object.freeze({
      kind: 'test-process-owner',
      path: resolve('test-process-owner'),
    }),
  }),
  output: Object.freeze({
    root: OUTPUT_ROOT,
    sampleDirectory: join(OUTPUT_ROOT, 'main', 'chromium', 'sample-1'),
    resultPath: join(OUTPUT_ROOT, 'main', 'chromium', 'sample-1', 'result.json'),
  }),
  ownership: Object.freeze({
    platform: 'linux',
    backend: 'inherited',
    outerAuthority: Object.freeze({
      kind: 'test-process-owner',
      backend: 'linux_subreaper',
      operationId: 'main-chromium-sample-1',
    }),
    operationClass: 'browser-sample',
    classDeadlineMs: 330_000,
    childDeadlineMs: 270_000,
  }),
  leaf: Object.freeze({
    executable: process.execPath,
    entrypoint: PLAYWRIGHT_PATH,
    arguments: Object.freeze([PLAYWRIGHT_PATH, 'test', '--project=chromium']),
    cwd: join(REPOSITORY_ROOT, 'web'),
    environment: Object.freeze({ LANG: 'C.UTF-8', PATH: 'injected-path' }),
  }),
})
const commandSha256 = canonicalSampleCommandSha256(command)
const signer = createProcessSettlementSigner({
  invocationId: 'settlement-test-invocation',
  now: () => NOW_UNIX_MS,
  createNonce: () => Buffer.alloc(32, 7),
})
const sample = Object.freeze({
  runId: 'settlement-test-run',
  runPolicy: browserRunPolicy('blocking'),
  suite: 'main',
  browser: 'chromium',
  sampleIndex: 1,
  checkoutSha: CHECKOUT_SHA,
})
const execution = Object.freeze({
  processEvidence: Object.freeze({ terminal: 'exited', exitCode: 0 }),
  treeEmpty: true,
  cleanupOutcome: 'completed',
  inputEvidence: Object.freeze({ outcome: 'delivered', failureCode: '', failureMessage: '' }),
  ownershipEvidence: Object.freeze({
    kind: 'test-process-owner',
    backend: 'linux_subreaper',
    terminationReason: 'natural',
  }),
})
const attestation = signer.signSample({
  sample,
  resultBytes: RESULT_BYTES,
  commandSha256,
  execution,
  ownershipBackend: 'linux_subreaper',
})

const verified = verifyProcessSettlementAttestations({
  trust: signer.trust,
  samples: [{ sample, resultBytes: RESULT_BYTES, commandSha256 }],
  attestations: [attestation],
  nowUnixMs: NOW_UNIX_MS,
})
assert.deepEqual(verified.sampleKeys, ['main/chromium/1'])
assert.deepEqual(Object.keys(signer).sort(), ['retire', 'signSample', 'trust'])
assert(!JSON.stringify(signer).includes('private'))
assert.notEqual(
  commandSha256,
  canonicalSampleCommandSha256({
    ...command,
    leaf: { ...command.leaf, arguments: [...command.leaf.arguments, '--retries=1'] },
  }),
)
assert.equal(
  commandSha256,
  canonicalSampleCommandSha256({
    ...command,
    driver: {
      ...command.driver,
      environment: { PATH: 'injected-path', LANG: 'C.UTF-8' },
    },
    leaf: {
      ...command.leaf,
      environment: { PATH: 'injected-path', LANG: 'C.UTF-8' },
    },
  }),
)

const dirtyAttestation = signer.signSample({
  sample,
  resultBytes: RESULT_BYTES,
  commandSha256,
  execution: Object.freeze({
    ...execution,
    treeEmpty: false,
    cleanupOutcome: 'failed',
  }),
  ownershipBackend: 'linux_subreaper',
})
assert.throws(
  () => verifyProcessSettlementAttestations({
    trust: signer.trust,
    samples: [{ sample, resultBytes: RESULT_BYTES, commandSha256 }],
    attestations: [dirtyAttestation],
    nowUnixMs: NOW_UNIX_MS,
  }),
  /did not prove clean quiescence/u,
)

signer.retire()
assert.throws(
  () => signer.signSample({
    sample,
    resultBytes: RESULT_BYTES,
    commandSha256,
    execution,
    ownershipBackend: 'linux_subreaper',
  }),
  /signer is retired/u,
)

process.stdout.write('process settlement signer contracts: PASS\n')
