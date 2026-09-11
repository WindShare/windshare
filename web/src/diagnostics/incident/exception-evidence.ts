import {
  JAVASCRIPT_EXCEPTION_KINDS,
  NATIVE_ERROR_CLASSES,
  projectDiagnosticException,
  type DiagnosticExceptionProjection,
} from '../exception'

export const MAX_INCIDENT_EXCEPTION_TEXT_BYTES = 2_048
const encoder = new TextEncoder()
const NULLABLE_TEXT_FIELDS = Object.freeze([
  'constructorName', 'errorName', 'message', 'stack', 'thrownValue', 'cause',
] as const)
const EXCEPTION_FIELDS = Object.freeze([
  'javascriptKind', 'nativeClass', 'thrownType', ...NULLABLE_TEXT_FIELDS,
])
const THROWN_TYPES = Object.freeze([
  'null', 'undefined', 'object', 'function', 'string', 'number', 'boolean', 'bigint', 'symbol',
])

/** Retain immutable evidence, never the thrown object or its mutable getters. */
export function snapshotIncidentException(error: unknown): DiagnosticExceptionProjection {
  return projectDiagnosticException(error, boundExceptionText)
}

export function isIncidentExceptionEvidence(value: unknown): value is DiagnosticExceptionProjection {
  if (typeof value !== 'object' || value === null || !Object.isFrozen(value)) return false
  const record = value as Record<string, unknown>
  const keys = Object.keys(record)
  return keys.length === EXCEPTION_FIELDS.length && keys.every(key => EXCEPTION_FIELDS.includes(key)) &&
    JAVASCRIPT_EXCEPTION_KINDS.some(kind => kind === record.javascriptKind) &&
    NATIVE_ERROR_CLASSES.some(kind => kind === record.nativeClass) &&
    THROWN_TYPES.some(type => type === record.thrownType) &&
    NULLABLE_TEXT_FIELDS.every(field => record[field] === null || (
      typeof record[field] === 'string' && encoder.encode(record[field]).byteLength <= MAX_INCIDENT_EXCEPTION_TEXT_BYTES
    ))
}

function boundExceptionText(value: string): string {
  let bytes = 0
  let end = 0
  for (const character of value) {
    bytes += encoder.encode(character).byteLength
    if (bytes > MAX_INCIDENT_EXCEPTION_TEXT_BYTES) break
    end += character.length
  }
  return value.slice(0, end)
}
