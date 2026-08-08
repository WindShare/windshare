import { V2DirectoryFailureError } from '../../catalog/v2-client'
import { V2BlockLaneAttemptsError } from '../../content/v2-broker'
import {
  V2BlockOperationError,
  V2RemoteOperationError,
  V2RemoteRevisionError,
  V2RevisionChangedDuringRecoveryError,
  V2RevisionLeaseExpiredError,
} from '../../content/v2-session-services'
import { OutputTransactionContractError } from '../output-file-transaction'
import {
  OutputDirectoryMutationError,
  OutputBudgetExceededError,
  OutputSessionCompromisedError,
  TransferPauseRequestedError,
} from '../output-session'
import {
  BoundaryFaultError,
  FaultScope,
  OutputFaultCode,
  SessionFaultCode,
  SourceFaultCode,
  dependencyContractFault,
  normalizeBoundaryError,
  outputFault,
  sessionFault,
  sourceFault,
  type Fault,
} from '../fault'
import {
  V2DirectoryTraversalError,
  V2OutputPausedError,
} from './contract'

const REVISION_FAILURE_STALE = 0x3001
const REVISION_FAILURE_NOT_FOUND = 0x3002
const REVISION_FAILURE_UNREADABLE = 0x3003
const REVISION_FAILURE_UNSUPPORTED_STABILITY = 0x3004
const REVISION_FAILURE_DRIFT = 0x3007

export function isDirectoryScopedFailure(error: unknown): boolean {
  return error instanceof V2DirectoryFailureError ||
    error instanceof V2DirectoryTraversalError ||
    error instanceof V2DirectoryOutputError ||
    (error instanceof V2RemoteOperationError && error.scope === 'directory')
}

export class V2DirectoryOutputError extends Error {
  readonly directoryId: string | undefined

  constructor(message: string, options: ErrorOptions & { readonly directoryId?: string }) {
    super(message, options)
    this.name = 'V2DirectoryOutputError'
    this.directoryId = options.directoryId
  }
}

export function isolatedDirectoryOutputFailure(
  error: unknown,
  fileFailureIsolation: boolean,
  directoryId?: string,
): V2DirectoryOutputError | undefined {
  return fileFailureIsolation &&
    error instanceof OutputDirectoryMutationError &&
    !error.sessionCompromised
    ? new V2DirectoryOutputError('Output backend isolated a child directory mutation', {
        cause: error,
        ...(directoryId === undefined ? {} : { directoryId }),
      })
    : undefined
}

export type NormalizedV2FileTransferFailure =
  | Readonly<{ kind: 'canceled', diagnostic: unknown }>
  | Readonly<{ kind: 'fault', fault: Fault, diagnostic: BoundaryFaultError }>

/**
 * Maps the immediate file collaborator result exactly once. Downstream policy
 * receives only this closed value and cannot recover authority from causes.
 */
export function normalizeV2FileTransferFailure(
  error: unknown,
  signal?: AbortSignal,
): NormalizedV2FileTransferFailure {
  if (error instanceof TransferPauseRequestedError) {
    return Object.freeze({ kind: 'canceled', diagnostic: error })
  }
  const classified = fileTransferFault(error)
  const candidate = classified === undefined
    ? error ?? new Error('File collaborator threw an undefined failure')
    : new BoundaryFaultError(classified, 'File collaborator returned a typed failure', { cause: error })
  const normalization = normalizeBoundaryError(candidate, signal)
  if (normalization.kind === 'canceled') {
    return Object.freeze({
      kind: 'canceled',
      diagnostic: signal?.aborted === true
        ? signal.reason ?? candidate
        : error ?? candidate,
    })
  }
  if (normalization.kind === 'success') {
    return normalizedV2FileTransferFault(
      dependencyContractFault(),
      candidate,
      'File collaborator failure normalized as an impossible success',
    )
  }
  if (candidate instanceof BoundaryFaultError && sameFault(candidate.fault, normalization.fault)) {
    return Object.freeze({ kind: 'fault', fault: candidate.fault, diagnostic: candidate })
  }
  return normalizedV2FileTransferFault(normalization.fault, candidate)
}

export function normalizedV2FileTransferFault(
  fault: Fault,
  cause: unknown,
  message = 'File transfer failed at a normalized collaborator boundary',
): NormalizedV2FileTransferFailure & { readonly kind: 'fault' } {
  const diagnostic = new BoundaryFaultError(fault, message, { cause })
  return Object.freeze({ kind: 'fault', fault: diagnostic.fault, diagnostic })
}

/** Only normalized file-local values may be isolated by TransferJob. */
export function isV2FileScopedTransferFailure(error: unknown): boolean {
  return error instanceof BoundaryFaultError && error.fault.scope === FaultScope.FileLocal
}

function fileTransferFault(error: unknown): Fault | undefined {
  if (error instanceof V2RemoteRevisionError) {
    return revisionFailureFault(error.failure.code, error.failure.retryable)
  }
  if (error instanceof V2RemoteOperationError) {
    if (error.scope === 'revision') return revisionFailureFault(error.code, error.retryable)
    if (error.scope === 'block') return sourceFault(FaultScope.FileLocal, SourceFaultCode.Unavailable)
    return sessionFault(FaultScope.OutputPause, SessionFaultCode.Transport)
  }
  if (error instanceof V2RevisionChangedDuringRecoveryError) {
    return sourceFault(FaultScope.FileLocal, SourceFaultCode.RevisionInvalidated)
  }
  if (error instanceof V2FileRevisionChangedError) {
    return sourceFault(FaultScope.FileLocal, SourceFaultCode.RevisionChanged)
  }
  if (error instanceof V2RevisionLeaseExpiredError) {
    return sourceFault(FaultScope.FileLocal, SourceFaultCode.Unavailable)
  }
  if (error instanceof V2BlockLaneAttemptsError) {
    return sessionFault(FaultScope.OutputPause, SessionFaultCode.Transport)
  }
  if (error instanceof V2BlockOperationError) {
    return sessionFault(FaultScope.SessionTerminal, SessionFaultCode.Protocol)
  }
  if (error instanceof OutputBudgetExceededError) {
    return outputFault(FaultScope.OutputPause, OutputFaultCode.ResourceBudget)
  }
  if (error instanceof OutputSessionCompromisedError) {
    return outputFault(FaultScope.OutputPause, OutputFaultCode.MutationAmbiguous)
  }
  if (error instanceof OutputTransactionContractError) {
    return outputFault(FaultScope.OutputPause, OutputFaultCode.Contract)
  }
  if (error instanceof V2FileOutputError || error instanceof V2OutputPausedError) {
    return outputFault(FaultScope.OutputPause, OutputFaultCode.StateIO)
  }
  return undefined
}

function revisionFailureFault(code: number, retryable: boolean): Fault {
  if (code === REVISION_FAILURE_DRIFT) {
    return sourceFault(FaultScope.FileLocal, SourceFaultCode.RevisionInvalidated)
  }
  if (!retryable && (
    code === REVISION_FAILURE_NOT_FOUND ||
    code === REVISION_FAILURE_UNREADABLE ||
    code === REVISION_FAILURE_UNSUPPORTED_STABILITY
  )) {
    return sourceFault(FaultScope.FileLocal, SourceFaultCode.Permanent)
  }
  if (code === REVISION_FAILURE_STALE) {
    return sourceFault(FaultScope.FileLocal, SourceFaultCode.RevisionChanged)
  }
  return sourceFault(FaultScope.FileLocal, SourceFaultCode.Unavailable)
}

function sameFault(left: Fault, right: Fault): boolean {
  return left.domain === right.domain && left.scope === right.scope && left.code === right.code
}

export class V2FileRevisionChangedError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'V2FileRevisionChangedError'
  }
}

export class V2FileOutputError extends Error {
  constructor(message: string, options: ErrorOptions) {
    super(message, options)
    this.name = 'V2FileOutputError'
  }
}
