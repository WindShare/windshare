import { associateDiagnosticException, projectDiagnosticException, type DiagnosticExceptionProjection } from '../exception'
import { boundedDiagnosticText, diagnosticBytes } from '../exception/text'

export type FailureContextValue = string | number | boolean | null
export interface FailureEvidenceInput {
  readonly error?: unknown
  readonly operation?: string
  readonly context?: Readonly<Record<string, FailureContextValue>>
}
export interface FailureEvidence {
  readonly id: string
  readonly operation: string
  readonly exception: DiagnosticExceptionProjection
  readonly context: Readonly<Record<string, FailureContextValue>>
  readonly contextTruncated: boolean
  readonly signature: string
}
const MAX_CONTEXT_FIELDS = 16
const MAX_CONTEXT_BYTES = 2_048
const MAX_CONTEXT_TEXT_BYTES = 256
export const MAX_FAILURE_OPERATION_BYTES = 128
export const FAILURE_SIGNATURE_HEX_LENGTH = 8
const associations = new WeakMap<object, FailureEvidence>()
const captured = new WeakSet<FailureEvidence>()
let nextEvidence = 1n

export function captureFailureEvidence(error: unknown, input: Omit<FailureEvidenceInput, 'error'>): FailureEvidence {
  const existing = evidenceFor(error)
  if (existing !== undefined) return existing
  try {
    if (typeof error === 'object' && error !== null && Reflect.get(error, 'errors') === undefined) {
      const cause = evidenceFor(Reflect.get(error, 'cause'))
      if (cause !== undefined) { bindFailureEvidence(error, cause); return cause }
    }
  } catch { /* Hostile exception access is captured below as unavailable evidence. */ }
  const operation = boundedDiagnosticText(input.operation ?? 'unknown', MAX_FAILURE_OPERATION_BYTES)
  const exception = projectDiagnosticException(error)
  const { context, contextTruncated } = captureContext(input.context)
  const evidence: FailureEvidence = Object.freeze({
    id: (nextEvidence++).toString(10), operation, exception,
    context: Object.freeze(context), contextTruncated,
    signature: causeSignature(operation, exception, context.invariant),
  })
  captured.add(evidence)
  bindFailureEvidence(error, evidence)
  return evidence
}

export function evidenceFromInput(input: FailureEvidenceInput | undefined, operation: string): FailureEvidence | undefined {
  return input !== undefined && Object.hasOwn(input, 'error')
    ? captureFailureEvidence(input.error, { ...input, operation: input.operation ?? operation })
    : undefined
}

/** Associations follow wrappers weakly; neither thrown objects nor their lifetime belong to history. */
export function bindFailureEvidence(value: unknown, evidence: FailureEvidence): void {
  if ((typeof value === 'object' && value !== null) || typeof value === 'function') associations.set(value, evidence)
  associateDiagnosticException(value, evidence.exception)
}
export function evidenceFor(value: unknown): FailureEvidence | undefined {
  return (typeof value === 'object' && value !== null) || typeof value === 'function' ? associations.get(value) : undefined
}
export function isFailureEvidence(value: unknown): value is FailureEvidence {
  return typeof value === 'object' && value !== null && captured.has(value as FailureEvidence)
}

function causeSignature(operation: string, exception: DiagnosticExceptionProjection, invariant: FailureContextValue | undefined): string {
  const shape = (node: DiagnosticExceptionProjection): unknown => [
    node.errorName, node.nativeClass, node.message, node.thrownValue,
    node.cause === null ? null : shape(node.cause),
    // Exception ordering does not nominate a cause; equivalent aggregate sets group together.
    node.errors.map(child => JSON.stringify(shape(child))).sort(),
  ]
  const text = JSON.stringify([operation, invariant ?? null, shape(exception)])
  let hash = 0x811c9dc5
  for (let index = 0; index < text.length; index++) hash = Math.imul(hash ^ text.charCodeAt(index), 0x01000193) >>> 0
  return hash.toString(16).padStart(FAILURE_SIGNATURE_HEX_LENGTH, '0')
}

function captureContext(input: FailureEvidenceInput['context']) {
  const context: Record<string, FailureContextValue> = {}
  let contextTruncated = false
  let bytes = 0
  try {
    // Callers supply a small record of identities/state, never an arbitrary object graph.
    let count = 0
    for (const key in input) {
      if (++count > MAX_CONTEXT_FIELDS) { contextTruncated = true; break }
      const value = input[key]
      const boundedKey = boundedDiagnosticText(key, MAX_CONTEXT_TEXT_BYTES)
      const bounded = typeof value === 'string' ? boundedDiagnosticText(value, MAX_CONTEXT_TEXT_BYTES) : value
      if (bounded === undefined || (typeof bounded === 'number' && !Number.isFinite(bounded))) continue
      if (bounded !== value || boundedKey !== key) contextTruncated = true
      bytes += diagnosticBytes([boundedKey, bounded])
      if (bytes > MAX_CONTEXT_BYTES) { contextTruncated = true; break }
      Object.defineProperty(context, boundedKey, { value: bounded, enumerable: true })
    }
  } catch { contextTruncated = true }
  return { context: Object.freeze(context), contextTruncated }
}
