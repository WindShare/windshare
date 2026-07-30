import {
  NETWORK_MATRIX_AGGREGATE_SCHEMA,
  NETWORK_MATRIX_EVIDENCE_OUTCOMES,
  NETWORK_MATRIX_EXECUTION_MODES,
  NETWORK_MATRIX_ID,
  NETWORK_MATRIX_IDENTITY_COUNTS,
  NETWORK_MATRIX_REPORTING_SEMANTICS,
  NETWORK_MATRIX_RUN_OUTCOMES,
  type NetworkMatrixExecutionMode,
  type NetworkMatrixRunOutcome,
} from './vocabulary.ts'
import type { LoadedNetworkMatrixRegistry } from './manifest.ts'
import { sha256 } from './manifest.ts'
import {
  canonicalNetworkRunResultJson,
  parseNetworkRunResult,
  type NetworkRunResult,
} from './result.ts'
import {
  networkMatrixError,
  parseNetworkMatrixJsonText,
  requireArray,
  requireCanonicalEncoding,
  requireEnum,
  requireExactKeys,
  requireLiteral,
  requireRecord,
  requireRunId,
  requireSafeInteger,
  requireSha256,
} from './contract-support.ts'

export interface NetworkMatrixAggregateRunReference {
  readonly executionMode: NetworkMatrixExecutionMode
  readonly runId: string
  readonly runSha256: string
  readonly runOutcome: NetworkMatrixRunOutcome
}

export interface NetworkMatrixAggregateCounts {
  readonly expectedIdentities: 60
  readonly observedSamples: number
  readonly matched: number
  readonly mismatched: number
  readonly notEvaluated: number
  readonly sampleInfrastructureFailures: number
}

export interface NetworkMatrixAggregate {
  readonly schemaVersion: typeof NETWORK_MATRIX_AGGREGATE_SCHEMA
  readonly matrixId: typeof NETWORK_MATRIX_ID
  readonly manifestSha256: string
  readonly reportingSemantics: typeof NETWORK_MATRIX_REPORTING_SEMANTICS
  readonly runs: readonly NetworkMatrixAggregateRunReference[]
  readonly counts: NetworkMatrixAggregateCounts
  readonly evidenceOutcome: (typeof NETWORK_MATRIX_EVIDENCE_OUTCOMES)[number]
}

export function aggregateNetworkMatrix(
  registry: LoadedNetworkMatrixRegistry,
  runInputs: readonly NetworkRunResult[],
): NetworkMatrixAggregate {
  const runs = realRuns(registry, runInputs)
  const references = Object.freeze(runs.map((run) => Object.freeze({
    executionMode: run.executionMode,
    runId: run.runId,
    runSha256: sha256(canonicalNetworkRunResultJson(run, registry)),
    runOutcome: run.runOutcome,
  })))
  const samples = runs.flatMap(({ samples }) => samples)
  const sampleInfrastructureFailures = samples.filter(
    ({ sampleOutcome }) => sampleOutcome === 'infrastructure-failed',
  ).length
  const counts = Object.freeze({
    expectedIdentities: NETWORK_MATRIX_IDENTITY_COUNTS.total,
    observedSamples: samples.length,
    matched: samples.filter(({ candidatePolicyOutcome }) => candidatePolicyOutcome === 'matched').length,
    mismatched: samples.filter(
      ({ candidatePolicyOutcome }) => candidatePolicyOutcome === 'mismatched',
    ).length,
    notEvaluated: samples.filter(
      ({ candidatePolicyOutcome }) => candidatePolicyOutcome === 'not-evaluated',
    ).length,
    sampleInfrastructureFailures,
  } as const)
  return Object.freeze({
    schemaVersion: NETWORK_MATRIX_AGGREGATE_SCHEMA,
    matrixId: NETWORK_MATRIX_ID,
    manifestSha256: registry.manifestSha256,
    reportingSemantics: NETWORK_MATRIX_REPORTING_SEMANTICS,
    runs: references,
    counts,
    evidenceOutcome: deriveEvidenceOutcome(runs, sampleInfrastructureFailures),
  })
}

export function parseNetworkMatrixAggregate(
  value: unknown,
  registry: LoadedNetworkMatrixRegistry,
  runInputs: readonly NetworkRunResult[],
): NetworkMatrixAggregate {
  const expected = aggregateNetworkMatrix(registry, runInputs)
  const record = requireRecord(value, 'browser network matrix aggregate')
  requireExactKeys(record, [
    'schemaVersion',
    'matrixId',
    'manifestSha256',
    'reportingSemantics',
    'runs',
    'counts',
    'evidenceOutcome',
  ], 'browser network matrix aggregate')
  const runReferences = requireArray(record.runs, 'browser network matrix aggregate runs')
  if (runReferences.length !== expected.runs.length) {
    networkMatrixError('browser network matrix aggregate run references differ from the real run inputs')
  }
  const runs = Object.freeze(runReferences.map((value, index) => {
    const reference = requireRecord(value, `browser network matrix aggregate run ${index}`)
    requireExactKeys(
      reference,
      ['executionMode', 'runId', 'runSha256', 'runOutcome'],
      `browser network matrix aggregate run ${index}`,
    )
    const expectedReference = expected.runs[index]
    if (expectedReference === undefined) networkMatrixError('aggregate run exceeds expected registry')
    return Object.freeze({
      executionMode: requireLiteral(
        reference.executionMode,
        expectedReference.executionMode,
        `aggregate run ${index} mode`,
      ),
      runId: requireLiteral(
        requireRunId(reference.runId, `aggregate run ${index} ID`),
        expectedReference.runId,
        `aggregate run ${index} ID`,
      ),
      runSha256: requireLiteral(
        requireSha256(reference.runSha256, `aggregate run ${index} digest`),
        expectedReference.runSha256,
        `aggregate run ${index} digest`,
      ),
      runOutcome: requireLiteral(
        requireEnum(reference.runOutcome, NETWORK_MATRIX_RUN_OUTCOMES, `aggregate run ${index} outcome`),
        expectedReference.runOutcome,
        `aggregate run ${index} outcome`,
      ),
    })
  }))
  const counts = parseCounts(record.counts, expected.counts)
  return Object.freeze({
    schemaVersion: requireLiteral(
      record.schemaVersion,
      NETWORK_MATRIX_AGGREGATE_SCHEMA,
      'browser network matrix aggregate schema',
    ),
    matrixId: requireLiteral(record.matrixId, NETWORK_MATRIX_ID, 'browser network matrix aggregate ID'),
    manifestSha256: requireLiteral(
      requireSha256(record.manifestSha256, 'browser network matrix aggregate manifest digest'),
      registry.manifestSha256,
      'browser network matrix aggregate manifest digest',
    ),
    reportingSemantics: requireLiteral(
      record.reportingSemantics,
      NETWORK_MATRIX_REPORTING_SEMANTICS,
      'browser network matrix aggregate reporting semantics',
    ),
    runs,
    counts,
    evidenceOutcome: requireLiteral(
      requireEnum(
        record.evidenceOutcome,
        NETWORK_MATRIX_EVIDENCE_OUTCOMES,
        'browser network matrix evidence outcome',
      ),
      expected.evidenceOutcome,
      'browser network matrix evidence outcome',
    ),
  })
}

export function parseNetworkMatrixAggregateJson(
  encoded: string,
  registry: LoadedNetworkMatrixRegistry,
  runs: readonly NetworkRunResult[],
): NetworkMatrixAggregate {
  return requireCanonicalEncoding(
    encoded,
    parseNetworkMatrixAggregate(
      parseNetworkMatrixJsonText(encoded, 'browser network matrix aggregate'),
      registry,
      runs,
    ),
    'browser network matrix aggregate',
  )
}

export function canonicalNetworkMatrixAggregateJson(
  aggregate: NetworkMatrixAggregate,
  registry: LoadedNetworkMatrixRegistry,
  runs: readonly NetworkRunResult[],
): string {
  return `${JSON.stringify(parseNetworkMatrixAggregate(aggregate, registry, runs))}\n`
}

function realRuns(
  registry: LoadedNetworkMatrixRegistry,
  inputs: readonly NetworkRunResult[],
): readonly NetworkRunResult[] {
  if (inputs.length === 0 || inputs.length > NETWORK_MATRIX_EXECUTION_MODES.length) {
    networkMatrixError('browser network matrix aggregate requires one or two real run inputs')
  }
  const runs = inputs.map((run) => parseNetworkRunResult(run, registry))
  if (new Set(runs.map(({ executionMode }) => executionMode)).size !== runs.length) {
    networkMatrixError('browser network matrix aggregate execution modes must be distinct')
  }
  if (new Set(runs.map(({ runId }) => runId)).size !== runs.length) {
    networkMatrixError('browser network matrix run IDs must be distinct')
  }
  return Object.freeze(runs.toSorted((left, right) =>
    NETWORK_MATRIX_EXECUTION_MODES.indexOf(left.executionMode) -
    NETWORK_MATRIX_EXECUTION_MODES.indexOf(right.executionMode)))
}

function parseCounts(
  value: unknown,
  expected: NetworkMatrixAggregateCounts,
): NetworkMatrixAggregateCounts {
  const counts = requireRecord(value, 'browser network matrix aggregate counts')
  requireExactKeys(counts, [
    'expectedIdentities',
    'observedSamples',
    'matched',
    'mismatched',
    'notEvaluated',
    'sampleInfrastructureFailures',
  ], 'browser network matrix aggregate counts')
  return Object.freeze({
    expectedIdentities: requireLiteral(
      counts.expectedIdentities,
      expected.expectedIdentities,
      'aggregate expected identity count',
    ),
    observedSamples: requireLiteral(
      requireSafeInteger(counts.observedSamples, 0, 60, 'aggregate observed sample count'),
      expected.observedSamples,
      'aggregate observed sample count',
    ),
    matched: requireLiteral(
      requireSafeInteger(counts.matched, 0, 60, 'aggregate matched count'),
      expected.matched,
      'aggregate matched count',
    ),
    mismatched: requireLiteral(
      requireSafeInteger(counts.mismatched, 0, 60, 'aggregate mismatched count'),
      expected.mismatched,
      'aggregate mismatched count',
    ),
    notEvaluated: requireLiteral(
      requireSafeInteger(counts.notEvaluated, 0, 60, 'aggregate not-evaluated count'),
      expected.notEvaluated,
      'aggregate not-evaluated count',
    ),
    sampleInfrastructureFailures: requireLiteral(
      requireSafeInteger(
        counts.sampleInfrastructureFailures,
        0,
        60,
        'aggregate sample infrastructure-failure count',
      ),
      expected.sampleInfrastructureFailures,
      'aggregate sample infrastructure-failure count',
    ),
  })
}

function deriveEvidenceOutcome(
  runs: readonly NetworkRunResult[],
  sampleInfrastructureFailures: number,
): NetworkMatrixAggregate['evidenceOutcome'] {
  if (
    sampleInfrastructureFailures !== 0 ||
    runs.some(({ runOutcome }) => runOutcome === 'infrastructure-failed')
  ) return 'infrastructure-failed'
  return runs.length === NETWORK_MATRIX_EXECUTION_MODES.length &&
    runs.every(({ runOutcome }) => runOutcome === 'completed')
    ? 'complete'
    : 'incomplete'
}
