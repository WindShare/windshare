import type { FailureFact } from '../incident/fact'
import { fingerprintFailureFact } from '../incident/fingerprint'
import type { FailureFactRef, FailureFactRelation } from '../incident/scope'
import { projectFailureFactV2 } from '../export/projector'
import type { FailureFactV2 } from '../export/incident-record-v2'
import { diagnosticBytes } from '../exception/text'

export const MAX_FAILURE_OBSERVATIONS = 64
export const MAX_FAILURE_OBSERVATION_BYTES = 262_144
export const MAX_FAILURE_OBSERVATION_CAUSES = 4

export interface FailureObservation {
  readonly sequence: string
  readonly time: string
  readonly count: string
  readonly relation: FailureFactRelation | 'observed'
  readonly scope?: Readonly<{ scope_kind: string; scope_sequence: string; fact_sequence: string }>
  readonly fact: FailureFactV2
}
export interface FailureEvidenceHistorySnapshot {
  readonly records: readonly FailureObservation[]
  readonly omitted: string
  readonly retainedBytes: number
}
export interface FailureEvidenceHistoryPort {
  record(fact: FailureFact, relation?: FailureFactRelation, ref?: FailureFactRef): void
  snapshot(): FailureEvidenceHistorySnapshot
  clear(): void
  subscribe(listener: () => void): () => void
}

interface Entry { readonly classification: string; readonly record: FailureObservation; readonly bytes: number }

/** Silent failure retention has no dependency on presentation decisions or trace activation. */
export class FailureEvidenceHistory implements FailureEvidenceHistoryPort {
  readonly #records = new Map<string, Entry>()
  readonly #listeners = new Set<() => void>()
  readonly #now: () => string
  #bytes = 0
  #sequence = 0n
  #omitted = 0n
  #snapshot: FailureEvidenceHistorySnapshot | undefined

  constructor(now: () => string = () => new Date().toISOString()) { this.#now = now }

  record(fact: FailureFact, relation?: FailureFactRelation, ref?: FailureFactRef): void {
    try {
      const classification = fingerprintFailureFact(fact)
      const key = classification + ':' + (fact.evidence?.signature ?? '-') + ':' + (relation ?? 'observed')
      const previous = this.#records.get(key)
      const record: FailureObservation = Object.freeze({
        sequence: (++this.#sequence).toString(10), time: this.#now(),
        count: (BigInt(previous?.record.count ?? '0') + 1n).toString(10),
        relation: relation ?? 'observed',
        ...(ref === undefined ? {} : { scope: Object.freeze({
          scope_kind: ref.scope.scopeKind, scope_sequence: ref.scope.scopeSequence.toString(10),
          fact_sequence: ref.factSequence.toString(10),
        }) }),
        fact: projectFailureFactV2(fact),
      })
      if (previous !== undefined) this.#remove(key, false)
      else {
        const sameClass = [...this.#records.entries()].filter(([, entry]) => entry.classification === classification)
        if (sameClass.length >= MAX_FAILURE_OBSERVATION_CAUSES) this.#remove(sameClass[0]![0], true)
      }
      const bytes = diagnosticBytes(record)
      if (bytes > MAX_FAILURE_OBSERVATION_BYTES) { this.#omitted++; this.#changed(); return }
      while (this.#records.size >= MAX_FAILURE_OBSERVATIONS || this.#bytes + bytes > MAX_FAILURE_OBSERVATION_BYTES) {
        const oldest = this.#records.keys().next().value
        if (oldest === undefined) break
        this.#remove(oldest, true)
      }
      this.#records.set(key, { classification, record, bytes })
      this.#bytes += bytes
      this.#changed()
    } catch {
      // Diagnostic sinks cannot interrupt the failure's owning workflow.
    }
  }

  snapshot(): FailureEvidenceHistorySnapshot {
    this.#snapshot ??= Object.freeze({
      records: Object.freeze([...this.#records.values()].map(entry => entry.record)),
      omitted: this.#omitted.toString(10), retainedBytes: this.#bytes,
    })
    return this.#snapshot
  }
  clear(): void { this.#records.clear(); this.#bytes = 0; this.#omitted = 0n; this.#changed() }
  subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener)
    return () => { this.#listeners.delete(listener) }
  }
  #remove(key: string, omitted: boolean): void {
    const entry = this.#records.get(key)
    if (entry === undefined) return
    this.#bytes -= entry.bytes
    if (omitted) this.#omitted += BigInt(entry.record.count)
    this.#records.delete(key)
  }
  #changed(): void {
    this.#snapshot = undefined
    for (const listener of this.#listeners) {
      try { listener() } catch { /* A subscriber has no authority over retention. */ }
    }
  }
}
