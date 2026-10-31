import type { AggregatedConsequences, FailureFactBucket } from './aggregate'
import type { IncidentPolicy } from './policy'
import type { IncidentScopeIdentity } from './scope'
import { hasFailureEvidenceDetails, summarizeFailureFact } from '../failure/summary'
import type { FailureFactBucketV2, IncidentRecordV2 } from '../export/incident-record-v2'
import { decimalUint64, deepFreezeJson, isDeeplyFrozen, jsonUtf8ByteLength, utcRfc3339 } from '../export/json'
import { projectFailureFactBucketV2 } from '../export/projector'

export const MAX_INCIDENT_CONSEQUENCE_RECORD_BYTES = 16_384

export interface ConsequenceObservationTimeV2 {
  readonly time: string
  readonly elapsed_ms: string
}

/** A cumulative supplement belongs to one sealed incident, never a new failure. */
export interface IncidentConsequencesV2 {
  readonly event: 'incident_consequences'
  readonly runtime_run_id: string
  readonly incident_sequence: string
  readonly scope: IncidentRecordV2['payload']['scope']
  readonly first_observed: ConsequenceObservationTimeV2
  readonly last_observed: ConsequenceObservationTimeV2
  readonly fact_count: string
  readonly consequences: readonly FailureFactBucketV2[]
  readonly overflow_fact_count: string
  readonly evidence_details_omitted: number
}

export function validateIncidentConsequencesV2(record: IncidentConsequencesV2, root: IncidentRecordV2): void {
  if (record.event !== 'incident_consequences' || !isDeeplyFrozen(record) ||
      record.runtime_run_id !== root.runtime_run_id || record.incident_sequence !== root.sequence ||
      record.scope.scope_kind !== root.payload.scope.scope_kind ||
      record.scope.scope_sequence !== root.payload.scope.scope_sequence ||
      BigInt(record.first_observed.elapsed_ms) < BigInt(root.elapsed_ms) ||
      BigInt(record.last_observed.elapsed_ms) < BigInt(record.first_observed.elapsed_ms) ||
      record.consequences.reduce((count, bucket) => count + BigInt(bucket.count),
        BigInt(record.overflow_fact_count)) !== BigInt(record.fact_count) ||
      jsonUtf8ByteLength(record) > MAX_INCIDENT_CONSEQUENCE_RECORD_BYTES) {
    throw new TypeError('Incident consequences must be an immutable bounded supplement of their retained root')
  }
}

export function summarizeIncidentConsequencesV2(record: IncidentConsequencesV2): IncidentConsequencesV2 {
  return Object.freeze({
    ...record,
    consequences: Object.freeze([]),
    overflow_fact_count: record.fact_count,
    evidence_details_omitted: record.evidence_details_omitted +
      record.consequences.filter(bucket => hasFailureEvidenceDetails(bucket.representative.evidence)).length,
  })
}

export function projectIncidentConsequencesV2(input: {
  readonly runtimeRunId: string
  readonly incidentSequence: bigint
  readonly scope: IncidentScopeIdentity
  readonly firstObserved: Readonly<{ time: string; elapsedMs: bigint }>
  readonly lastObserved: Readonly<{ time: string; elapsedMs: bigint }>
  readonly facts: AggregatedConsequences
  readonly policy: IncidentPolicy
}): Readonly<{ record: IncidentConsequencesV2; omittedBuckets: readonly FailureFactBucket[] }> {
  const consequences = input.facts.buckets.map(bucket => projectFailureFactBucketV2(bucket, input.policy))
  let overflow = input.facts.overflowCount
  let evidenceDetailsOmitted = 0
  while (consequences.length > input.policy.maxRecordListItems) {
    overflow += BigInt(consequences.pop()!.count)
  }
  const build = (): IncidentConsequencesV2 => ({
    event: 'incident_consequences',
    runtime_run_id: input.runtimeRunId,
    incident_sequence: decimalUint64(input.incidentSequence, 'consequence owner'),
    scope: {
      scope_kind: input.scope.scopeKind,
      scope_sequence: decimalUint64(input.scope.scopeSequence, 'consequence scope'),
    },
    first_observed: observationTime(input.firstObserved),
    last_observed: observationTime(input.lastObserved),
    fact_count: decimalUint64(input.facts.factCount, 'late consequence count'),
    consequences,
    overflow_fact_count: decimalUint64(overflow, 'late consequence overflow'),
    evidence_details_omitted: evidenceDetailsOmitted,
  })
  const maximumBytes = Math.min(MAX_INCIDENT_CONSEQUENCE_RECORD_BYTES, input.policy.maxIncidentRecordBytes)
  let record = build()
  if (jsonUtf8ByteLength(record) > maximumBytes) {
    for (let index = 0; index < consequences.length; index++) {
      const bucket = consequences[index]!
      const summary = summarizeFailureFact(bucket.representative)
      if (summary.evidenceDetailsOmitted === 0) continue
      consequences[index] = { ...bucket, representative: summary.fact }
      evidenceDetailsOmitted += summary.evidenceDetailsOmitted
    }
    record = build()
  }
  while (jsonUtf8ByteLength(record) > maximumBytes) {
    const removed = consequences.pop()
    if (removed === undefined) throw new RangeError('Consequence summary exceeds record byte capacity')
    overflow += BigInt(removed.count)
    record = build()
  }
  return Object.freeze({
    record: deepFreezeJson(record),
    omittedBuckets: Object.freeze(input.facts.buckets.slice(consequences.length)),
  })
}

function observationTime(input: Readonly<{ time: string; elapsedMs: bigint }>): ConsequenceObservationTimeV2 {
  return {
    time: utcRfc3339(input.time, 'consequence observation time'),
    elapsed_ms: decimalUint64(input.elapsedMs, 'consequence observation elapsed milliseconds'),
  }
}
