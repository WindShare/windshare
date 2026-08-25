import { encodeBase64Url } from '../../crypto/bytes'
import type {
  FailureFact,
  ProtocolFailure,
} from '../incident/fact'
import type {
  FailureFactBucket,
  SealedFailureFacts,
} from '../incident/aggregate'
import {
  isAllowedPresentationOutcome,
  type PresentationBoundary,
  type PresentationOutcome,
} from '../incident/presentation'
import {
  DEFAULT_INCIDENT_POLICY,
  createIncidentPolicy,
  type IncidentPolicy,
} from '../incident/policy'
import type { IncidentDiagnosticsHealthSnapshot } from '../incident/health'
import { projectCorrelationV1 } from './correlation-v1'
import type { DiagnosticContextV1 } from './context'
import {
  PERFORMANCE_MILESTONES_V1,
  type PerformancePhasePayloadV1,
  type PerformancePhaseProjectionInput,
} from '../trace/transfer-payload'
import {
  projectPerformanceCorrelationV1,
} from './performance-projection'
export { projectPerformanceSummaryPayloadV1 } from './performance-projection'
import {
  INCIDENT_RECORD_EVENT,
  INCIDENT_RECORD_SCHEMA_VERSION,
  type BuildIdentityV1,
  type DiagnosticsHealthV1,
  type FailureFactBucketV1,
  type FailureFactV1,
  type FailureIncidentPayloadV1,
  type IncidentRecordV1,
  type LifecycleReasonV1,
  type LifecycleStateV1,
  type NativeOutputCodeV1,
  type PeerFailureCodeV1,
  type ProtocolFailureV1,
} from './incident-record-v1'
import {
  boundedUtf8String,
  decimalUint64,
  deepFreezeJson,
  jsonUtf8ByteLength,
  snakeCaseClosedValue,
  utcRfc3339,
} from './json'

export interface RuntimeRunIdentity {
  readonly byteLength: 16
  copyBytes(): Uint8Array
}

export interface BuildSnapshot {
  readonly version: string
  readonly revision?: string
  readonly mode: 'development' | 'production' | 'test'
}

export interface IncidentRecordProjectionInput {
  readonly sequence: bigint
  readonly time: string
  readonly elapsedMs: bigint
  readonly presentation: Readonly<{
    boundary: PresentationBoundary
    outcome: PresentationOutcome
  }>
  readonly facts: SealedFailureFacts
  readonly context: DiagnosticContextV1
  readonly health: IncidentDiagnosticsHealthSnapshot
  readonly rootIncidentSequence?: bigint
}

export interface IncidentRecordProjection {
  readonly record: IncidentRecordV1
  readonly overflowFactCount: bigint
}

export interface IncidentRecordProjector {
  project(input: IncidentRecordProjectionInput): IncidentRecordProjection
}

export interface IncidentRecordProjectorOptions {
  readonly runtimeRunId: RuntimeRunIdentity
  readonly build: BuildSnapshot
  readonly secureContext: boolean
  readonly policy?: IncidentPolicy
}

const RUNTIME_RUN_ID_BYTES = 16
const REVISION_PATTERN = /^[0-9a-f]{7,64}$/
const SEMVER_IDENTIFIER_PATTERN = /^[0-9A-Za-z-]+$/

class FixedRuntimeRunIdentity implements RuntimeRunIdentity {
  readonly byteLength = RUNTIME_RUN_ID_BYTES
  readonly #bytes: Uint8Array

  constructor(bytes: Uint8Array) {
    this.#bytes = bytes.slice()
    Object.freeze(this)
  }

  copyBytes(): Uint8Array {
    return this.#bytes.slice()
  }
}

class V1IncidentRecordProjector implements IncidentRecordProjector {
  readonly #runtimeRunId: string
  readonly #build: BuildIdentityV1
  readonly #runtime: Readonly<{ kind: 'browser'; secure_context: boolean }>
  readonly #policy: IncidentPolicy

  constructor(options: IncidentRecordProjectorOptions) {
    const runtimeIdentity = createRuntimeRunIdentity(options.runtimeRunId.copyBytes())
    this.#runtimeRunId = encodeBase64Url(runtimeIdentity.copyBytes())
    this.#policy = options.policy === undefined
      ? DEFAULT_INCIDENT_POLICY
      : createIncidentPolicy(options.policy)
    this.#build = projectBuild(options.build, this.#policy)
    if (typeof options.secureContext !== 'boolean') {
      throw new TypeError('secure context identity must be boolean')
    }
    this.#runtime = Object.freeze({
      kind: 'browser',
      secure_context: options.secureContext,
    })
  }

  project(input: IncidentRecordProjectionInput): IncidentRecordProjection {
    if (
      !isAllowedPresentationOutcome(
        input.presentation.boundary,
        input.presentation.outcome,
      )
    ) {
      throw new TypeError('Incident presentation is invalid')
    }
    const sequence = decimalUint64(input.sequence, 'incident sequence')
    const rootIncidentSequence = input.rootIncidentSequence === undefined
      ? undefined
      : decimalUint64(input.rootIncidentSequence, 'root incident sequence')
    if (
      input.rootIncidentSequence !== undefined &&
      (
        input.rootIncidentSequence === 0n ||
        input.rootIncidentSequence >= input.sequence
      )
    ) {
      throw new RangeError('Root incident sequence must precede its linked incident')
    }

    const trigger = projectFailureFactV1(input.facts.trigger.fact)
    const correlation = projectCorrelationV1(input.facts.trigger.fact.correlation)
    const contributors = input.facts.contributorBuckets.map((bucket) =>
      projectBucket(bucket, this.#policy))
    const consequences = input.facts.consequenceBuckets.map((bucket) =>
      projectBucket(bucket, this.#policy))
    let overflowFactCount =
      input.facts.contributorOverflowCount +
      input.facts.consequenceOverflowCount

    overflowFactCount += pruneList(
      contributors,
      this.#policy.maxRecordListItems,
    )
    overflowFactCount += pruneList(
      consequences,
      this.#policy.maxRecordListItems,
    )

    const buildRecord = (): IncidentRecordV1 => ({
      schema_version: INCIDENT_RECORD_SCHEMA_VERSION,
      sequence,
      time: utcRfc3339(input.time, 'incident time'),
      elapsed_ms: decimalUint64(input.elapsedMs, 'incident elapsed milliseconds'),
      level: 'error',
      event: INCIDENT_RECORD_EVENT,
      runtime_run_id: this.#runtimeRunId,
      ...(correlation === undefined ? {} : { correlation }),
      payload: {
        ...(rootIncidentSequence === undefined
          ? {}
          : { root_incident_sequence: rootIncidentSequence }),
        scope: {
          scope_kind: input.facts.scope.scopeKind,
          scope_sequence: decimalUint64(
            input.facts.scope.scopeSequence,
            'incident scope sequence',
          ),
        },
        presentation: {
          boundary: input.presentation.boundary,
          outcome: input.presentation.outcome,
        },
        build: this.#build,
        runtime: this.#runtime,
        trigger,
        contributors,
        consequences,
        fact_count: decimalUint64(input.facts.factCount, 'incident fact count'),
        overflow_fact_count: decimalUint64(
          overflowFactCount,
          'incident overflow fact count',
        ),
        context: input.context,
        diagnostics_health_at_seal: projectDiagnosticsHealthV1(input.health),
      } satisfies FailureIncidentPayloadV1,
    })

    let record = buildRecord()
    while (
      jsonUtf8ByteLength(record) > this.#policy.maxIncidentRecordBytes
    ) {
      const removed = consequences.pop() ?? contributors.pop()
      if (removed === undefined) {
        throw new RangeError('Incident trigger exceeds the record byte capacity')
      }
      overflowFactCount += BigInt(removed.count)
      record = buildRecord()
    }
    return Object.freeze({
      record: deepFreezeJson(record),
      overflowFactCount,
    })
  }
}

export function createRuntimeRunIdentity(
  bytes: Uint8Array,
): RuntimeRunIdentity {
  if (
    !(bytes instanceof Uint8Array) ||
    bytes.byteLength !== RUNTIME_RUN_ID_BYTES
  ) {
    throw new RangeError('Runtime run identity must be exactly 16 bytes')
  }
  if (bytes.every((value) => value === 0)) {
    throw new RangeError('Runtime run identity must be non-zero')
  }
  return new FixedRuntimeRunIdentity(bytes)
}

export function createIncidentRecordProjector(
  options: IncidentRecordProjectorOptions,
): IncidentRecordProjector {
  return new V1IncidentRecordProjector(options)
}

function projectBuild(
  snapshot: BuildSnapshot,
  policy: IncidentPolicy,
): BuildIdentityV1 {
  const version = boundedUtf8String(
    snapshot.version,
    'build version',
    policy.maxSafeStringUtf8Bytes,
  )
  if (!isSemanticVersion(version)) {
    throw new TypeError('Build version must be semantic version text')
  }
  if (
    snapshot.mode !== 'development' &&
    snapshot.mode !== 'production' &&
    snapshot.mode !== 'test'
  ) {
    throw new TypeError('Build mode is invalid')
  }
  const revision =
    snapshot.revision !== undefined &&
    REVISION_PATTERN.test(snapshot.revision) &&
    new TextEncoder().encode(snapshot.revision).byteLength <=
      policy.maxSafeStringUtf8Bytes
      ? snapshot.revision
      : undefined
  return deepFreezeJson({
    application: 'windshare_web',
    version,
    ...(revision === undefined ? {} : { revision }),
    mode: snapshot.mode,
  })
}

function isSemanticVersion(value: string): boolean {
  const buildParts = value.split('+')
  if (buildParts.length > 2) return false
  const releaseAndPrerelease = buildParts[0]!
  const prereleaseSeparator = releaseAndPrerelease.indexOf('-')
  const release = prereleaseSeparator < 0
    ? releaseAndPrerelease
    : releaseAndPrerelease.slice(0, prereleaseSeparator)
  const prerelease = prereleaseSeparator < 0
    ? undefined
    : releaseAndPrerelease.slice(prereleaseSeparator + 1)
  const releaseParts = release.split('.')
  if (
    releaseParts.length !== 3 ||
    !releaseParts.every(isCanonicalDecimalIdentifier)
  ) {
    return false
  }
  if (
    prerelease !== undefined &&
    !validSemanticIdentifiers(prerelease, true)
  ) {
    return false
  }
  const build = buildParts[1]
  return build === undefined || validSemanticIdentifiers(build, false)
}

function isCanonicalDecimalIdentifier(value: string): boolean {
  if (value.length === 0 || !/^\d+$/.test(value)) return false
  return value === '0' || !value.startsWith('0')
}

function validSemanticIdentifiers(
  value: string,
  rejectNumericLeadingZero: boolean,
): boolean {
  const identifiers = value.split('.')
  return identifiers.every((identifier) =>
    identifier.length > 0 &&
    SEMVER_IDENTIFIER_PATTERN.test(identifier) &&
    (
      !rejectNumericLeadingZero ||
      !/^\d+$/.test(identifier) ||
      isCanonicalDecimalIdentifier(identifier)
    ))
}

function projectFailureFactV1(fact: FailureFact): FailureFactV1 {
  const correlation = projectCorrelationV1(fact.correlation)
  const common = {
    kind: fact.kind,
    stage: fact.stage,
    recovery_disposition: fact.recoveryDisposition,
    ...(correlation === undefined ? {} : { correlation }),
  }
  switch (fact.kind) {
    case 'fault':
      return deepFreezeJson({
        ...common,
        kind: fact.kind,
        payload: {
          fault: {
            domain: fact.payload.fault.domain,
            scope: snakeCaseClosedValue(fact.payload.fault.scope),
            code: snakeCaseClosedValue(fact.payload.fault.code),
          },
        },
      }) as FailureFactV1
    case 'protocol_failure':
      return deepFreezeJson({
        ...common,
        kind: fact.kind,
        payload: {
          protocol_failure: projectProtocolFailureV1(
            fact.payload.protocolFailure,
          ),
        },
      })
    case 'peer_failure':
      return deepFreezeJson({
        ...common,
        kind: fact.kind,
        payload: {
          peer_failure: {
            scope: fact.payload.peerFailure.scope,
            code: snakeCaseClosedValue(
              fact.payload.peerFailure.code,
            ) as PeerFailureCodeV1,
            retryable: fact.payload.peerFailure.retryable,
          },
        },
      })
    case 'native_output_failure':
      return deepFreezeJson({
        ...common,
        kind: fact.kind,
        payload: {
          native_output_failure: {
            native_class: fact.payload.nativeOutputFailure.nativeClass,
            ...(fact.payload.nativeOutputFailure.code === undefined
              ? {}
              : {
                  code: snakeCaseClosedValue(
                    fact.payload.nativeOutputFailure.code,
                  ) as NativeOutputCodeV1,
                }),
          },
        },
      })
    case 'lifecycle_failure':
      return deepFreezeJson({
        ...common,
        kind: fact.kind,
        payload: {
          lifecycle_failure: {
            state: snakeCaseClosedValue(
              fact.payload.lifecycleFailure.kind,
            ) as LifecycleStateV1,
            ...(fact.payload.lifecycleFailure.reason === undefined
              ? {}
              : {
                  reason: snakeCaseClosedValue(
                    fact.payload.lifecycleFailure.reason,
                  ) as LifecycleReasonV1,
                }),
          },
        },
      })
    case 'unclassified':
      return deepFreezeJson({
        ...common,
        kind: fact.kind,
        payload: { unclassified: {} },
      })
  }
}

function projectProtocolFailureV1(
  failure: ProtocolFailure,
): ProtocolFailureV1 {
  const correlation = projectCorrelationV1(failure.correlation)
  if (correlation === undefined) {
    throw new TypeError('Protocol failure projection requires correlation')
  }
  return deepFreezeJson({
    request_kind: failure.requestKind,
    wire_scope: failure.wireScope,
    wire_code: failure.wireCode,
    retryable: failure.retryable,
    ...(failure.retryAfterMilliseconds === undefined
      ? {}
      : { retry_after_ms: failure.retryAfterMilliseconds }),
    settlement: failure.settlement.kind === 'received_authenticated'
      ? { kind: failure.settlement.kind }
      : {
          kind: failure.settlement.kind,
          admitted: failure.settlement.admitted,
          settled: failure.settlement.settled,
          outcome: failure.settlement.outcome,
        },
    correlation,
  })
}

function projectBucket(
  bucket: FailureFactBucket,
  policy: IncidentPolicy,
): FailureFactBucketV1 {
  return deepFreezeJson({
    fingerprint: boundedUtf8String(
      bucket.fingerprint.replaceAll('-', '_'),
      'failure fingerprint',
      policy.maxSafeStringUtf8Bytes,
    ),
    count: decimalUint64(bucket.count, 'failure bucket count'),
    representative: projectFailureFactV1(bucket.representative),
  })
}

function pruneList(
  buckets: FailureFactBucketV1[],
  maximumItems: number,
): bigint {
  let removedCount = 0n
  while (buckets.length > maximumItems) {
    const removed = buckets.pop()
    if (removed !== undefined) removedCount += BigInt(removed.count)
  }
  return removedCount
}

export function projectPerformancePhasePayloadV1(
  input: PerformancePhaseProjectionInput,
): PerformancePhasePayloadV1 {
  if (!PERFORMANCE_MILESTONES_V1.includes(input.milestone)) {
    throw new TypeError('performance milestone is outside its closed vocabulary')
  }
  return deepFreezeJson({
    correlation: projectPerformanceCorrelationV1(input.correlation),
    milestone: input.milestone,
    observer_elapsed_ms: decimalUint64(
      input.observerElapsedMilliseconds,
      'performance phase observer elapsed milliseconds',
    ),
  })
}

export function projectDiagnosticsHealthV1(
  health: IncidentDiagnosticsHealthSnapshot,
): DiagnosticsHealthV1 {
  return Object.freeze({
    fact_overflow_count: decimalUint64(
      health.factOverflowCount,
      'fact overflow health',
    ),
    incident_history_eviction_count: decimalUint64(
      health.incidentHistoryEvictionCount,
      'incident history eviction health',
    ),
    console_suppression_count: decimalUint64(
      health.consoleSuppressionCount,
      'console suppression health',
    ),
    late_link_eviction_count: decimalUint64(
      health.lateLinkEvictionCount,
      'late-link eviction health',
    ),
    trace_dropped_count: decimalUint64(
      health.traceDroppedCount,
      'trace dropped health',
    ),
    trace_overwritten_count: decimalUint64(
      health.traceOverwrittenCount,
      'trace overwritten health',
    ),
    trace_sampled_count: decimalUint64(
      health.traceSampledCount,
      'trace sampled health',
    ),
    trace_coalesced_count: decimalUint64(
      health.traceCoalescedCount,
      'trace coalesced health',
    ),
  })
}
