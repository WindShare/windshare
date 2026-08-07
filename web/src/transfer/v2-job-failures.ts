import { V2DirectoryFailureError } from '../catalog/v2-client'
import { V2BlockLaneAttemptsError } from '../content/v2-broker'
import {
  V2BlockOperationError,
  V2RemoteOperationError,
  V2RemoteRevisionError,
  V2RevisionChangedDuringRecoveryError,
  V2RevisionLeaseExpiredError,
} from '../content/v2-session-services'
import {
  OutputDirectoryMutationError,
  OutputBudgetExceededError,
  OutputSessionSuspendedError,
} from './output-session'
import {
  V2DirectoryTraversalError,
  V2OutputPausedError,
} from './v2-job-contract'

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

export function isPauseReason(error: unknown): boolean {
  return error instanceof V2OutputPausedError ||
    error instanceof OutputSessionSuspendedError ||
    error instanceof OutputBudgetExceededError
}

export function isV2FileScopedTransferFailure(error: unknown): boolean {
  if (error instanceof V2RemoteOperationError) {
    return error.scope === 'revision' || error.scope === 'block'
  }
  return error instanceof V2RemoteRevisionError ||
    error instanceof V2RevisionLeaseExpiredError ||
    error instanceof V2BlockOperationError ||
    error instanceof V2BlockLaneAttemptsError ||
    error instanceof V2RevisionChangedDuringRecoveryError ||
    error instanceof V2FileRevisionChangedError ||
    error instanceof V2FileLeaseSettlementError ||
    error instanceof V2FileOutputError
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

/** Preserves revision-scoped release evidence without escalating an isolated file. */
export class V2FileLeaseSettlementError extends Error {
  readonly primaryFailure: unknown
  readonly releaseFailure: unknown

  constructor(primaryFailure: unknown, releaseFailure: unknown) {
    const evidence = primaryFailure === undefined
      ? releaseFailure
      : new AggregateError([primaryFailure, releaseFailure], 'File transfer and revision lease release failed')
    super('Revision lease release failed within file isolation', { cause: evidence })
    this.name = 'V2FileLeaseSettlementError'
    this.primaryFailure = primaryFailure
    this.releaseFailure = releaseFailure
  }
}
