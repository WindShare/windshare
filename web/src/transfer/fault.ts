export const FaultDomain = Object.freeze({
  Protocol: 'protocol',
  Source: 'source',
  Output: 'output',
  Checkpoint: 'checkpoint',
  Cancellation: 'cancellation',
} as const)

export type FaultDomain = (typeof FaultDomain)[keyof typeof FaultDomain]

export const FaultScope = Object.freeze({
  Job: 'job',
  DirectoryLocal: 'directory-local',
  FileLocal: 'file-local',
} as const)

export type FaultScope = (typeof FaultScope)[keyof typeof FaultScope]

export const OutputFaultCode = Object.freeze({
  WriteFailed: 'write-failed',
  CommitFailed: 'commit-failed',
  QuotaExceeded: 'quota-exceeded',
  PermissionDenied: 'permission-denied',
  DirectoryMetadata: 'directory-metadata',
  StorageUnavailable: 'storage-unavailable',
  TargetCollision: 'target-collision',
} as const)

export type OutputFaultCode = (typeof OutputFaultCode)[keyof typeof OutputFaultCode]

export const SourceFaultCode = Object.freeze({
  RevisionChanged: 'revision-changed',
  RevisionInvalidated: 'revision-invalidated',
  Unavailable: 'unavailable',
  Permanent: 'permanent',
} as const)

export type SourceFaultCode = (typeof SourceFaultCode)[keyof typeof SourceFaultCode]

export const CheckpointFaultCode = Object.freeze({
  Invalid: 'invalid',
  StorageFailed: 'storage-failed',
  Busy: 'busy',
} as const)

export type CheckpointFaultCode = (typeof CheckpointFaultCode)[keyof typeof CheckpointFaultCode]

export type FaultCode = OutputFaultCode | SourceFaultCode | CheckpointFaultCode | string

export interface Fault {
  readonly domain: FaultDomain
  readonly scope: FaultScope
  readonly code: FaultCode
}

const VALID_DOMAINS = new Set<string>(Object.values(FaultDomain))
const VALID_SCOPES = new Set<string>(Object.values(FaultScope))

export function createFault(domain: FaultDomain, scope: FaultScope, code: FaultCode): Fault {
  return Object.freeze({ domain, scope, code })
}

export function outputFault(scope: FaultScope, code: OutputFaultCode): Fault {
  return createFault(FaultDomain.Output, scope, code)
}

export function sourceFault(scope: FaultScope, code: SourceFaultCode): Fault {
  return createFault(FaultDomain.Source, scope, code)
}

export function isFault(value: unknown): value is Fault {
  if (typeof value !== 'object' || value === null) return false
  const record = value as Partial<Fault>
  return typeof record.domain === 'string' && VALID_DOMAINS.has(record.domain) &&
    typeof record.scope === 'string' && VALID_SCOPES.has(record.scope) &&
    typeof record.code === 'string' && record.code.length > 0
}

export class TransferFaultError extends Error {
  readonly fault: Fault

  constructor(fault: Fault, message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'TransferFaultError'
    this.fault = fault
  }
}

export class TransferPauseError extends Error {
  readonly reason: 'user' | 'storage-full' | 'permission-needed'

  constructor(reason: 'user' | 'storage-full' | 'permission-needed', message = 'Transfer paused') {
    super(message)
    this.name = 'TransferPauseError'
    this.reason = reason
  }
}

export class TransferStopError extends Error {
  constructor(message = 'Transfer stopped') {
    super(message)
    this.name = 'TransferStopError'
  }
}

export function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === 'AbortError'
}

export function isQuotaExceededError(error: unknown): boolean {
  if (error instanceof TransferFaultError && error.fault.code === OutputFaultCode.QuotaExceeded) return true
  if (error instanceof DOMException) {
    return error.name === 'QuotaExceededError' || error.name === 'NS_ERROR_DOM_QUOTA_REACHED'
  }
  return error instanceof Error && error.name === 'QuotaExceededError'
}

export function isPermissionDeniedError(error: unknown): boolean {
  if (error instanceof TransferFaultError && error.fault.code === OutputFaultCode.PermissionDenied) return true
  return error instanceof DOMException && (error.name === 'NotAllowedError' || error.name === 'SecurityError')
}

export function isSourceInvalidatedError(error: unknown): boolean {
  return error instanceof TransferFaultError && (
    error.fault.code === SourceFaultCode.RevisionChanged ||
    error.fault.code === SourceFaultCode.RevisionInvalidated
  )
}

export type NativeErrorClass =
  | 'abort'
  | 'timeout'
  | 'not_allowed'
  | 'not_found'
  | 'invalid_state'
  | 'quota_exceeded'
  | 'data'
  | 'security'
  | 'not_supported'
  | 'no_modification_allowed'
  | 'unknown'

const DOM_ERROR_CLASSES: Readonly<Record<string, NativeErrorClass>> = Object.freeze({
  AbortError: 'abort',
  TimeoutError: 'timeout',
  NotAllowedError: 'not_allowed',
  NotFoundError: 'not_found',
  InvalidStateError: 'invalid_state',
  QuotaExceededError: 'quota_exceeded',
  DataError: 'data',
  SecurityError: 'security',
  NotSupportedError: 'not_supported',
  NoModificationAllowedError: 'no_modification_allowed',
})

export function classifyNativeError(error: unknown): NativeErrorClass {
  if (error instanceof DOMException || error instanceof Error) {
    return DOM_ERROR_CLASSES[error.name] ?? 'unknown'
  }
  return 'unknown'
}
