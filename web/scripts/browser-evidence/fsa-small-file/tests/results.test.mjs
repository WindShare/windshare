import assert from 'node:assert/strict'
import test from 'node:test'
import {
  BASELINE_RESULT_SCHEMA,
  PRODUCT_RESULT_SCHEMA,
  summarizePairedEvidence,
  validateProductResult,
} from '../results.mjs'

const WORKLOAD_SHA256 = '1'.repeat(64)
const ENVIRONMENT = Object.freeze({
  evidenceSessionId: 'same-host-session-1',
  repositoryCommit: '2'.repeat(40),
  os: { platform: 'win32', release: 'test', architecture: 'x64' },
  hardware: { cpuModel: 'test cpu' },
  browser: { name: 'Edge', version: '151.0.0.0', executableSha256: '3'.repeat(64) },
  targetVolume: { fileSystem: 'NTFS', volumeType: 'NVMe', volumeIdentity: 'volume-test' },
})

function hostVerification(rootPath) {
  return {
    schema: 'windshare/fsa-small-file-host-verification/v1',
    status: 'verified',
    workloadSha256: WORKLOAD_SHA256,
    rootPath,
    verifiedAt: '2026-08-24T00:00:00.000Z',
    fileCount: 582,
    directoryCount: 105,
    totalBytes: 6_762_858,
    emptyFileCount: 31,
    mismatchCount: 0,
  }
}

function baseline(repetition, duration) {
  return {
    schema: BASELINE_RESULT_SCHEMA,
    runId: `baseline-${repetition}`,
    pairId: `pair-${repetition}`,
    repetition,
    warmup: repetition === 0,
    workloadSha256: WORKLOAD_SHA256,
    environment: structuredClone(ENVIRONMENT),
    concurrency: 8,
    hostVerification: hostVerification(`C:\\evidence\\baseline-${repetition}`),
    timing: {
      startMilestone: 'authority-acquired',
      endMilestone: 'baseline-complete',
      durationMilliseconds: duration,
      pickerWaitExcluded: true,
    },
    phaseDurations: {
      authorityToFirstWriteMilliseconds: duration,
      firstWriteToLastByteMilliseconds: 0,
      lastByteToCompletedMilliseconds: 0,
    },
    outcome: { lifecycle: 'Completed', bytesWritten: 6_762_858, route: 'pure-fsa' },
  }
}

function product(repetition, duration) {
  return {
    schema: PRODUCT_RESULT_SCHEMA,
    runId: `product-${repetition}`,
    pairId: `pair-${repetition}`,
    repetition,
    warmup: repetition === 0,
    workloadSha256: WORKLOAD_SHA256,
    environment: structuredClone(ENVIRONMENT),
    concurrency: 8,
    hostVerification: hostVerification(`C:\\evidence\\product-${repetition}`),
    timing: {
      startMilestone: 'authority-acquired',
      endMilestone: 'published',
      durationMilliseconds: duration,
      pickerWaitExcluded: true,
    },
    phaseDurations: {
      authorityToFirstContentRequestMilliseconds: duration,
      firstContentRequestToFirstWriteMilliseconds: 0,
      firstWriteToLastByteMilliseconds: 0,
      lastByteToLastFinalTransactionMilliseconds: 0,
      lastFinalTransactionToSettlementMilliseconds: 0,
      settlementToPublishedMilliseconds: 0,
    },
    outcome: {
      lifecycle: 'Published',
      bytesWritten: 6_762_858,
      artifactRoute: 'DirectTree',
      fallbackRoute: null,
      partial: false,
      needsAttention: false,
    },
  }
}

test('paired evidence gates the 15-second Published target while ratio remains diagnostic', () => {
  const summary = summarizePairedEvidence({
    baselineResults: [baseline(0, 9_000), baseline(1, 8_000), baseline(2, 8_100), baseline(3, 8_200)],
    productResults: [product(0, 13_000), product(1, 12_657), product(2, 13_287), product(3, 14_589)],
    now: () => new Date('2026-08-24T01:00:00.000Z'),
  })
  assert.equal(summary.warmupPairCount, 1)
  assert.equal(summary.measuredPairCount, 3)
  assert.equal(summary.durations.baseline.medianMilliseconds, 8_100)
  assert.equal(summary.durations.product.medianMilliseconds, 13_287)
  assert.deepEqual(Object.keys(summary).sort(), [
    'concurrency', 'createdAt', 'diagnostics', 'durations', 'environment', 'measuredPairCount',
    'pairs', 'performanceTarget', 'schema', 'warmupPairCount', 'workloadSha256',
  ].sort())
  assert.deepEqual(summary.performanceTarget, {
    authorityToPublishedMedianMilliseconds: 13_287,
    limitMilliseconds: 15_000,
    status: 'passed',
  })
  assert.equal(summary.diagnostics.productToBaselineMedianRatio, 13_287 / 8_100)
  assert.ok(summary.diagnostics.productToBaselineMedianRatio > 1.5)
})

test('15-second boundary is inclusive and the Published median alone determines status', () => {
  const atLimit = summarizePairedEvidence({
    baselineResults: [baseline(0, 9_000), baseline(1, 8_000), baseline(2, 8_100), baseline(3, 8_200)],
    productResults: [product(0, 13_000), product(1, 14_999), product(2, 15_000), product(3, 15_000)],
  })
  assert.equal(atLimit.performanceTarget.status, 'passed')

  const overLimit = summarizePairedEvidence({
    baselineResults: [baseline(0, 9_000), baseline(1, 8_000), baseline(2, 8_100), baseline(3, 8_200)],
    productResults: [product(0, 13_000), product(1, 14_999), product(2, 15_001), product(3, 16_000)],
  })
  assert.equal(overLimit.performanceTarget.status, 'failed')
})

test('ratio diagnostic must remain finite and positive', () => {
  assert.throws(() => summarizePairedEvidence({
    baselineResults: [baseline(0, Number.MIN_VALUE), baseline(1, Number.MIN_VALUE), baseline(2, Number.MIN_VALUE), baseline(3, Number.MIN_VALUE)],
    productResults: [product(0, 1_000), product(1, 1_000), product(2, 1_000), product(3, 1_000)],
  }), /finite positive diagnostic metric/)
})

test('bytes written cannot substitute for the Published product lifecycle', () => {
  const result = product(1, 11_000)
  result.outcome.lifecycle = 'Failed'
  assert.throws(() => validateProductResult(result), /lifecycle is Published/)
})

test('result contract rejects stale five-second evidence fields at any depth', () => {
  const result = product(1, 11_000)
  result.phaseDurations.fiveSecondBudgetMilliseconds = 5_000
  assert.throws(() => validateProductResult(result), /Stale five-second field is forbidden/)
})

test('pairing rejects different environment sessions', () => {
  const baselines = [baseline(0, 9_000), baseline(1, 8_000), baseline(2, 8_100), baseline(3, 8_200)]
  const products = [product(0, 13_000), product(1, 12_657), product(2, 13_287), product(3, 14_589)]
  products[2].environment.evidenceSessionId = 'different-session'
  assert.throws(() => summarizePairedEvidence({ baselineResults: baselines, productResults: products }), /same environment and session/)
})

test('pairing rejects environment drift between repetitions', () => {
  const baselines = [baseline(0, 9_000), baseline(1, 8_000), baseline(2, 8_100), baseline(3, 8_200)]
  const products = [product(0, 13_000), product(1, 12_657), product(2, 13_287), product(3, 14_589)]
  baselines[3].environment.browser.version = 'different-browser-version'
  products[3].environment.browser.version = 'different-browser-version'
  assert.throws(() => summarizePairedEvidence({ baselineResults: baselines, productResults: products }), /same environment and session/)
})
