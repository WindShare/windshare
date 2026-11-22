import {
  isBoundedDiagnosticExceptionProjection,
  MAX_EXCEPTION_TEXT_BYTES,
  type DiagnosticExceptionProjection,
} from '../exception'
import type { IncidentScopeKind } from '../incident/scope'
import type { CorrelationV1 } from './correlation-v1'

export interface LocalOutputOperationFailureV1 {
  readonly schemaVersion: 1
  readonly recordKind: 'local_output_operation_failure'
  readonly stageFailure: Readonly<{
    schemaVersion: 1
    sequence: number
    correlation: Readonly<{
      operationId: string
      outputSessionId: string
      artifactId: string
    }>
    exception: DiagnosticExceptionProjection
  }>
}

export interface CorrelatedLocalOutputOperationFailureV1 {
  readonly owningScope: Readonly<{ scopeKind: IncidentScopeKind; scopeSequence: string }>
  readonly correlation: CorrelationV1
  readonly failure: LocalOutputOperationFailureV1
}

export interface LocalOutputOperationFailureReadPort {
  snapshot(): readonly CorrelatedLocalOutputOperationFailureV1[]
  subscribe(listener: () => void): () => void
}

export interface LocalOutputOperationFailureDiagnosticsPort extends LocalOutputOperationFailureReadPort {
  clear(): void
}

export class BoundedLocalOutputOperationFailureHistory implements LocalOutputOperationFailureDiagnosticsPort {
  readonly #records: CorrelatedLocalOutputOperationFailureV1[] = []
  readonly #listeners = new Set<() => void>()

  snapshot(): readonly CorrelatedLocalOutputOperationFailureV1[] {
    return Object.freeze([...this.#records])
  }

  subscribe(listener: () => void): () => void {
    this.#listeners.add(listener)
    return () => { this.#listeners.delete(listener) }
  }

  clear(): void {
    if (this.#records.length === 0) return
    this.#records.length = 0
    for (const listener of this.#listeners) listener()
  }
}

export function hasBoundedOutputExceptionEvidence(record: LocalOutputOperationFailureV1): boolean {
  return isBoundedDiagnosticExceptionProjection(
    record.stageFailure.exception,
    MAX_EXCEPTION_TEXT_BYTES,
  )
}
