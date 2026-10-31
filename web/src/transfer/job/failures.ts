import { recordScopedFailure } from '../../diagnostics/incident/scope'
import { withFailureEvidence } from '../../diagnostics/failure/attach'
import type { FailureEvidenceInput } from '../../diagnostics/failure/evidence'
import { ClassifiedTransferFailureError, snapshotClassification, isClassifiedTransferFailure, type ClassifiedTransferFailure } from '../failure/classification'
import { declaredOutputFailure, type OutputFailure } from '../failure/output'
import { DirectoryFailureError } from '../../catalog/client'
import { BlockLaneAttemptsError } from '../../content/broker'
import {
  BlockOperationError,
  RemoteOperationError,
  RemoteRevisionError,
  RevisionCapacityBusyError,
  RevisionChangedDuringRecoveryError,
  RevisionLeaseExpiredError,
} from '../../content/session-services'
import {
  faultFailureFact,
  protocolFailureFact,
  unclassifiedFailureFact,
  type FailureFact,
  type FailureFactRef,
  type FailureFactRelation,
  type FailureStage,
  type IncidentScopeHandle,
  type RecoveryDisposition,
} from '../../diagnostics/incident'
import { OutputTransactionContractError } from '../output/transaction'
import {
  OutputDirectoryMutationError,
  OutputBudgetExceededError,
  OutputSessionCompromisedError,
  TransferPauseRequestedError,
  TransferStopRequestedError,
} from '../output/session'
import {
  BoundaryFaultError,
  CatalogFaultCode,
  FaultDomain,
  FaultScope,
  OutputFaultCode,
  SessionFaultCode,
  SourceFaultCode,
  catalogFault,
  dependencyContractFault,
  isFault,
  normalizeBoundaryError,
  outputFault,
  sessionFault,
  sourceFault,
  type Fault,
} from '../failure/fault'
import type {
  TransferFileOutcomeEvidence,
} from '../failure/file-outcome'
import {
  type MaterializationFailureReason,
} from '../failure/materialization'
import {
  CatalogTraversalError,
  DirectoryTraversalError,
  OutputPausedError,
} from './contract'
import { SelectionTargetMissingError } from './selection'
import { RevisionCapacityWaitBudgetError } from '../revision-capacity/public'

const REVISION_FAILURE_STALE = 0x3001
const REVISION_FAILURE_NOT_FOUND = 0x3002
const REVISION_FAILURE_UNREADABLE = 0x3003
const REVISION_FAILURE_UNSUPPORTED_STABILITY = 0x3004
const REVISION_FAILURE_DRIFT = 0x3007
const MAXIMUM_FAILURE_CAUSE_DEPTH = 8

export interface TransferFailureClassificationOptions extends FailureEvidenceInput {
  readonly signal?: AbortSignal
  readonly incidentScope?: IncidentScopeHandle
  readonly relation?: FailureFactRelation
  readonly stage?: FailureStage
  readonly materializationFailureReason?: MaterializationFailureReason
}

export class DirectoryOutputError extends ClassifiedTransferFailureError {
  readonly directoryId: string | undefined

  constructor(classification: ClassifiedTransferFailure, directoryId?: string) {
    super(classification)
    this.name = 'DirectoryOutputError'
    this.directoryId = directoryId
  }
}

export function isolatedDirectoryOutputFailure(
  error: unknown,
  fileFailureIsolation: boolean,
  directoryId?: string,
  incidentScope?: IncidentScopeHandle,
): DirectoryOutputError | undefined {
  const mutation = findOutputDirectoryMutationError(error)
  if (
    !fileFailureIsolation ||
    mutation === undefined ||
    mutation.sessionCompromised
  ) {
    return undefined
  }
  const normalized = normalizedFileTransferFault(
    outputFault(FaultScope.DirectoryLocal, OutputFaultCode.DirectoryMetadata),
    {
      error,
      stage: 'output_commit',
      materializationFailureReason: 'directory-finalize-failed',
      ...(incidentScope === undefined ? {} : { incidentScope }),
    },
  )
  return new DirectoryOutputError(normalized.diagnostic.classification, directoryId)
}

export type NormalizedFileTransferFailure =
  | Readonly<{ kind: 'canceled', diagnostic: unknown }>
  | Readonly<{
      kind: 'fault'
      fault: Fault
      fact: FailureFact
      factRef?: FailureFactRef
      materializationFailureReason: MaterializationFailureReason
      diagnostic: ClassifiedTransferFailureError
    }>

/**
 * Maps one immediate collaborator result into product and diagnostic semantics.
 * A previously classified wrapper is materialized into a scope, never classified again.
 */
export function normalizeFileTransferFailure(
  error: unknown,
  options: TransferFailureClassificationOptions = {},
): NormalizedFileTransferFailure {
  if (error instanceof TransferPauseRequestedError || error instanceof TransferStopRequestedError) {
    return Object.freeze({ kind: 'canceled', diagnostic: error })
  }
  if (options.signal?.aborted === true) {
    recordConcurrentCancellationFailure(error, options)
    return Object.freeze({
      kind: 'canceled',
      diagnostic: options.signal.reason ?? error,
    })
  }
  options = { ...options, error }
  if (error instanceof RevisionCapacityWaitBudgetError) {
    return normalizedFileTransferClassification(Object.freeze({
      fault: outputFault(FaultScope.OutputPause, OutputFaultCode.ResourceBudget),
      fact: error.failureFact,
      materializationFailureReason: error.surface === 'revision_open'
        ? 'file-open-failed'
        : 'content-read-failed',
    }), options)
  }
  if (error instanceof RevisionCapacityBusyError) {
    // This is a defensive terminal seam. Normal transfer composition intercepts
    // the exact authenticated type in the wrapped content ports before normalization.
    return normalizedFileTransferClassification(Object.freeze({
      fault: outputFault(FaultScope.OutputPause, OutputFaultCode.ResourceBudget),
      fact: protocolFailureFact({
        stage: 'protocol_operation',
        recoveryDisposition: 'resumable_receive',
        protocolFailure: error.protocolFailure,
      }),
      materializationFailureReason: 'file-open-failed',
    }), options)
  }
  if (error instanceof ClassifiedTransferFailureError) {
    return normalizedFromClassification(
      materializeClassifiedTransferFailure(
        error.classification,
        options.incidentScope,
        options.relation,
      ),
      error.fileOutcomeEvidence,
    )
  }

  const outputFailure = normalizeDeclaredOutputFailure(error, options)
  if (outputFailure !== undefined) return outputFailure
  const classified = fileTransferFault(error)
  const candidate = classified === undefined
    ? error ?? new Error('File collaborator threw an undefined failure')
    : new BoundaryFaultError(classified, 'File collaborator returned a typed failure')
  const normalization = normalizeBoundaryError(candidate, options.signal)
  if (normalization.kind === 'canceled') {
    return Object.freeze({
      kind: 'canceled',
      diagnostic: options.signal?.reason ?? error ?? candidate,
    })
  }
  const fault = normalization.kind === 'success'
    ? dependencyContractFault()
    : normalization.fault
  const stage = options.stage ?? failureStage(error)
  const recoveryDisposition = recoveryDispositionFor(error, fault)
  const reason = options.materializationFailureReason ?? materializationFailureReason(error)
  const fileOutcomeEvidence = transferFileOutcomeEvidence(error)
  if (error instanceof RemoteOperationError) {
    return normalizedFileTransferClassification(
      Object.freeze({
        fault,
        fact: error.failureFact,
        materializationFailureReason: reason,
      }),
      options,
      fileOutcomeEvidence,
    )
  }
  if (error instanceof RemoteRevisionError) {
    return normalizedFileTransferClassification(
      Object.freeze({
        fault,
        fact: error.failureFact,
        materializationFailureReason: reason,
      }),
      options,
      fileOutcomeEvidence,
    )
  }
  // Preserve bounded exception evidence when there is no protocol/domain fact.
  // Product recovery still follows the reviewed fault, never exception text.
  if (classified === undefined) {
    return normalizedFileTransferClassification(Object.freeze({
      fault,
      fact: unclassifiedFailureFact({ ...options, stage, recoveryDisposition, error }),
      materializationFailureReason: reason,
    }), options, fileOutcomeEvidence)
  }
  return normalizedFileTransferFault(fault, {
    ...options,
    stage,
    materializationFailureReason: reason,
    recoveryDisposition,
    ...(fileOutcomeEvidence === undefined ? {} : { fileOutcomeEvidence }),
  })
}

function normalizeDeclaredOutputFailure(
  error: unknown,
  options: TransferFailureClassificationOptions,
): NormalizedFileTransferFailure | undefined {
  // An explicit outer boundary already owns severity; a cause cannot replace it.
  if (error instanceof BoundaryFaultError) return undefined
  const declared = findDeclaredOutputFailure(error)
  if (declared === undefined) return undefined
  const fact = declared.classification.fact
  const stage = options.stage ?? failureStage(error)
  return normalizedFileTransferClassification(Object.freeze({
    fault: declared.classification.fault,
    fact: fact.stage === stage ? fact : Object.freeze({ ...fact, stage }),
    materializationFailureReason: options.materializationFailureReason ?? materializationFailureReason(error),
  }), options, transferFileOutcomeEvidence(error))
}

export function transferFileOutcomeEvidence(
  input: unknown,
): TransferFileOutcomeEvidence | undefined {
  let error = input
  for (let depth = 0; depth < MAXIMUM_FAILURE_CAUSE_DEPTH; depth += 1) {
    if (error instanceof ClassifiedTransferFailureError &&
        error.fileOutcomeEvidence !== undefined) {
      return error.fileOutcomeEvidence
    }
    const outputEvidence = declaredOutputFailure(error)?.fileOutcomeEvidence
    if (outputEvidence !== undefined) return outputEvidence
    if (error instanceof RevisionChangedDuringRecoveryError ||
        error instanceof FileRevisionChangedError ||
        (error instanceof RemoteRevisionError &&
          (error.failure.code === REVISION_FAILURE_DRIFT ||
           error.failure.code === REVISION_FAILURE_STALE)) ||
        (error instanceof BoundaryFaultError &&
          error.fault.domain === 'source' &&
          (error.fault.code === SourceFaultCode.RevisionChanged ||
           error.fault.code === SourceFaultCode.RevisionInvalidated))) {
      return Object.freeze({ kind: 'authenticated-source-drift' })
    }
    if (!(error instanceof Error) || error.cause === undefined) return undefined
    error = error.cause
  }
  return undefined
}

export function normalizedFileTransferFault(
  fault: Fault,
  options: Omit<TransferFailureClassificationOptions, 'signal'> & {
    readonly recoveryDisposition?: RecoveryDisposition
    readonly fileOutcomeEvidence?: TransferFileOutcomeEvidence
  } = {},
): NormalizedFileTransferFailure & { readonly kind: 'fault' } {
  const stage = options.stage ?? 'content_read'
  return normalizedFileTransferClassification(Object.freeze({
    fault,
    fact: faultFailureFact({
      ...options,
      stage,
      recoveryDisposition: options.recoveryDisposition ?? recoveryDispositionFor(undefined, fault),
      fault,
    }),
    materializationFailureReason: options.materializationFailureReason ?? 'content-read-failed',
  }), options, options.fileOutcomeEvidence)
}

function normalizedFileTransferClassification(
  classification: ClassifiedTransferFailure,
  options: Pick<TransferFailureClassificationOptions, 'incidentScope' | 'relation'> & FailureEvidenceInput,
  fileOutcomeEvidence?: TransferFileOutcomeEvidence,
): NormalizedFileTransferFailure & { readonly kind: 'fault' } {
  const captured = Object.hasOwn(options, 'error')
    ? Object.freeze({ ...classification, fact: withFailureEvidence(classification.fact, options.error, options) })
    : classification
  return normalizedFromClassification(
    materializeClassifiedTransferFailure(
      captured,
      options.incidentScope,
      options.relation,
    ),
    fileOutcomeEvidence,
  )
}

export function classificationForTransferFailure(
  error: unknown,
  options: TransferFailureClassificationOptions = {},
): ClassifiedTransferFailure | undefined {
  const normalized = normalizeFileTransferFailure(error, options)
  return normalized.kind === 'fault'
    ? normalized.diagnostic.classification
    : undefined
}

/** Only normalized file-local values may be isolated by TransferJob. */
export function isFileScopedTransferFailure(error: unknown): boolean {
  return error instanceof ClassifiedTransferFailureError &&
    error.classification.fault.scope === FaultScope.FileLocal
}

function normalizedFromClassification(
  classification: ClassifiedTransferFailure,
  fileOutcomeEvidence?: TransferFileOutcomeEvidence,
): NormalizedFileTransferFailure & { readonly kind: 'fault' } {
  const snapshot = snapshotClassification(classification)
  const diagnostic = new ClassifiedTransferFailureError(snapshot, fileOutcomeEvidence)
  return Object.freeze({
    kind: 'fault',
    fault: snapshot.fault,
    fact: snapshot.fact,
    ...(snapshot.factRef === undefined ? {} : { factRef: snapshot.factRef }),
    materializationFailureReason: snapshot.materializationFailureReason,
    diagnostic,
  })
}

export function materializeClassifiedTransferFailure(
  classification: ClassifiedTransferFailure,
  incidentScope: IncidentScopeHandle | undefined,
  relation: FailureFactRelation = 'contributor',
): ClassifiedTransferFailure {
  const snapshot = snapshotClassification(classification)
  if (incidentScope === undefined) return snapshot
  let factRef: FailureFactRef | undefined
  try {
    factRef = recordScopedFailure(incidentScope, snapshot.fact, relation, snapshot.factRef)
  } catch {
    // Incident recording is observational and cannot alter product classification.
  }
  return Object.freeze({
    fault: snapshot.fault,
    fact: snapshot.fact,
    ...(factRef === undefined ? {} : { factRef }),
    materializationFailureReason: snapshot.materializationFailureReason,
  })
}

function fileTransferFault(error: unknown): Fault | undefined {
  if (isFault(error) && Object.isFrozen(error)) return error
  if (error instanceof BoundaryFaultError) return error.fault
  return remoteTransferFault(error) ??
    sourceTransferFault(error) ??
    catalogTransferFault(error) ??
    outputTransferFault(error)
}

function remoteTransferFault(error: unknown): Fault | undefined {
  if (error instanceof RemoteRevisionError) {
    return revisionFailureFault(error.failure.code, error.failure.retryable)
  }
  if (!(error instanceof RemoteOperationError)) return undefined
  switch (error.scope) {
    case 'revision': return revisionFailureFault(error.code, error.retryable)
    case 'block': return sourceFault(FaultScope.FileLocal, SourceFaultCode.Unavailable)
    case 'directory': return catalogFault(FaultScope.DirectoryLocal, CatalogFaultCode.Unavailable)
    case 'peer': return sessionFault(FaultScope.OutputPause, SessionFaultCode.Transport)
  }
}

function sourceTransferFault(error: unknown): Fault | undefined {
  if (error instanceof RevisionChangedDuringRecoveryError) {
    return sourceFault(FaultScope.FileLocal, SourceFaultCode.RevisionInvalidated)
  }
  if (error instanceof FileRevisionChangedError) {
    return sourceFault(FaultScope.FileLocal, SourceFaultCode.RevisionChanged)
  }
  if (error instanceof RevisionLeaseExpiredError) {
    return sourceFault(FaultScope.FileLocal, SourceFaultCode.Unavailable)
  }
  if (error instanceof BlockLaneAttemptsError) {
    return sessionFault(FaultScope.OutputPause, SessionFaultCode.Transport)
  }
  if (error instanceof BlockOperationError) {
    return sessionFault(FaultScope.SessionTerminal, SessionFaultCode.Protocol)
  }
  return undefined
}

function catalogTransferFault(error: unknown): Fault | undefined {
  if (error instanceof DirectoryFailureError) {
    return catalogFault(FaultScope.DirectoryLocal, CatalogFaultCode.Unavailable)
  }
  if (error instanceof DirectoryTraversalError) {
    return catalogFault(FaultScope.DirectoryLocal, CatalogFaultCode.InvalidGeneration)
  }
  if (error instanceof CatalogTraversalError) {
    return catalogFault(FaultScope.SessionTerminal, CatalogFaultCode.InvalidGeneration)
  }
  if (error instanceof SelectionTargetMissingError) {
    return catalogFault(FaultScope.FileLocal, CatalogFaultCode.Unavailable)
  }
  return undefined
}

function findDeclaredOutputFailure(input: unknown): OutputFailure | undefined {
  let error = input
  for (let depth = 0; depth < MAXIMUM_FAILURE_CAUSE_DEPTH; depth += 1) {
    const declared = declaredOutputFailure(error)
    if (declared !== undefined) return declared
    if (!(error instanceof Error) || error.cause === undefined) return undefined
    error = error.cause
  }
  return undefined
}

function outputTransferFault(error: unknown): Fault | undefined {
  if (error instanceof OutputBudgetExceededError) {
    return outputFault(FaultScope.OutputPause, OutputFaultCode.ResourceBudget)
  }
  if (error instanceof OutputSessionCompromisedError) {
    return outputFault(FaultScope.OutputPause, OutputFaultCode.MutationAmbiguous)
  }
  if (error instanceof OutputTransactionContractError) {
    return outputFault(FaultScope.OutputPause, OutputFaultCode.Contract)
  }
  if (error instanceof DirectoryOutputError) {
    return outputFault(FaultScope.DirectoryLocal, OutputFaultCode.DirectoryMetadata)
  }
  const directoryMutation = findOutputDirectoryMutationError(error)
  if (directoryMutation !== undefined) {
    return outputFault(
      FaultScope.OutputPause,
      directoryMutation.sessionCompromised
        ? OutputFaultCode.MutationAmbiguous
        : OutputFaultCode.DirectoryMetadata,
    )
  }
  if (error instanceof FileOutputError || error instanceof OutputPausedError) {
    return outputFault(FaultScope.OutputPause, OutputFaultCode.StateIO)
  }
  return undefined
}

function failureStage(error: unknown): FailureStage {
  const declared = declaredOutputFailure(error)
  if (declared !== undefined) return declared.classification.fact.stage
  if (error instanceof RemoteOperationError) return 'protocol_operation'
  if (error instanceof FileOutputError) {
    return error.materializationFailureReason === 'output-commit-failed'
      ? 'output_commit'
      : 'output_write'
  }
  if (
    error instanceof DirectoryOutputError ||
    findOutputDirectoryMutationError(error) !== undefined
  ) {
    return 'output_commit'
  }
  if (
    error instanceof OutputBudgetExceededError ||
    error instanceof OutputSessionCompromisedError ||
    error instanceof OutputTransactionContractError ||
    error instanceof OutputPausedError
  ) {
    return 'output_write'
  }
  return findDeclaredOutputFailure(error)?.classification.fact.stage ?? 'content_read'
}

function findOutputDirectoryMutationError(input: unknown): OutputDirectoryMutationError | undefined {
  let error = input
  for (let depth = 0; depth < MAXIMUM_FAILURE_CAUSE_DEPTH; depth += 1) {
    if (error instanceof OutputDirectoryMutationError) return error
    if (!(error instanceof Error) || error.cause === undefined) return undefined
    error = error.cause
  }
  return undefined
}

function recoveryDispositionFor(
  error: unknown,
  fault: Fault,
): RecoveryDisposition {
  if (
    error instanceof RemoteOperationError && error.retryable ||
    error instanceof RevisionLeaseExpiredError
  ) {
    return 'retryable'
  }
  switch (fault.scope) {
    case FaultScope.FileLocal:
    case FaultScope.DirectoryLocal:
      return 'none'
    case FaultScope.OutputPause:
      return 'resumable_receive'
    case FaultScope.SessionTerminal:
      return 'terminal'
  }
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

export class FileRevisionChangedError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'FileRevisionChangedError'
  }
}

export class FileOutputError extends Error {
  readonly materializationFailureReason: Extract<
    MaterializationFailureReason,
    'output-write-failed' | 'output-commit-failed'
  >

  constructor(
    message: string,
    reason: Extract<MaterializationFailureReason, 'output-write-failed' | 'output-commit-failed'>,
    cause?: unknown,
  ) {
    super(message, { cause })
    this.name = 'FileOutputError'
    this.materializationFailureReason = reason
  }
}

/**
 * Reads only immediate reviewed types. Materialization policy never walks Error.cause.
 */
export function materializationFailureReason(
  input: unknown,
  fallback: MaterializationFailureReason = 'content-read-failed',
): MaterializationFailureReason {
  if (isClassifiedTransferFailure(input)) return input.materializationFailureReason
  if (input instanceof ClassifiedTransferFailureError) {
    return input.classification.materializationFailureReason
  }
  if (input instanceof FileOutputError) return input.materializationFailureReason
  const outputFailure = declaredOutputFailure(input)
  if (outputFailure !== undefined) return outputFailure.classification.materializationFailureReason
  if (
    isFault(input) &&
    input.domain === FaultDomain.Output &&
    input.scope === FaultScope.DirectoryLocal
  ) {
    return 'directory-finalize-failed'
  }
  if (input instanceof OutputDirectoryMutationError || input instanceof DirectoryOutputError) {
    return 'directory-finalize-failed'
  }
  if (
    input instanceof FileRevisionChangedError ||
    input instanceof RevisionChangedDuringRecoveryError
  ) {
    return 'source-revision-changed'
  }
  if (input instanceof RemoteRevisionError || input instanceof RevisionLeaseExpiredError) {
    return 'file-open-failed'
  }
  if (input instanceof BlockLaneAttemptsError || input instanceof BlockOperationError) {
    return 'content-read-failed'
  }
  return fallback
}

function recordConcurrentCancellationFailure(error: unknown, options: TransferFailureClassificationOptions): void {
  if (error === options.signal?.reason) return
  try {
    options.incidentScope?.facts.record(unclassifiedFailureFact({
      stage: options.stage ?? 'cleanup', recoveryDisposition: 'none', error, operation: 'transfer.cancel.concurrent',
    }), 'consequence')
  } catch { /* Concurrent cleanup evidence cannot override cancellation. */ }
}
