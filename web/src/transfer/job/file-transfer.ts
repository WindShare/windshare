import { compareRevisionDescriptors, sourceMetadataFields } from '../../content/revision-comparison'
import { OutputCapacityBlockedError } from '../capacity-pressure/drain'
import type { ShareDescriptor } from '../../catalog/records'
import { ByteRangeSet, bigintToSafeNumber, byteRange, type ByteRange } from '../../content/geometry'
import {
  BLOCK_BROKER_PARALLEL_READS,
  type BlockRangeReader,
} from '../../content/broker'
import {
  RevisionReopenedDuringRecoveryError,
  type OpenedRevision,
  type RevisionReader,
} from '../../content/session-services'
import {
  observeReadmittedCoverage, observeRevisionReadmission,
  rangeBytes, releaseRevisionLease, validateOpenedFileRevision,
  type FileRevisionReadmission, type FileRevisionReadmissionObservation,
} from './file-authority'
import { settleReopenedFile, settleTransferError, UNCONFIRMED_FILE_SETTLEMENT } from '../settlement/file'
import { encodeBase64Url } from '../../crypto/bytes'
import type { IncidentScopeHandle } from '../../diagnostics/incident'
import {
  OutputTransactionContractError,
  bindOutputFileTransaction,
  ownOutputFileTransaction,
} from '../output/transaction'
import {
  registerFileCheckpoint, systemCheckpointClock,
  type CheckpointClock, type CheckpointObservation, type FileCheckpointController,
} from '../checkpoint/controller'
import {
  OutputBudgetExceededError,
  OutputSessionCompromisedError,
  TransferPauseRequestedError,
  TransferStopRequestedError,
  snapshotOpenedOutputRevision,
  snapshotOutputFile,
  snapshotOutputFileRequest,
  type OutputFile,
  type OutputFileTransaction,
  type OpenedOutputRevision,
  type OutputSession,
} from '../output/session'
import { OutputPausedError, type FileTransferSettlement, type PendingFile } from './contract'
import {
  ClassifiedTransferFailureError,
  type ClassifiedTransferFailure,
} from '../failure/classification'
import {
  FileOutputError,
  materializeClassifiedTransferFailure,
  normalizeFileTransferFailure,
  normalizedFileTransferFault,
  type NormalizedFileTransferFailure,
} from './failures'
import { BoundaryFaultError, joinFaults } from '../failure/fault'
import type { DirectTreeCoordinateContract } from './coordinate/direct-tree'
import type { PerformanceFilePipelineObservation } from '../../diagnostics/performance/performance-runtime-observations'

export class RangeReaderContractError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'RangeReaderContractError'
  }
}

export interface FileTransferOptions {
  readonly descriptor: ShareDescriptor
  readonly revisions: RevisionReader
  readonly broker: BlockRangeReader
  readonly output: OutputSession
  readonly directTreeCoordinates?: DirectTreeCoordinateContract
  readonly signal: AbortSignal
  readonly outputSettlementTimeoutMilliseconds: number
  readonly incidentScope?: IncidentScopeHandle
  readonly performancePipeline?: PerformanceFilePipelineObservation
  readonly onInitialCoverage?: (coverage: Readonly<{ durableBytes: bigint; acceptedBytes: bigint }>) => void
  readonly onWriteAcknowledged: (bytes: bigint, firstWrite: boolean) => void
  readonly onRecoverableAcknowledged?: (
    bytes: bigint,
    transition: 'automatic-checkpoint' | 'final' | 'pause' | 'readmission',
  ) => void
  readonly onSettlement: (settlement: FileTransferSettlement) => void
  readonly onRevisionReadmission?: (event: FileRevisionReadmissionObservation) => void
  readonly checkpointClock?: CheckpointClock
  readonly onCheckpointObservation?: (event: CheckpointObservation) => void
}

/** Each attempt owns one lease and transaction. The file settles after its last attempt. */
export async function transferFile(options: FileTransferOptions, pending: PendingFile): Promise<void> {
  const retry = await transferFileAttempt(options, pending)
  if (retry !== undefined) await transferFileAttempt(options, pending, retry)
}

interface FileAttempt {
  acquired?: OpenedRevision
  transaction?: OutputFileTransaction
  initialDurable?: ByteRangeSet
  checkpoint?: FileCheckpointController
  admitted: boolean
}

interface FileAttemptOutcome {
  readonly settlement: FileTransferSettlement
  readonly failure?: NormalizedFileTransferFailure
  readonly capacityBlocked?: OutputCapacityBlockedError
  readonly retry?: FileRevisionReadmission
}

async function transferFileAttempt(
  options: FileTransferOptions,
  pending: PendingFile,
  previous?: FileRevisionReadmission,
): Promise<FileRevisionReadmission | undefined> {
  const checkpointFailure = new AbortController()
  const active = { ...options, signal: AbortSignal.any([options.signal, checkpointFailure.signal]) }
  const attempt: FileAttempt = { admitted: false }
  let outcome: FileAttemptOutcome = { settlement: UNCONFIRMED_FILE_SETTLEMENT }
  try {
    try {
      const admission = await admitFileAttempt(active, pending, attempt, previous)
      outcome = { settlement: await writeFileAttempt(active, attempt, admission, checkpointFailure) }
    } catch (error) {
      outcome = await settleFileAttemptError(options, attempt, error, previous)
    }
    return outcome.retry
  } finally {
    await finishFileAttempt(active, attempt, outcome)
  }
}

interface FileAttemptAdmission {
  readonly opened: OpenedRevision | undefined
  readonly outputFile: OutputFile
  readonly bound: ReturnType<typeof bindOutputFileTransaction>
  readonly checkpoint: Awaited<ReturnType<OutputSession['beginFile']>>['checkpoint']
}

interface OutputRevisionAdmission {
  attempted: boolean
  opened?: OpenedRevision
  failure?: Readonly<{ reason: unknown }>
}

async function admitFileAttempt(
  options: FileTransferOptions, pending: PendingFile, attempt: FileAttempt, previous?: FileRevisionReadmission,
): Promise<FileAttemptAdmission> {
  options.signal.throwIfAborted()
  const revisionAdmission: OutputRevisionAdmission = { attempted: false }
  const request = snapshotOutputFileRequest({
    source: {
      shareInstance: encodeBase64Url(options.descriptor.shareInstance),
      fileId: pending.entry.idText,
    },
    sourceAuthenticationPath: pending.sourceAuthenticationPath,
    logicalArtifactPath: pending.logicalArtifactPath,
    materializationRelativePath: pending.materializationRelativePath,
    expectedSize: pending.entry.expectedSize,
    ...(pending.parent.kind === 'materialized' ? { parentAdmission: pending.parent.admission } : {}),
    ...(pending.modifiedTime === undefined || !options.output.capabilities.modificationTime
      ? {} : { modifiedTime: pending.modifiedTime }),
    ...(options.performancePipeline === undefined ? {} : { performancePipeline: options.performancePipeline }),
    openRevision: signal => openRevisionForOutput(options, pending, signal, attempt, revisionAdmission, previous),
  }, options.directTreeCoordinates)
  const begun = await outputOperation(
    options.signal, 'Unable to begin the output file transaction', 'output-write-failed',
    () => previous === undefined
      ? options.output.beginFile(request, options.signal)
      : previous.beginFile(request, options.signal),
    () => revisionAdmission.failure,
  )
  // A collaborator can return an invalid binding after acquiring a writer.
  // Take ownership first so the attempt can still settle that writer.
  attempt.transaction = ownOutputFileTransaction(begun)
  const revision = snapshotOpenedOutputRevision(begun.revision)
  const opened = revisionAdmission.opened
  if (revision.shareInstance !== request.source.shareInstance || revision.fileId !== request.source.fileId ||
      (opened !== undefined && (revision.fileRevision !== opened.descriptor.fileRevisionText ||
        revision.exactSize !== opened.descriptor.exactSize))) {
    throw new OutputTransactionContractError('output adapter returned a different authenticated revision')
  }
  const outputFile = outputFileFor(options.output, pending, revision, options.directTreeCoordinates)
  const bound = bindOutputFileTransaction(begun, outputFile, options.output)
  attempt.transaction = bound.transaction
  options.performancePipeline?.transition('block_read_authentication')
  const initialDurable = bound.initialDurable.asRangeSet()
  attempt.initialDurable = initialDurable
  if (opened === undefined && (previous !== undefined || revisionAdmission.attempted ||
      options.output.capabilities.durability === 'None' || !initialDurable.covers(byteRange(0n, outputFile.exactSize)))) {
    throw new OutputTransactionContractError('output adapter omitted revision open without complete durable coverage')
  }
  if (previous !== undefined && opened !== undefined) {
    observeReadmittedCoverage(options, previous, opened.descriptor, initialDurable)
  }
  attempt.admitted = true
  options.onInitialCoverage?.({ durableBytes: rangeBytes(initialDurable), acceptedBytes: rangeBytes(bound.initialAccepted) })
  return { opened, outputFile, bound, checkpoint: begun.checkpoint }
}

async function openRevisionForOutput(
  options: FileTransferOptions, pending: PendingFile, signal: AbortSignal, attempt: FileAttempt,
  admission: OutputRevisionAdmission, previous?: FileRevisionReadmission,
): Promise<OpenedOutputRevision> {
  if (admission.attempted) {
    throw new OutputTransactionContractError('output adapter invoked the authenticated revision callback more than once')
  }
  admission.attempted = true
  options.performancePipeline?.transition('revision_open')
  try {
    attempt.acquired = await options.revisions.open(pending.entry.id, signal)
    const opened = validateOpenedFileRevision(options.descriptor, pending.entry, attempt.acquired)
    if (previous !== undefined && compareRevisionDescriptors(previous.previous, opened.descriptor).kind !== 'metadata-agreement') {
      throw new RevisionReopenedDuringRecoveryError(previous.previous, opened.descriptor)
    }
    admission.opened = opened
    return snapshotOpenedOutputRevision({
      shareInstance: opened.descriptor.shareInstanceId,
      fileId: opened.descriptor.fileIdText,
      fileRevision: opened.descriptor.fileRevisionText,
      ...sourceMetadataFields(opened.descriptor),
      exactSize: opened.descriptor.exactSize,
    })
  } catch (reason) {
    // Keep source authority through the adapter await so a source error cannot
    // be mistaken for an output mutation failure.
    admission.failure = Object.freeze({ reason })
    throw reason
  } finally {
    options.performancePipeline?.transition('initial_lineage')
  }
}

async function writeFileAttempt(
  options: FileTransferOptions, attempt: FileAttempt, admission: FileAttemptAdmission,
  checkpointFailure: AbortController,
): Promise<FileTransferSettlement> {
  const { bound, opened, outputFile } = admission
  const transaction = bound.transaction
  const wanted = new ByteRangeSet(outputFile.exactSize, [byteRange(0n, outputFile.exactSize)])
  const missing = bound.initialAccepted.missingFrom(wanted)
  const initialDurable = bound.initialDurable.asRangeSet()
  const initialBytes = rangeBytes(initialDurable)
  const initialPendingBytes = rangeBytes(bound.initialAccepted) - initialBytes
  const checkpoint = registerFileCheckpoint(options.output, {
    object: admission.checkpoint ?? { objectId: bound.initialDurable.ownership.ownedFileIdentity,
      policy: { kind: 'disabled' } },
    durableBytes: initialBytes, pendingBytes: initialPendingBytes,
    remainingBytes: rangeBytes(missing), signal: options.signal,
    clock: options.checkpointClock ?? systemCheckpointClock,
    checkpoint: async trigger => {
      const result = await outputOperation(options.signal, 'Unable to checkpoint the output file',
        'output-write-failed', () => transaction.automaticCheckpoint(trigger, options.signal))
      return result.kind === 'advanced'
        ? { kind: 'advanced', durableBytes: rangeBytes(result.durable.asRangeSet()) } : result
    },
    onAdvanced: bytes => options.onRecoverableAcknowledged?.(bytes, 'automatic-checkpoint'),
    onFailure: error => checkpointFailure.abort(error),
    ...(options.onCheckpointObservation === undefined ? {} : { observe: options.onCheckpointObservation }),
  })
  attempt.checkpoint = checkpoint
  options.signal.throwIfAborted()
  let wrote = initialPendingBytes > 0n
  for (const missingRange of missing.ranges) {
    if (opened === undefined) throw new OutputTransactionContractError('missing bytes require a live revision lease')
    wrote = await transferMissingRange(options, opened, transaction, missingRange, wrote, checkpoint)
  }
  await checkpoint.drain()
  options.performancePipeline?.transition('final_transaction')
  await outputOperation(options.signal, 'Unable to commit the output file', 'output-commit-failed',
    () => transaction.commit(options.signal))
  return Object.freeze({ kind: 'completed', exactSize: outputFile.exactSize })
}

async function settleFileAttemptError(
  options: FileTransferOptions, attempt: FileAttempt, error: unknown, previous?: FileRevisionReadmission,
): Promise<FileAttemptOutcome> {
  const started = performance.now()
  const failure = await drainCheckpointFailure(attempt.checkpoint, error)
  // The checkpoint controller can abort the reader to drain it. The caller's
  // signal still decides whether to start another attempt.
  const outcome = failure instanceof RevisionReopenedDuringRecoveryError && attempt.transaction !== undefined &&
      attempt.initialDurable !== undefined && options.output.capabilities.durability !== 'None'
    ? await settleReopenedFile(options, attempt.transaction, failure, attempt.initialDurable, started, previous !== undefined)
    : await settleTransferError(options, attempt.transaction, failure)
  if (previous === undefined || attempt.admitted || outcome.settlement.kind !== 'unconfirmed') return outcome
  const compared = error instanceof RevisionReopenedDuringRecoveryError ? error : previous
  const retainedBytes = rangeBytes(previous.paused.asRangeSet())
  observeRevisionReadmission(options, previous.previous, compared.current, compared.comparison,
    'admission-failed', retainedBytes, 0n, previous.started)
  return { ...outcome, settlement: { kind: 'paused', retainedBytes } }
}

async function finishFileAttempt(
  options: FileTransferOptions, attempt: FileAttempt, outcome: FileAttemptOutcome,
): Promise<void> {
  try {
    observeSettlementCoverage(options, outcome.settlement, attempt.checkpoint)
    if (outcome.retry === undefined) options.onSettlement(outcome.settlement)
  } finally {
    await retireFileAttemptLease(options, attempt, outcome)
  }
}

async function retireFileAttemptLease(
  options: FileTransferOptions, attempt: FileAttempt, outcome: FileAttemptOutcome,
): Promise<void> {
  try {
    await finishTransferFile(options, attempt.acquired, outcome.failure, outcome.capacityBlocked)
  } catch (error) {
    if (outcome.retry !== undefined) options.onSettlement(outcome.settlement)
    throw error
  }
}

function observeSettlementCoverage(
  options: FileTransferOptions,
  settlement: FileTransferSettlement,
  checkpoint: FileCheckpointController | undefined,
): void {
  if (settlement.kind !== 'unconfirmed' && checkpoint !== undefined) {
    const durableBytes = settlement.kind === 'paused' ? settlement.retainedBytes : settlement.exactSize
    const advancedBytes = durableBytes - checkpoint.durableBytes
    if (advancedBytes > 0n) {
      options.onRecoverableAcknowledged?.(advancedBytes, settlement.kind === 'paused' ? 'pause' : 'final')
    }
  }
}

async function drainCheckpointFailure(
  checkpoint: FileCheckpointController | undefined, failure: unknown,
): Promise<unknown> {
  try { await checkpoint?.drain() } catch (checkpointError) { return checkpointError }
  return failure
}

async function finishTransferFile(
  options: FileTransferOptions,
  acquired: OpenedRevision | undefined,
  primaryFailure: NormalizedFileTransferFailure | undefined,
  capacityBlocked: OutputCapacityBlockedError | undefined,
): Promise<void> {
  const releaseFailure = await releaseRevisionLease(acquired)
  if (capacityBlocked !== undefined && releaseFailure === undefined) {
    try { await capacityBlocked.waitForDrain(options.signal) } catch (error) {
      primaryFailure = normalizeFileTransferFailure(error, { signal: options.signal })
    }
  }
  throwTransferOrReleaseFailure(options.incidentScope, primaryFailure, releaseFailure)
}

function throwTransferOrReleaseFailure(
  incidentScope: IncidentScopeHandle | undefined,
  primaryFailure: NormalizedFileTransferFailure | undefined,
  releaseFailure: NormalizedFileTransferFailure | undefined,
): void {
  if (releaseFailure === undefined) {
    if (primaryFailure !== undefined) throw primaryFailure.diagnostic
    return
  }
  if (primaryFailure === undefined) {
    if (releaseFailure.kind === 'canceled') throw releaseFailure.diagnostic
    throw classifiedFailureError(materializeClassifiedTransferFailure(
      releaseFailure.diagnostic.classification,
      incidentScope,
      'contributor',
    ))
  }

  // Cancellation controls the run outside the fault lattice. A concurrent
  // settlement failure is retained only as a consequence of that product decision.
  if (primaryFailure.kind === 'canceled') {
    if (releaseFailure.kind === 'fault') {
      materializeClassifiedTransferFailure(
        releaseFailure.diagnostic.classification,
        incidentScope,
        'consequence',
      )
    }
    throw primaryFailure.diagnostic
  }
  if (releaseFailure.kind === 'canceled') throw primaryFailure.diagnostic

  materializeClassifiedTransferFailure(
    releaseFailure.diagnostic.classification,
    incidentScope,
    'consequence',
  )
  const governing = joinFaults(primaryFailure.fault, releaseFailure.fault)
  if (governing === undefined) throw primaryFailure.diagnostic
  throw normalizedFileTransferFault(governing, {
    error: primaryFailure.diagnostic,
    ...(incidentScope === undefined ? {} : { incidentScope }),
    stage: 'settlement',
    materializationFailureReason: primaryFailure.materializationFailureReason,
  }).diagnostic
}

function classifiedFailureError(
  classification: ClassifiedTransferFailure,
): ClassifiedTransferFailureError {
  return new ClassifiedTransferFailureError(classification)
}

function outputFileFor(
  output: OutputSession,
  pending: PendingFile,
  revision: OpenedOutputRevision,
  directTreeCoordinates: DirectTreeCoordinateContract | undefined,
): OutputFile {
  return snapshotOutputFile({
    source: {
      shareInstance: revision.shareInstance,
      fileId: revision.fileId,
      fileRevision: revision.fileRevision,
    },
    sourceAuthenticationPath: pending.sourceAuthenticationPath,
    logicalArtifactPath: pending.logicalArtifactPath,
    materializationRelativePath: pending.materializationRelativePath,
    exactSize: revision.exactSize,
    ...(pending.parent.kind === 'materialized'
      ? { parentAdmission: pending.parent.admission }
      : {}),
    ...(pending.modifiedTime === undefined || !output.capabilities.modificationTime
      ? {}
      : { modifiedTime: pending.modifiedTime }),
  }, directTreeCoordinates)
}

async function transferMissingRange(
  options: FileTransferOptions,
  opened: OpenedRevision,
  transaction: OutputFileTransaction,
  requested: ByteRange,
  wrote: boolean,
  checkpoint: FileCheckpointController,
): Promise<boolean> {
  if (requested.start >= requested.end) {
    throw new RangeReaderContractError('requested transfer range is empty')
  }
  const geometry = opened.descriptor.geometry
  let covered = requested.start
  let atomic = atomicRangeAt(geometry.blockSize, covered, requested.end)
  let data = new Uint8Array(bigintToSafeNumber(atomic.end - atomic.start, 'atomic output range'))
  let filled = 0
  // One range iterator lets the broker refill its global block window while the
  // consumer awaits output I/O. Atomic block assembly keeps that network
  // pipeline from exposing partial collaborator slices to the output authority.
  for await (const slice of options.broker.readRange(
    opened.descriptor,
    opened.leaseId,
    requested,
    {
      signal: options.signal,
      maximumParallel: BLOCK_BROKER_PARALLEL_READS,
      priority: 'download',
    },
  )) {
    options.signal.throwIfAborted()
    if (typeof slice.offset !== 'bigint' || !(slice.data instanceof Uint8Array) || slice.data.byteLength === 0) {
      throw new RangeReaderContractError('range reader returned an empty or malformed slice')
    }
    const end = slice.offset + BigInt(slice.data.byteLength)
    if (slice.offset !== covered || end > requested.end) {
      throw new RangeReaderContractError(
        'range reader slice escapes, overlaps, or leaves a gap in the requested interval',
      )
    }
    let sliceOffset = 0
    while (sliceOffset < slice.data.byteLength) {
      const available = data.byteLength - filled
      const consumed = Math.min(available, slice.data.byteLength - sliceOffset)
      data.set(slice.data.subarray(sliceOffset, sliceOffset + consumed), filled)
      filled += consumed
      sliceOffset += consumed
      covered += BigInt(consumed)
      if (filled !== data.byteLength) continue
      wrote = await acceptAtomicOutputRange(
        options,
        transaction,
        atomic.start,
        data,
        wrote,
        checkpoint,
      )
      if (covered < requested.end) {
        atomic = atomicRangeAt(geometry.blockSize, covered, requested.end)
        data = new Uint8Array(bigintToSafeNumber(atomic.end - atomic.start, 'atomic output range'))
        filled = 0
      }
    }
  }
  options.signal.throwIfAborted()
  if (covered !== requested.end || filled !== data.byteLength) {
    throw new RangeReaderContractError('range reader returned before covering the requested interval')
  }
  return wrote
}

async function acceptAtomicOutputRange(
  options: FileTransferOptions,
  transaction: OutputFileTransaction,
  offset: bigint,
  data: Uint8Array<ArrayBuffer>,
  wrote: boolean,
  checkpoint: FileCheckpointController,
): Promise<boolean> {
  const writtenBytes = BigInt(data.byteLength)
  options.performancePipeline?.transition('writer_lifecycle')
  try {
    await checkpoint.write(writtenBytes, async () => {
      await writeAtomicRange(options, transaction, offset, data)
      options.onWriteAcknowledged(writtenBytes, !wrote)
    })
    return true
  } finally {
    options.performancePipeline?.transition('block_read_authentication')
  }
}

function atomicRangeAt(blockSize: bigint, offset: bigint, requestedEnd: bigint): ByteRange {
  if (blockSize <= 0n || offset < 0n || offset >= requestedEnd) {
    throw new RangeReaderContractError('content geometry lost an atomic output range')
  }
  const blockEnd = (offset / blockSize + 1n) * blockSize
  return byteRange(offset, blockEnd < requestedEnd ? blockEnd : requestedEnd)
}

async function writeAtomicRange(
  options: FileTransferOptions,
  transaction: OutputFileTransaction,
  offset: bigint,
  data: Uint8Array<ArrayBuffer>,
): Promise<void> {
  await outputOperation(
    options.signal,
    'Unable to write the output file range',
    'output-write-failed',
    () => transaction.writeRange(offset, data, options.signal),
  )
}

async function outputOperation<T>(
  signal: AbortSignal,
  message: string,
  reason: 'output-write-failed' | 'output-commit-failed',
  operation: () => Promise<T>,
  collaboratorFailure?: () => Readonly<{ readonly reason: unknown }> | undefined,
): Promise<T> {
  try {
    return await operation()
  } catch (cause) {
    const collaborator = collaboratorFailure?.()
    if (collaborator !== undefined) throw collaborator.reason
    if (cause instanceof OutputTransactionContractError ||
        cause instanceof BoundaryFaultError ||
        cause instanceof TransferPauseRequestedError ||
        cause instanceof TransferStopRequestedError ||
        cause instanceof OutputBudgetExceededError ||
        cause instanceof OutputSessionCompromisedError ||
        cause instanceof OutputPausedError ||
        signal.aborted) throw cause
    throw new FileOutputError(message, reason, cause)
  }
}
