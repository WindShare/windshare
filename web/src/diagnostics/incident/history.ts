import type { IncidentRecordV1 } from '../export/incident-record-v1'
import { isDeeplyFrozen } from '../export/json'
import {
  DEFAULT_INCIDENT_POLICY,
  createIncidentPolicy,
  type IncidentPolicy,
} from './policy'

export interface IncidentHistoryReadPort {
  last(): IncidentRecordV1 | null
  snapshot(): readonly IncidentRecordV1[]
}

export interface IncidentHistoryPort extends IncidentHistoryReadPort {
  nextAppendEvictionCount(): bigint
  append(record: IncidentRecordV1): void
  clear(): void
}

export class BoundedIncidentHistory implements IncidentHistoryPort {
  readonly #capacity: number
  readonly #records: IncidentRecordV1[] = []

  constructor(policy: IncidentPolicy = DEFAULT_INCIDENT_POLICY) {
    const snapshot = policy === DEFAULT_INCIDENT_POLICY
      ? DEFAULT_INCIDENT_POLICY
      : createIncidentPolicy(policy)
    this.#capacity = snapshot.maxIncidentHistoryRecords
  }

  nextAppendEvictionCount(): bigint {
    return this.#records.length >= this.#capacity ? 1n : 0n
  }

  append(record: IncidentRecordV1): void {
    if (
      record.schema_version !== 1 ||
      record.event !== 'failure_incident' ||
      !isDeeplyFrozen(record)
    ) {
      throw new TypeError('Incident history accepts only immutable V1 incident records')
    }
    if (this.#records.length >= this.#capacity) this.#records.shift()
    this.#records.push(record)
  }

  last(): IncidentRecordV1 | null {
    return this.#records.at(-1) ?? null
  }

  snapshot(): readonly IncidentRecordV1[] {
    return Object.freeze([...this.#records])
  }

  clear(): void {
    this.#records.splice(0)
  }
}
