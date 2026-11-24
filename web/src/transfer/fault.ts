import { DirectoryFailureError } from '../catalog/client'
import { CatalogPageStoreError } from '../catalog/page-store'
import { DIRECTORY_FAILURE_CODE_RESOURCE_LIMIT } from '../catalog/records'
import { BlockLaneAttemptsError } from '../content/broker'
import { RemoteOperationError } from '../content/session-operations'
import { RevisionCapacityBusyError, requiresRevisionLeaseReplacement } from '../content/session-services'
import { BandwidthWaitError } from '../contracts/capacity'
import { GenerationRecoveryExhaustedError } from '../receiver/generation-recovery'
import {
  AllRelaysRestrictedError,
  RelayCooldownError,
  isTerminalRecoveryFailure,
} from '../receiver/recovery-failure'
import { RelayEndpointFailure } from '../receiver/relay-selection/race'
import { LaneAdmissionTransportError } from '../session/lane-admission'
import { SessionHandshakeTimeoutError, SessionRuntimeError } from '../session/runtime-types'
import { RelayHeartbeatError } from '../transport/relay/heartbeat'
import { RelayReceiverError } from '../transport/relay/receiver'
import {
  WebRTCChannelClosedError,
  WebRTCRemoteClosedError,
  WebRTCTransportError,
} from '../transport/webrtc/errors'
import type { CatalogCapacityReason, ReceivePauseReason } from './state'

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
  PortableLimitExceeded: 'portable-limit-exceeded',
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

/** The failed initial commit left no bytes or file reservation. Replaying the whole file is safe. */
export class RetryableFilePublishError extends TransferFaultError {
  constructor(cause: unknown) {
    super(outputFault(FaultScope.FileLocal, OutputFaultCode.CommitFailed),
      cause instanceof Error ? cause.message : String(cause), { cause })
    this.name = 'RetryableFilePublishError'
  }
}

export class FileDestinationConflictError extends TransferFaultError {
  readonly destinationPath: string

  constructor(destinationPath: string, options?: ErrorOptions) {
    super(outputFault(FaultScope.FileLocal, OutputFaultCode.TargetCollision),
      `WindShare could not verify "${destinationPath}". Check the file in your folder and move it aside before downloading it again.`, options)
    this.name = 'FileDestinationConflictError'
    this.destinationPath = destinationPath
  }
}

/** A failed shared object invalidates every member, while an independent writer owns its failure. */
export function outputFailure(scope: FaultScope, cause: unknown, fallback: OutputFaultCode): TransferFaultError {
  if (cause instanceof TransferFaultError && cause.fault.scope === scope) return cause
  let code: FaultCode = fallback
  if (isQuotaExceededError(cause)) code = OutputFaultCode.QuotaExceeded
  else if (isPermissionDeniedError(cause)) code = OutputFaultCode.PermissionDenied
  else if (cause instanceof TransferFaultError) code = cause.fault.code
  return new TransferFaultError(createFault(FaultDomain.Output, scope, code),
    cause instanceof Error ? cause.message : String(cause), { cause })
}

export class TransferPauseError extends Error {
  readonly reason: ReceivePauseReason

  constructor(reason: ReceivePauseReason, message = 'Transfer paused') {
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

const PORTABLE_LIMIT_MEBIBYTE = 1024n * 1024n

/** Portable downloads have no resume or partial export, so exceeding their cap aborts the whole job. */
export function portableLimitExceededError(maxBytes: bigint): TransferFaultError {
  const limitLabel = maxBytes >= PORTABLE_LIMIT_MEBIBYTE && maxBytes % PORTABLE_LIMIT_MEBIBYTE === 0n
    ? `${Number(maxBytes / PORTABLE_LIMIT_MEBIBYTE)} MiB`
    : `${maxBytes.toString()} bytes`
  return new TransferFaultError(
    outputFault(FaultScope.Job, OutputFaultCode.PortableLimitExceeded),
    `Portable browser download is limited to ${limitLabel}`,
  )
}

export function isPortableLimitExceededError(error: unknown): boolean {
  return error instanceof TransferFaultError && error.fault.code === OutputFaultCode.PortableLimitExceeded
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

export function storagePauseReason(error: unknown): 'storage-full' | 'permission-needed' | undefined {
  if (isQuotaExceededError(error)) return 'storage-full'
  if (isPermissionDeniedError(error)) return 'permission-needed'
  return undefined
}

export function isSourceInvalidatedError(error: unknown): boolean {
  return error instanceof TransferFaultError && (
    error.fault.code === SourceFaultCode.RevisionChanged ||
    error.fault.code === SourceFaultCode.RevisionInvalidated
  )
}

/**
 * Classifies permanent catalog capacity ceilings by owner so browsing and
 * downloads can give accurate guidance: browser-local cache exhaustion can still
 * succeed via `wind get` or a narrower folder, whereas sender share capacity
 * exhaustion is permanent for every receiver on this live share.
 */
export function catalogCapacityReason(error: unknown): CatalogCapacityReason | undefined {
  if (error instanceof CatalogPageStoreError && error.kind === 'resource-limit') {
    return 'browser-catalog-limit'
  }
  if (
    error instanceof DirectoryFailureError &&
    error.failure.kind === 'resource-limit' &&
    !error.failure.retryable
  ) {
    return 'share-catalog-limit'
  }
  if (
    error instanceof RemoteOperationError &&
    error.scope === 'directory' &&
    error.code === DIRECTORY_FAILURE_CODE_RESOURCE_LIMIT &&
    !error.retryable
  ) {
    return 'share-catalog-limit'
  }
  return undefined
}

const MAX_INTERRUPTION_INSPECTION_DEPTH = 8

/**
 * Classifies transient transport, lane, session-recovery, and exhausted remote
 * revision-budget interruptions (capacity backpressure or lease replacement)
 * that should pause resumable backends rather than aborting saved progress.
 * Permanent share stops, protocol violations, revision drift, and local I/O
 * faults return false so they keep their terminal semantics.
 */
export function isRecoverableTransferInterruption(error: unknown, depth = 0): boolean {
  if (depth >= MAX_INTERRUPTION_INSPECTION_DEPTH) return false
  if (error instanceof BlockLaneAttemptsError) return true
  if (error instanceof RevisionCapacityBusyError || requiresRevisionLeaseReplacement(error)) return true
  if (error instanceof RelayEndpointFailure) {
    return isRecoverableTransferInterruption(error.cause, depth + 1)
  }
  if (error instanceof AggregateError) {
    return error.errors.length > 0 && error.errors.every(cause => isRecoverableTransferInterruption(cause, depth + 1))
  }
  if (error instanceof RelayReceiverError) return !isTerminalRecoveryFailure(error)
  return isTransientLaneOrTransportError(error)
}

function isTransientLaneOrTransportError(error: unknown): boolean {
  if (error instanceof SessionRuntimeError) return error.scope === 'lane'
  if (error instanceof DOMException && error.name === 'TimeoutError') return true
  return error instanceof BandwidthWaitError ||
    error instanceof RelayHeartbeatError ||
    error instanceof SessionHandshakeTimeoutError ||
    error instanceof LaneAdmissionTransportError ||
    error instanceof GenerationRecoveryExhaustedError ||
    error instanceof RelayCooldownError ||
    error instanceof AllRelaysRestrictedError ||
    error instanceof WebRTCTransportError ||
    error instanceof WebRTCRemoteClosedError ||
    error instanceof WebRTCChannelClosedError
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
