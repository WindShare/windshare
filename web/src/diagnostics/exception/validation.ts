import {
  JAVASCRIPT_EXCEPTION_KINDS, NATIVE_ERROR_CLASSES, MAX_EXCEPTION_BYTES,
  MAX_EXCEPTION_DEPTH, MAX_EXCEPTION_NODES, MAX_EXCEPTION_BRANCHES,
  type DiagnosticExceptionProjection,
} from './model'
import { diagnosticBytes } from './text'

const TEXT_FIELDS = ['constructorName', 'errorName', 'message', 'stack', 'thrownValue'] as const
const FIELDS = [...TEXT_FIELDS, 'javascriptKind', 'nativeClass', 'thrownType', 'cause', 'errors', 'omittedBranches', 'truncated', 'unavailable']
const THROWN_TYPES = ['null', 'undefined', 'object', 'function', 'string', 'number', 'boolean', 'bigint', 'symbol']
const MAX_CAPTURE_MARKERS = 16
const MAX_MARKER_BYTES = 64

/** Export may clone a snapshot; validate the bounded value, not its in-memory provenance. */
export function isBoundedDiagnosticExceptionProjection(projection: DiagnosticExceptionProjection, maximumTextBytes: number): boolean {
  if (!Number.isSafeInteger(maximumTextBytes) || maximumTextBytes <= 0) {
    throw new RangeError('diagnostic exception text capacity must be a positive integer')
  }
  let nodes = 0
  const visit = (node: DiagnosticExceptionProjection, depth: number): boolean => {
    if (++nodes > MAX_EXCEPTION_NODES || depth > MAX_EXCEPTION_DEPTH ||
        typeof node !== 'object' || node === null || !Object.isFrozen(node)) return false
    const keys = Object.keys(node)
    if (keys.length !== FIELDS.length || keys.some(key => !FIELDS.includes(key))) return false
    if (!JAVASCRIPT_EXCEPTION_KINDS.includes(node.javascriptKind) || !NATIVE_ERROR_CLASSES.includes(node.nativeClass) ||
        !THROWN_TYPES.includes(node.thrownType) ||
        !Number.isSafeInteger(node.omittedBranches) || node.omittedBranches < 0) return false
    if (!TEXT_FIELDS.every(key => node[key] === null ||
        (typeof node[key] === 'string' && new TextEncoder().encode(node[key]).byteLength <= maximumTextBytes))) return false
    for (const markers of [node.truncated, node.unavailable]) {
      if (!Array.isArray(markers) || !Object.isFrozen(markers) || markers.length > MAX_CAPTURE_MARKERS ||
          markers.some(marker => typeof marker !== 'string' || diagnosticBytes(marker) > MAX_MARKER_BYTES)) return false
    }
    return Array.isArray(node.errors) && Object.isFrozen(node.errors) && node.errors.length <= MAX_EXCEPTION_BRANCHES &&
      (node.cause === null || visit(node.cause, depth + 1)) && node.errors.every(child => visit(child, depth + 1))
  }
  try { return visit(projection, 0) && diagnosticBytes(projection) <= MAX_EXCEPTION_BYTES }
  catch { return false }
}
