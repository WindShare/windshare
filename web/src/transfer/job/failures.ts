import { V2DirectoryFailureError } from '../../catalog/v2-client'
import { V2BlockLaneAttemptsError } from '../../content/v2-broker'
import {
  V2BlockOperationError,
  V2RemoteOperationError,
  V2RemoteRevisionError,
  V2RevisionCapacityBusyError,
  V2RevisionChangedDuringRecoveryError,
  V2RevisionLeaseExpiredError,
} from '../../content/v2-session-services'
import {
  faultFailureFact,
  isFailureFact,
  protocolFailureFact,
  type FailureFact,
  type FailureFactRef,
  type FailureFactRelation,
  type FailureStage,
  type IncidentScopeHandle,
  type RecoveryDisposition,
} from '../../diagnostics/incident'
import {
  CheckpointLineageDecisionError,
  DestinationCollisionError,
  SourceRevisionChangedError,
} from '../../output/persistent-tree/errors'
import { OutputTransactionContractError } from '../output-file-transaction'
import {
  OutputDirectoryMutationError,
  OutputBudgetExceededError,
  OutputSessionCompromisedError,
  TransferPauseRequestedError,
  TransferStopRequestedError,
} from '../output-session'
import {
  BoundaryFaultError,
  CatalogFaultCode,
  CheckpointFaultCode,
  FaultDomain,
  FaultScope,
  OutputFaultCode,
  SessionFaultCode,
  SourceFaultCode,
  catalogFault,
  checkpointFault,
  dependencyContractFault,
  isFault,
  normalizeBoundaryError,
  outputFault,
  sessionFault,
  sourceFault,
  type Fault,
} from '../fault'
import type { TransferFileOutcomeEvidence } from '../outcome'
import {
  MATERIALIZATION_FAILURE_REASONS,
  V2CatalogTraversalError,
  V2DirectoryTraversalError,
  V2OutputPausedError,
  type MaterializationFailureReason,
} from './contract'
import { V2SelectionTargetMissingError } from './selection'
import { V2RevisionCapacityWaitBudgetError } from '../revision-capacity/public'

const REVISION_FAILURE_STALE = 0x3001
const REVISION_FAILURE_NOT_FOUND = 0x3002
const REVISION_FAILURE_UNREADABLE = 0x3003
const REVISION_FAILURE_UNSUPPORTED_STABILITY = 0x3004
const REVISION_FAILURE_DRIFT = 0x3007
const MAXIMUM_FAILURE_CAUSE_DEPTH = 8

export interface ClassifiedTransferFailure {
  readonly fault: Fault
  readonly fact: FailureFact
  readonly factRef?: FailureFactRef
  readonly materializationFailureReason: MaterializationFailureReason
}

export interface TransferFailureClassificationOptions {
  readonly signal?: AbortSignal
  readonly incidentScope?: IncidentScopeHandle
  readonly relation?: FailureFactRelation
  readonly stage?: FailureStage
  readonly materializationFailureReason?: MaterializationFailureReason
}

export class V2ClassifiedTransferFailureError extends BoundaryFaultError {
  readonly classification: ClassifiedTransferFailure
  readonly fileOutcomeEvidence: TransferFileOutcomeEvidence | undefined

  constructor(
    classification: ClassifiedTransferFailure,
    fileOutcomeEvidence?: TransferFileOutcomeEvidence,
  ) {
    super(classification.fault, 'File transfer failed at a classified boundary')
    this.name = 'V2ClassifiedTransferFailureError'
    this.classification = snapshotClassification(classification)
    this.fileOutcomeEvidence = fileOutcomeEvidence === undefined
      ? undefined
      : snapshotTransferFileOutcomeEvidence(fileOutcomeEvidence)
  }
}

export class V2DirectoryOutputError extends V2ClassifiedTransferFailureError {
  readonly directoryId: string | undefined

  constructor(classification: ClassifiedTransferFailure, directoryId?: string) {
    super(classification)
    this.name = 'V2DirectoryOutputError'
    this.directoryId = directoryId
  }
}

export function isolatedDirectoryOutputFailure(
  error: unknown,
  fileFailureIsolation: boolean,
  directoryId?: string,
  incidentScope?: IncidentScopeHandle,
): V2DirectoryOutputError | undefined {
  const mutation = findOutputDirectoryMutationError(error)
  if (
    !fileFailureIsolation ||
    mutation === undefined ||
    mutation.sessionCompromised
  ) {
    return undefined
  }
  const normalized = normalizedV2FileTransferFault(
    outputFault(FaultScope.DirectoryLocal, OutputFaultCode.DirectoryMetadata),
    {
      stage: 'output_commit',
      materializationFailureReason: 'directory-finalize-failed',
      ...(incidentScope === undefined ? {} : { incidentScope }),
    },
  )
  return new V2DirectoryOutputError(normalized.diagnostic.classification, directoryId)
}

export type NormalizedV2FileTransferFailure =
  | Readonly<{ kind: 'canceled', diagnostic: unknown }>
  | Readonly<{
      kind: 'fault'
      fault: Fault
      fact: FailureFact
      factRef?: FailureFactRef
      materializationFailureReason: MaterializationFailureReason
      diagnostic: V2ClassifiedTransferFailureError
    }>

/**
 * Maps one immediate collaborator result into product and diagnostic semantics.
 * A previously classified wrapper is materialized into a scope, never classified again.
 */
export function normalizeV2FileTransferFailure(
  error: unknown,
  options: TransferFailureClassificationOptions = {},
): NormalizedV2FileTransferFailure {
  if (error instanceof TransferPauseRequestedError || error instanceof TransferStopRequestedError) {
    return Object.freeze({ kind: 'canceled', diagnostic: error })
  }
  if (options.signal?.aborted === true) {
    return Object.freeze({
      kind: 'canceled',
      diagnostic: options.signal.reason ?? error,
    })
  }
  if (error instanceof V2RevisionCapacityWaitBudgetError) {
    return normalizedV2FileTransferClassification(Object.freeze({
      fault: outputFault(FaultScope.OutputPause, OutputFaultCode.ResourceBudget),
      fact: error.failureFact,
      materializationFailureReason: error.surface === 'revision_open'
        ? 'file-open-failed'
        : 'content-read-failed',
    }), options)
  }
  if (error instanceof V2RevisionCapacityBusyError) {
    // This is a defensive terminal seam. Normal transfer composition intercepts
    // the exact authenticated type in the wrapped content ports before normalization.
    return normalizedV2FileTransferClassification(Object.freeze({
      fault: outputFault(FaultScope.OutputPause, OutputFaultCode.ResourceBudget),
      fact: protocolFailureFact({
        stage: 'protocol_operation',
        recoveryDisposition: 'resumable_receive',
        protocolFailure: error.protocolFailure,
      }),
      materializationFailureReason: 'file-open-failed',
    }), options)
  }
  if (error instanceof V2ClassifiedTransferFailureError) {
    return normalizedFromClassification(
      materializeClassifiedTransferFailure(
        error.classification,
        options.incidentScope,
        options.relation,
      ),
      error.fileOutcomeEvidence,
    )
  }

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
  if (error instanceof V2RemoteOperationError) {
    return normalizedV2FileTransferClassification(
      Object.freeze({
        fault,
        fact: error.failureFact,
        materializationFailureReason: reason,
      }),
      options,
      fileOutcomeEvidence,
    )
  }
  if (error instanceof V2RemoteRevisionError) {
    return normalizedV2FileTransferClassification(
      Object.freeze({
        fault,
        fact: error.failureFact,
        materializationFailureReason: reason,
      }),
      options,
      fileOutcomeEvidence,
    )
  }
  return normalizedV2FileTransferFault(fault, {
    ...options,
    stage,
    materializationFailureReason: reason,
    recoveryDisposition,
    ...(fileOutcomeEvidence === undefined ? {} : { fileOutcomeEvidence }),
  })
}

export function transferFileOutcomeEvidence(
  input: unknown,
): TransferFileOutcomeEvidence | undefined {
  let error = input
  for (let depth = 0; depth < MAXIMUM_FAILURE_CAUSE_DEPTH; depth += 1) {
    if (error instanceof V2ClassifiedTransferFailureError &&
        error.fileOutcomeEvidence !== undefined) {
      return error.fileOutcomeEvidence
    }
    if (error instanceof CheckpointLineageDecisionError) {
      return Object.freeze({ kind: 'checkpoint-decision', decision: error.decision })
    }
    if (error instanceof DestinationCollisionError) {
      return Object.freeze({ kind: 'occupied-unbound-destination' })
    }
    if (error instanceof SourceRevisionChangedError ||
        error instanceof V2RevisionChangedDuringRecoveryError ||
        error instanceof V2FileRevisionChangedError ||
        (error instanceof V2RemoteRevisionError &&
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

export function normalizedV2FileTransferFault(
  fault: Fault,
  options: Omit<TransferFailureClassificationOptions, 'signal'> & {
    readonly recoveryDisposition?: RecoveryDisposition
    readonly fileOutcomeEvidence?: TransferFileOutcomeEvidence
  } = {},
): NormalizedV2FileTransferFailure & { readonly kind: 'fault' } {
  const stage = options.stage ?? 'content_read'
  return normalizedV2FileTransferClassification(Object.freeze({
    fault,
    fact: faultFailureFact({
      stage,
      recoveryDisposition: options.recoveryDisposition ?? recoveryDispositionFor(undefined, fault),
      fault,
    }),
    materializationFailureReason: options.materializationFailureReason ?? 'content-read-failed',
  }), options, options.fileOutcomeEvidence)
}

function normalizedV2FileTransferClassification(
  classification: ClassifiedTransferFailure,
  options: Pick<TransferFailureClassificationOptions, 'incidentScope' | 'relation'>,
  fileOutcomeEvidence?: TransferFileOutcomeEvidence,
): NormalizedV2FileTransferFailure & { readonly kind: 'fault' } {
  return normalizedFromClassification(
    materializeClassifiedTransferFailure(
      classification,
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
  const normalized = normalizeV2FileTransferFailure(error, options)
  return normalized.kind === 'fault'
    ? normalized.diagnostic.classification
    : undefined
}

/** Only normalized file-local values may be isolated by TransferJob. */
export function isV2FileScopedTransferFailure(error: unknown): boolean {
  return error instanceof V2ClassifiedTransferFailureError &&
    error.classification.fault.scope === FaultScope.FileLocal
}

function normalizedFromClassification(
  classification: ClassifiedTransferFailure,
  fileOutcomeEvidence?: TransferFileOutcomeEvidence,
): NormalizedV2FileTransferFailure & { readonly kind: 'fault' } {
  const snapshot = snapshotClassification(classification)
  const diagnostic = new V2ClassifiedTransferFailureError(snapshot, fileOutcomeEvidence)
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
  if (snapshot.factRef !== undefined || incidentScope === undefined) return snapshot
  let factRef: FailureFactRef | undefined
  try {
    factRef = incidentScope.facts.record(snapshot.fact, relation)
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

function snapshotTransferFileOutcomeEvidence(
  evidence: TransferFileOutcomeEvidence,
): TransferFileOutcomeEvidence {
  return Object.freeze({ ...evidence })
}

function snapshotClassification(
  classification: ClassifiedTransferFailure,
): ClassifiedTransferFailure {
  if (!isClassifiedTransferFailure(classification)) {
    throw new TypeError('Transfer failure classification is not closed and immutable')
  }
  return Object.freeze({
    fault: classification.fault,
    fact: classification.fact,
    ...(classification.factRef === undefined ? {} : { factRef: classification.factRef }),
    materializationFailureReason: classification.materializationFailureReason,
  })
}

function fileTransferFault(error: unknown): Fault | undefined {
  if (isFault(error) && Object.isFrozen(error)) return error
  if (error instanceof BoundaryFaultError) return error.fault
  return persistentFileTransferFault(error) ??
    remoteTransferFault(error) ??
    sourceTransferFault(error) ??
    catalogTransferFault(error) ??
    outputTransferFault(error)
}

function remoteTransferFault(error: unknown): Fault | undefined {
  if (error instanceof V2RemoteRevisionError) {
    return revisionFailureFault(error.failure.code, error.failure.retryable)
  }
  if (!(error instanceof V2RemoteOperationError)) return undefined
  switch (error.scope) {
    case 'revision': return revisionFailureFault(error.code, error.retryable)
    case 'block': return sourceFault(FaultScope.FileLocal, SourceFaultCode.Unavailable)
    case 'directory': return catalogFault(FaultScope.DirectoryLocal, CatalogFaultCode.Unavailable)
    case 'peer': return sessionFault(FaultScope.OutputPause, SessionFaultCode.Transport)
  }
}

function sourceTransferFault(error: unknown): Fault | undefined {
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
  return undefined
}

function catalogTransferFault(error: unknown): Fault | undefined {
  if (error instanceof V2DirectoryFailureError) {
    return catalogFault(FaultScope.DirectoryLocal, CatalogFaultCode.Unavailable)
  }
  if (error instanceof V2DirectoryTraversalError) {
    return catalogFault(FaultScope.DirectoryLocal, CatalogFaultCode.InvalidGeneration)
  }
  if (error instanceof V2CatalogTraversalError) {
    return catalogFault(FaultScope.SessionTerminal, CatalogFaultCode.InvalidGeneration)
  }
  if (error instanceof V2SelectionTargetMissingError) {
    return catalogFault(FaultScope.FileLocal, CatalogFaultCode.Unavailable)
  }
  return undefined
}

function persistentFileTransferFault(input: unknown): Fault | undefined {
  let error = input
  for (let depth = 0; depth < MAXIMUM_FAILURE_CAUSE_DEPTH; depth += 1) {
    if (error instanceof CheckpointLineageDecisionError) {
      switch (error.decision) {
        case 'revision-conflict':
          return checkpointFault(FaultScope.FileLocal, CheckpointFaultCode.UnsafeInstall)
        case 'ownership-conflict':
          return checkpointFault(FaultScope.FileLocal, CheckpointFaultCode.OwnershipMismatch)
        case 'invalid':
          return checkpointFault(FaultScope.FileLocal, CheckpointFaultCode.CorruptRecord)
      }
    }
    if (error instanceof DestinationCollisionError) {
      return outputFault(FaultScope.FileLocal, OutputFaultCode.NamespaceUnsafe)
    }
    if (error instanceof SourceRevisionChangedError) {
      return sourceFault(FaultScope.FileLocal, SourceFaultCode.RevisionChanged)
    }
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
  if (error instanceof V2DirectoryOutputError) {
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
  if (error instanceof V2FileOutputError || error instanceof V2OutputPausedError) {
    return outputFault(FaultScope.OutputPause, OutputFaultCode.StateIO)
  }
  return undefined
}

function failureStage(error: unknown): FailureStage {
  if (error instanceof V2RemoteOperationError) return 'protocol_operation'
  if (
    error instanceof CheckpointLineageDecisionError ||
    error instanceof DestinationCollisionError ||
    error instanceof SourceRevisionChangedError
  ) {
    return 'output_write'
  }
  if (error instanceof V2FileOutputError) {
    return error.materializationFailureReason === 'output-commit-failed'
      ? 'output_commit'
      : 'output_write'
  }
  if (
    error instanceof V2DirectoryOutputError ||
    findOutputDirectoryMutationError(error) !== undefined
  ) {
    return 'output_commit'
  }
  if (
    error instanceof OutputBudgetExceededError ||
    error instanceof OutputSessionCompromisedError ||
    error instanceof OutputTransactionContractError ||
    error instanceof V2OutputPausedError
  ) {
    return 'output_write'
  }
  return 'content_read'
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
    error instanceof V2RemoteOperationError && error.retryable ||
    error instanceof V2RevisionLeaseExpiredError
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

export class V2FileRevisionChangedError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'V2FileRevisionChangedError'
  }
}

export class V2FileOutputError extends Error {
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
    this.name = 'V2FileOutputError'
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
  if (input instanceof V2ClassifiedTransferFailureError) {
    return input.classification.materializationFailureReason
  }
  if (input instanceof V2FileOutputError) return input.materializationFailureReason
  if (
    isFault(input) &&
    input.domain === FaultDomain.Output &&
    input.scope === FaultScope.DirectoryLocal
  ) {
    return 'directory-finalize-failed'
  }
  if (input instanceof OutputDirectoryMutationError || input instanceof V2DirectoryOutputError) {
    return 'directory-finalize-failed'
  }
  if (
    input instanceof V2FileRevisionChangedError ||
    input instanceof V2RevisionChangedDuringRecoveryError
  ) {
    return 'source-revision-changed'
  }
  if (input instanceof V2RemoteRevisionError || input instanceof V2RevisionLeaseExpiredError) {
    return 'file-open-failed'
  }
  if (input instanceof V2BlockLaneAttemptsError || input instanceof V2BlockOperationError) {
    return 'content-read-failed'
  }
  return fallback
}

export function isClassifiedTransferFailure(
  value: unknown,
): value is ClassifiedTransferFailure {
  if (typeof value !== 'object' || value === null || !Object.isFrozen(value)) return false
  const candidate = value as Partial<ClassifiedTransferFailure>
  return isFault(candidate.fault) &&
    Object.isFrozen(candidate.fault) &&
    isFailureFact(candidate.fact) &&
    factMatchesFault(candidate.fact, candidate.fault) &&
    typeof candidate.materializationFailureReason === 'string' &&
    MATERIALIZATION_FAILURE_REASONS.some(reason =>
      reason === candidate.materializationFailureReason)
}

function factMatchesFault(fact: FailureFact, fault: Fault): boolean {
  return fact.kind !== 'fault' || fact.payload.fault === fault
}
