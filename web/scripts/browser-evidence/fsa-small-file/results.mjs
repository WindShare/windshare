import { isDeepStrictEqual } from 'node:util'
import {
  DIRECTORY_COUNT,
  EMPTY_FILE_COUNT,
  FILE_COUNT,
  TOTAL_BYTES,
} from './generate-workload.mjs'
import { HOST_VERIFICATION_SCHEMA } from './host-verification.mjs'

export const BASELINE_RESULT_SCHEMA = 'windshare/fsa-small-file-pure-fsa-result/v1'
export const PRODUCT_RESULT_SCHEMA = 'windshare/fsa-small-file-product-result/v1'
export const PAIRED_EVIDENCE_SCHEMA = 'windshare/fsa-small-file-paired-evidence/v1'
export const TARGET_CONCURRENCY = 8
export const AUTHORITY_TO_PUBLISHED_MEDIAN_LIMIT_MILLISECONDS = 15_000
export const MINIMUM_MEASURED_PAIRS = 3

const SHA256_PATTERN = /^[0-9a-f]{64}$/
const COMMIT_PATTERN = /^[0-9a-f]{40}$/
const STALE_FIVE_SECOND_FIELDS = new Set([
  'fiveSecondBudgetMilliseconds',
  'fiveSecondFeasibility',
  'pureFsaFiveSecondFeasibility',
  'pureFsaFiveSecondStatus',
])

function assertExactKeys(value, expected, label) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) throw new Error(`${label} must be an object`)
  const actual = Object.keys(value).sort()
  const wanted = [...expected].sort()
  if (actual.length !== wanted.length || actual.some((key, index) => key !== wanted[index])) {
    throw new Error(`${label} keys must be exactly: ${wanted.join(', ')}`)
  }
}

function rejectStaleFiveSecondFields(value, path = 'result') {
  if (value === null || typeof value !== 'object') return
  for (const [key, child] of Object.entries(value)) {
    if (STALE_FIVE_SECOND_FIELDS.has(key)) throw new Error(`Stale five-second field is forbidden: ${path}.${key}`)
    rejectStaleFiveSecondFields(child, `${path}.${key}`)
  }
}

function assertNonEmptyString(value, label) {
  if (typeof value !== 'string' || value.trim() === '') throw new Error(`${label} must be a non-empty string`)
}

function assertSha256(value, label) {
  if (typeof value !== 'string' || !SHA256_PATTERN.test(value)) throw new Error(`${label} must be SHA-256`)
}

function assertDuration(value, label) {
  if (typeof value !== 'number' || !Number.isFinite(value) || value < 0) throw new Error(`${label} must be a finite non-negative number`)
}

function validateEnvironment(environment) {
  assertExactKeys(environment, ['evidenceSessionId', 'repositoryCommit', 'os', 'hardware', 'browser', 'targetVolume'], 'result.environment')
  assertNonEmptyString(environment.evidenceSessionId, 'result.environment.evidenceSessionId')
  if (!COMMIT_PATTERN.test(environment.repositoryCommit)) throw new Error('result.environment.repositoryCommit must be a full Git commit')
  assertExactKeys(environment.os, ['platform', 'release', 'architecture'], 'result.environment.os')
  assertExactKeys(environment.hardware, ['cpuModel'], 'result.environment.hardware')
  assertExactKeys(environment.browser, ['name', 'version', 'executableSha256'], 'result.environment.browser')
  assertExactKeys(environment.targetVolume, ['fileSystem', 'volumeType', 'volumeIdentity'], 'result.environment.targetVolume')
  for (const [label, value] of [
    ['platform', environment.os.platform],
    ['release', environment.os.release],
    ['architecture', environment.os.architecture],
    ['cpuModel', environment.hardware.cpuModel],
    ['browser.name', environment.browser.name],
    ['browser.version', environment.browser.version],
    ['targetVolume.fileSystem', environment.targetVolume.fileSystem],
    ['targetVolume.volumeType', environment.targetVolume.volumeType],
    ['targetVolume.volumeIdentity', environment.targetVolume.volumeIdentity],
  ]) assertNonEmptyString(value, `result.environment.${label}`)
  assertSha256(environment.browser.executableSha256, 'result.environment.browser.executableSha256')
}

function validateHostVerification(hostVerification, workloadSha256) {
  assertExactKeys(hostVerification, [
    'schema', 'status', 'workloadSha256', 'rootPath', 'verifiedAt', 'fileCount', 'directoryCount',
    'totalBytes', 'emptyFileCount', 'mismatchCount',
  ], 'result.hostVerification')
  if (hostVerification.schema !== HOST_VERIFICATION_SCHEMA || hostVerification.status !== 'verified') {
    throw new Error('result.hostVerification must be a successful canonical host verification')
  }
  if (hostVerification.workloadSha256 !== workloadSha256) throw new Error('Host verification workload digest does not match the result')
  assertNonEmptyString(hostVerification.rootPath, 'result.hostVerification.rootPath')
  if (!Number.isFinite(Date.parse(hostVerification.verifiedAt))) throw new Error('result.hostVerification.verifiedAt must be an ISO timestamp')
  const expected = {
    fileCount: FILE_COUNT,
    directoryCount: DIRECTORY_COUNT,
    totalBytes: TOTAL_BYTES,
    emptyFileCount: EMPTY_FILE_COUNT,
    mismatchCount: 0,
  }
  for (const [name, value] of Object.entries(expected)) {
    if (hostVerification[name] !== value) throw new Error(`result.hostVerification.${name} must equal ${value}`)
  }
}

function validateCommon(result, schema) {
  rejectStaleFiveSecondFields(result)
  assertExactKeys(result, [
    'schema', 'runId', 'pairId', 'repetition', 'warmup', 'workloadSha256', 'environment',
    'concurrency', 'hostVerification', 'timing', 'phaseDurations', 'outcome',
  ], 'result')
  if (result.schema !== schema) throw new Error(`Unexpected result schema: ${result.schema}`)
  assertNonEmptyString(result.runId, 'result.runId')
  assertNonEmptyString(result.pairId, 'result.pairId')
  if (!Number.isSafeInteger(result.repetition) || result.repetition < 0) throw new Error('result.repetition must be a non-negative safe integer')
  if (typeof result.warmup !== 'boolean') throw new Error('result.warmup must be boolean')
  if (result.warmup !== (result.repetition === 0)) throw new Error('Only repetition zero may be the warm-up')
  assertSha256(result.workloadSha256, 'result.workloadSha256')
  validateEnvironment(result.environment)
  if (result.concurrency !== TARGET_CONCURRENCY) throw new Error(`result.concurrency must equal ${TARGET_CONCURRENCY}`)
  validateHostVerification(result.hostVerification, result.workloadSha256)
  return result
}

function validateTiming(timing, expectedEnd) {
  assertExactKeys(timing, ['startMilestone', 'endMilestone', 'durationMilliseconds', 'pickerWaitExcluded'], 'result.timing')
  if (timing.startMilestone !== 'authority-acquired') throw new Error('Timing must start at authority acquisition after picker return')
  if (timing.endMilestone !== expectedEnd) throw new Error(`Timing must end at ${expectedEnd}`)
  if (timing.pickerWaitExcluded !== true) throw new Error('Picker wait must be excluded from the measured interval')
  assertDuration(timing.durationMilliseconds, 'result.timing.durationMilliseconds')
  if (timing.durationMilliseconds === 0) throw new Error('Measured duration must be greater than zero')
}

function validatePhaseDurations(phaseDurations, expectedKeys, total) {
  assertExactKeys(phaseDurations, expectedKeys, 'result.phaseDurations')
  let sum = 0
  for (const key of expectedKeys) {
    assertDuration(phaseDurations[key], `result.phaseDurations.${key}`)
    sum += phaseDurations[key]
  }
  if (Math.abs(sum - total) > 0.001) throw new Error(`Phase durations must sum to the measured duration: sum=${sum} duration=${total}`)
}

export function validateBaselineResult(result) {
  validateCommon(result, BASELINE_RESULT_SCHEMA)
  validateTiming(result.timing, 'baseline-complete')
  validatePhaseDurations(result.phaseDurations, [
    'authorityToFirstWriteMilliseconds',
    'firstWriteToLastByteMilliseconds',
    'lastByteToCompletedMilliseconds',
  ], result.timing.durationMilliseconds)
  assertExactKeys(result.outcome, ['lifecycle', 'bytesWritten', 'route'], 'result.outcome')
  if (result.outcome.lifecycle !== 'Completed' || result.outcome.route !== 'pure-fsa' || result.outcome.bytesWritten !== TOTAL_BYTES) {
    throw new Error('Pure-FSA result must complete the canonical byte count through the pure-fsa route')
  }
  return result
}

export function validateProductResult(result) {
  validateCommon(result, PRODUCT_RESULT_SCHEMA)
  validateTiming(result.timing, 'published')
  validatePhaseDurations(result.phaseDurations, [
    'authorityToFirstContentRequestMilliseconds',
    'firstContentRequestToFirstWriteMilliseconds',
    'firstWriteToLastByteMilliseconds',
    'lastByteToLastFinalTransactionMilliseconds',
    'lastFinalTransactionToSettlementMilliseconds',
    'settlementToPublishedMilliseconds',
  ], result.timing.durationMilliseconds)
  assertExactKeys(result.outcome, [
    'lifecycle', 'bytesWritten', 'artifactRoute', 'fallbackRoute', 'partial', 'needsAttention',
  ], 'result.outcome')
  if (result.outcome.lifecycle !== 'Published') throw new Error('Bytes written are not successful evidence until product lifecycle is Published')
  if (result.outcome.bytesWritten !== TOTAL_BYTES) throw new Error(`Product result must write exactly ${TOTAL_BYTES} bytes`)
  if (result.outcome.artifactRoute !== 'DirectTree' || result.outcome.fallbackRoute !== null) {
    throw new Error('Product evidence must use DirectTree without ZIP or OriginPrivate fallback')
  }
  if (result.outcome.partial !== false || result.outcome.needsAttention !== false) {
    throw new Error('Published product evidence cannot be partial or need attention')
  }
  return result
}

function median(values) {
  const ordered = [...values].sort((left, right) => left - right)
  const middle = Math.floor(ordered.length / 2)
  return ordered.length % 2 === 0 ? (ordered[middle - 1] + ordered[middle]) / 2 : ordered[middle]
}

function summarizeDurations(values) {
  const ordered = [...values].sort((left, right) => left - right)
  return Object.freeze({
    minimumMilliseconds: ordered[0],
    medianMilliseconds: median(ordered),
    maximumMilliseconds: ordered.at(-1),
  })
}

export function summarizePairedEvidence({ baselineResults, productResults, now = () => new Date() }) {
  if (!Array.isArray(baselineResults) || !Array.isArray(productResults)) throw new Error('Baseline and product results must be arrays')
  const baselines = baselineResults.map(validateBaselineResult)
  const products = productResults.map(validateProductResult)
  if (baselines.length === 0 || products.length === 0) throw new Error('Paired evidence cannot be empty')
  const reference = baselines[0]
  for (const result of [...baselines, ...products]) {
    if (!isDeepStrictEqual(result.environment, reference.environment)) throw new Error('All evidence results must use the same environment and session')
    if (result.workloadSha256 !== reference.workloadSha256) throw new Error('All evidence results must use the same canonical workload')
    if (result.concurrency !== reference.concurrency) throw new Error('All evidence results must use the same concurrency')
  }
  const runIds = [...baselines, ...products].map((result) => result.runId)
  if (new Set(runIds).size !== runIds.length) throw new Error('Run IDs must be unique across paired evidence')
  const baselineByPair = new Map(baselines.map((result) => [result.pairId, result]))
  const productByPair = new Map(products.map((result) => [result.pairId, result]))
  if (baselineByPair.size !== baselines.length || productByPair.size !== products.length) throw new Error('Pair IDs must be unique within each result kind')
  if (baselines.length !== products.length || [...baselineByPair.keys()].some((pairId) => !productByPair.has(pairId))) {
    throw new Error('Every baseline result must have exactly one product result with the same pair ID')
  }
  const pairs = baselines.map((baseline) => {
    const product = productByPair.get(baseline.pairId)
    for (const [name, matches] of [
      ['environment', isDeepStrictEqual(baseline.environment, product.environment)],
      ['workload digest', baseline.workloadSha256 === product.workloadSha256],
      ['concurrency', baseline.concurrency === product.concurrency],
      ['repetition', baseline.repetition === product.repetition],
      ['warm-up classification', baseline.warmup === product.warmup],
    ]) if (!matches) throw new Error(`Paired result ${baseline.pairId} has mismatched ${name}`)
    return Object.freeze({
      pairId: baseline.pairId,
      repetition: baseline.repetition,
      warmup: baseline.warmup,
      baselineRunId: baseline.runId,
      productRunId: product.runId,
      baselineMilliseconds: baseline.timing.durationMilliseconds,
      productMilliseconds: product.timing.durationMilliseconds,
    })
  }).sort((left, right) => left.repetition - right.repetition)
  const warmups = pairs.filter((pair) => pair.warmup)
  const measured = pairs.filter((pair) => !pair.warmup)
  if (warmups.length !== 1) throw new Error('Evidence requires exactly one paired untimed warm-up')
  if (measured.length < MINIMUM_MEASURED_PAIRS) throw new Error(`Evidence requires at least ${MINIMUM_MEASURED_PAIRS} measured pairs`)
  const repetitions = measured.map((pair) => pair.repetition)
  if (new Set(repetitions).size !== repetitions.length || repetitions.some((value, index) => value !== index + 1)) {
    throw new Error('Measured repetitions must be unique and contiguous from one')
  }
  const baseline = summarizeDurations(measured.map((pair) => pair.baselineMilliseconds))
  const product = summarizeDurations(measured.map((pair) => pair.productMilliseconds))
  const productToBaselineMedianRatio = product.medianMilliseconds / baseline.medianMilliseconds
  if (!Number.isFinite(productToBaselineMedianRatio) || productToBaselineMedianRatio <= 0) {
    throw new Error('Product-to-baseline median ratio must be a finite positive diagnostic metric')
  }
  const targetPassed = product.medianMilliseconds <= AUTHORITY_TO_PUBLISHED_MEDIAN_LIMIT_MILLISECONDS
  return Object.freeze({
    schema: PAIRED_EVIDENCE_SCHEMA,
    createdAt: now().toISOString(),
    environment: reference.environment,
    workloadSha256: reference.workloadSha256,
    concurrency: TARGET_CONCURRENCY,
    warmupPairCount: warmups.length,
    measuredPairCount: measured.length,
    pairs: Object.freeze(pairs),
    durations: Object.freeze({ baseline, product }),
    performanceTarget: Object.freeze({
      authorityToPublishedMedianMilliseconds: product.medianMilliseconds,
      limitMilliseconds: AUTHORITY_TO_PUBLISHED_MEDIAN_LIMIT_MILLISECONDS,
      status: targetPassed ? 'passed' : 'failed',
    }),
    diagnostics: Object.freeze({
      productToBaselineMedianRatio,
    }),
  })
}
