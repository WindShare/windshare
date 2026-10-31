import { INCIDENT_RECORD_SCHEMA_VERSION, type IncidentRecordV2 } from '../export/incident-record-v2'
import { isDeeplyFrozen } from '../export/json'
import { validateIncidentConsequencesV2, type IncidentConsequencesV2 } from './consequences'
import {
  DEFAULT_INCIDENT_POLICY,
  createIncidentPolicy,
  type IncidentPolicy,
} from './policy'

export interface IncidentHistoryReadPort {
  last(): IncidentRecordV2 | null
  snapshot(): readonly IncidentRecordV2[]
  consequencesSnapshot(): readonly IncidentConsequencesV2[]
}

export interface IncidentHistoryPort extends IncidentHistoryReadPort {
  nextAppendEvictionCount(): bigint
  append(record: IncidentRecordV2): void
  recordConsequences(record: IncidentConsequencesV2): boolean
  clear(): void
}

export class BoundedIncidentHistory implements IncidentHistoryPort {
  readonly #listeners = new Set<() => void>()
  readonly #maximumRecordBytes: number
  readonly #capacity: number
  readonly #records: IncidentRecordV2[] = []
  readonly #consequences = new Map<string, IncidentConsequencesV2>()

  constructor(policy: IncidentPolicy = DEFAULT_INCIDENT_POLICY) {
    const snapshot = policy === DEFAULT_INCIDENT_POLICY
      ? DEFAULT_INCIDENT_POLICY
      : createIncidentPolicy(policy)
    this.#maximumRecordBytes = snapshot.maxIncidentRecordBytes
    this.#capacity = snapshot.maxIncidentHistoryRecords
  }

  subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener)
    return () => { this.#listeners.delete(listener) }
  }

  nextAppendEvictionCount(): bigint {
    return this.#records.length >= this.#capacity ? 1n : 0n
  }

  append(record: IncidentRecordV2): void {
    if (
      record.schema_version !== INCIDENT_RECORD_SCHEMA_VERSION ||
      record.event !== 'failure_incident' ||
      !isDeeplyFrozen(record) ||
      new TextEncoder().encode(JSON.stringify(record)).byteLength > this.#maximumRecordBytes
    ) {
      throw new TypeError('Incident history accepts only immutable V2 incident records')
    }
    if (this.#records.length >= this.#capacity) {
      const evicted = this.#records.shift()!
      this.#consequences.delete(evicted.sequence)
    }
    this.#records.push(record)
    this.#changed()
  }

  recordConsequences(record: IncidentConsequencesV2): boolean {
    const root = this.#records.find(incident => incident.sequence === record.incident_sequence)
    // Late work cannot resurrect an evicted root or displace an unrelated incident.
    if (root === undefined) return false
    validateIncidentConsequencesV2(record, root)
    if (new TextEncoder().encode(JSON.stringify(record)).byteLength > this.#maximumRecordBytes) {
      throw new TypeError('Incident consequences exceed the configured record byte capacity')
    }
    this.#consequences.set(root.sequence, record)
    this.#changed()
    return true
  }

  #changed(): void {
    for (const listener of this.#listeners) {
      try { listener() } catch { /* History remains available if a subscriber fails. */ }
    }
  }

  last(): IncidentRecordV2 | null {
    return this.#records.at(-1) ?? null
  }

  snapshot(): readonly IncidentRecordV2[] {
    return Object.freeze([...this.#records])
  }

  consequencesSnapshot(): readonly IncidentConsequencesV2[] {
    return Object.freeze(this.#records.flatMap(record => {
      const consequences = this.#consequences.get(record.sequence)
      return consequences === undefined ? [] : [consequences]
    }))
  }

  clear(): void {
    this.#records.splice(0)
    this.#consequences.clear()
    this.#changed()
  }
}
