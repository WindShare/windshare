import assert from 'node:assert/strict'
import { join, resolve } from 'node:path'

import {
  canonicalSampleCommandSha256,
  createProcessSettlementSigner,
} from './settlement-signer.mjs'
import { verifyProcessSettlementAttestations } from '../../../../web/scripts/browser-evidence/artifact/settlement-receipt.ts'
import { browserRunPolicy } from '../../../../web/scripts/browser-evidence/run-policy.ts'

const NOW_UNIX_MS = 1_800_000_000_000
const RUNTIME_MANIFEST_SHA256 = 'a'.repeat(64)
const CHECKOUT_SHA = 'b'.repeat(40)
const RESULT_BYTES = Buffer.from('{"resultStatus":"final-valid"}\n', 'utf8')
const REPOSITORY_ROOT = resolve('.')
const OUTPUT_ROOT = resolve('settlement-test-output')
const DRIVER_PATH = join(REPOSITORY_ROOT, 'web', 'scripts', 'browser-evidence', 'sample-driver.ts')
const PLAYWRIGHT_PATH = join(REPOSITORY_ROOT, 'web', 'node_modules', '@playwright', 'test', 'cli.js')
const command = Object.freeze({
  repository: Object.freeze({ root: REPOSITORY_ROOT, checkoutSha: CHECKOUT_SHA }),
  driver: Object.freeze({
    node: Object.freeze({ path: process.execPath, byteLength: 1, sha256: 'c'.repeat(64) }),
    source: Object.freeze({ path: DRIVER_PATH, byteLength: 1, sha256: 'd'.repeat(64) }),
    cwd: REPOSITORY_ROOT,
    environment: Object.freeze({ LANG: 'C.UTF-8', PATH: 'injected-path' }),
  }),
  identity: Object.freeze({
    runId: 'settlement-test-run',
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
    manifest: Object.freeze({ path: resolve('runtime.json'), byteLength: 1, sha256: RUNTIME_MANIFEST_SHA256 }),
    processOwner: Object.freeze({
      kind: 'linux-process-owner',
      path: resolve('linux-process-owner'),
      byteLength: 1,
      sha256: '1'.repeat(64),
    }),
  }),
  output: Object.freeze({
    root: OUTPUT_ROOT,
    sampleDirectory: join(OUTPUT_ROOT, 'main', 'chromium', 'sample-1'),
    resultPath: join(OUTPUT_ROOT, 'main', 'chromium', 'sample-1', 'result.json'),
  }),
  ownership: Object.freeze({
    platform: 'linux',
    insideWindowsD5: false,
    backend: 'linux-subreaper',
    operationClass: 'browser-sample',
    classDeadlineMs: 330_000,
    childDeadlineMs: 270_000,
  }),
  leaf: Object.freeze({
    executable: Object.freeze({ path: process.execPath, byteLength: 1, sha256: 'c'.repeat(64) }),
    entrypoint: Object.freeze({ path: PLAYWRIGHT_PATH, byteLength: 1, sha256: '2'.repeat(64) }),
    arguments: Object.freeze([PLAYWRIGHT_PATH, 'test', '--project=chromium']),
    cwd: join(REPOSITORY_ROOT, 'web'),
    environment: Object.freeze({ LANG: 'C.UTF-8', PATH: 'injected-path' }),
  }),
})
const commandSha256 = canonicalSampleCommandSha256(command)
const signer = createProcessSettlementSigner({
  invocationId: 'settlement-test-invocation',
  runtimeManifestSha256: RUNTIME_MANIFEST_SHA256,
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
  timedOut: false,
  launched: true,
  treeEmpty: true,
  inputEvidence: Object.freeze({ outcome: 'delivered', failureCode: '', failureMessage: '' }),
  clientIoEvidence: Object.freeze({
    requestOutcome: 'delivered',
    rawInputOutcome: 'delivered',
    controlOutcome: 'not-requested',
    outputOutcome: 'delivered',
    failureCode: '',
    failureMessage: '',
  }),
  ownershipEvidence: Object.freeze({
    ownerPid: 10,
    rootPid: 11,
    rootStartTimeTicks: '12',
    inventoryScans: 2,
    maximumObservedDescendants: 0,
    quietInventoryCount: 2,
    controlOutcome: 'target-terminal',
    cleanupOutcome: 'completed',
    failureCode: '',
    failureMessage: '',
  }),
})
const attestation = signer.signSample({
  sample,
  resultBytes: RESULT_BYTES,
  commandSha256,
  execution,
  ownershipBackend: 'linux-subreaper',
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
    ownershipEvidence: Object.freeze({
      ...execution.ownershipEvidence,
      quietInventoryCount: 0,
      cleanupOutcome: 'failed',
      failureCode: 'OWNERSHIP_EVIDENCE_LOST',
      failureMessage: 'injected cleanup failure',
    }),
  }),
  ownershipBackend: 'linux-subreaper',
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
    ownershipBackend: 'linux-subreaper',
  }),
  /signer is retired/u,
)

process.stdout.write('process settlement signer contracts: PASS\n')
