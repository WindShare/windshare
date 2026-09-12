import { INCIDENT_RECORD_SCHEMA_VERSION, type IncidentRecordV2 } from '../export/incident-record-v2'
import { isDeeplyFrozen } from '../export/json'
import {
  DEFAULT_INCIDENT_POLICY,
  createIncidentPolicy,
  type IncidentPolicy,
} from './policy'

export interface IncidentHistoryReadPort {
  last(): IncidentRecordV2 | null
  snapshot(): readonly IncidentRecordV2[]
}

export interface IncidentHistoryPort extends IncidentHistoryReadPort {
  nextAppendEvictionCount(): bigint
  append(record: IncidentRecordV2): void
  clear(): void
}

export class BoundedIncidentHistory implements IncidentHistoryPort {
  readonly #capacity: number
  readonly #records: IncidentRecordV2[] = []

  constructor(policy: IncidentPolicy = DEFAULT_INCIDENT_POLICY) {
    const snapshot = policy === DEFAULT_INCIDENT_POLICY
      ? DEFAULT_INCIDENT_POLICY
      : createIncidentPolicy(policy)
    this.#capacity = snapshot.maxIncidentHistoryRecords
  }

  nextAppendEvictionCount(): bigint {
    return this.#records.length >= this.#capacity ? 1n : 0n
  }

  append(record: IncidentRecordV2): void {
    if (
      record.schema_version !== INCIDENT_RECORD_SCHEMA_VERSION ||
      record.event !== 'failure_incident' ||
      !isDeeplyFrozen(record)
    ) {
      throw new TypeError('Incident history accepts only immutable V2 incident records')
    }
    if (this.#records.length >= this.#capacity) this.#records.shift()
    this.#records.push(record)
  }

  last(): IncidentRecordV2 | null {
    return this.#records.at(-1) ?? null
  }

  snapshot(): readonly IncidentRecordV2[] {
    return Object.freeze([...this.#records])
  }

  clear(): void {
    this.#records.splice(0)
  }
}
