import type { V2ShareDescriptor } from '../../catalog/v2-records'
import { ByteRangeSet, bigintToSafeNumber, byteRange, type ByteRange } from '../../content/geometry'
import {
  V2_BLOCK_BROKER_PARALLEL_READS,
  type V2BlockRangeReader,
} from '../../content/v2-broker'
import type {
  V2OpenedRevision,
  V2RevisionReader,
} from '../../content/v2-session-services'
import { encodeBase64Url } from '../../crypto/bytes'
import type { IncidentScopeHandle } from '../../diagnostics/incident'
import {
  OutputTransactionContractError,
  retireOutputFileTransaction,
  bindOutputFileTransaction,
  ownOutputFileTransaction,
} from '../output-file-transaction'
import {
  createCheckpointSchedule,
  nextCheckpointRetryPendingBytes,
} from '../checkpoint-schedule'
import {
  OutputBudgetExceededError,
  OutputSessionCompromisedError,
  TransferPauseRequestedError,
  TransferStopRequestedError,
  snapshotOpenedOutputRevision,
  snapshotOutputFile,
  snapshotOutputFileRequest,
  type AutomaticCheckpointTrigger,
  type OutputFile,
  type OutputExecutionProfileBoundedCheckpoint,
  type OutputSession,
} from '../output-session'
import { V2OutputPausedError, type PendingFile } from './contract'
import {
  V2ClassifiedTransferFailureError,
  V2FileOutputError,
  materializeClassifiedTransferFailure,
  normalizeV2FileTransferFailure,
  normalizedV2FileTransferFault,
  type ClassifiedTransferFailure,
  type NormalizedV2FileTransferFailure,
} from './failures'
import {
  BoundaryFaultError,
  FaultScope,
  OutputFaultCode,
  authorizeFileRetirement,
  joinFaults,
  outputFault,
  promoteFaultScope,
} from '../fault'
import { validateOpenedFileRevision } from './file-authority'
import type { DirectTreeCoordinateContract } from './coordinate/direct-tree'
import { withOutputSettlementTimeout } from '../settlement/v2-output'
import type { PerformanceFilePipelineObservation } from '../../output/diagnostics/performance-runtime-observations'

export class V2RangeReaderContractError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'V2RangeReaderContractError'
  }
}

export interface V2FileTransferOptions {
  readonly descriptor: V2ShareDescriptor
  readonly revisions: V2RevisionReader
  readonly broker: V2BlockRangeReader
  readonly output: OutputSession
  readonly directTreeCoordinates?: DirectTreeCoordinateContract
  readonly signal: AbortSignal
  readonly outputSettlementTimeoutMilliseconds: number
  readonly incidentScope?: IncidentScopeHandle
  readonly performancePipeline?: PerformanceFilePipelineObservation
  readonly onWriteAcknowledged: (bytes: bigint, firstWrite: boolean) => void
  readonly onRecoverableAcknowledged?: (
    bytes: bigint,
    transition: 'automatic-checkpoint' | 'final',
  ) => void
  readonly onComplete: (exactSize: bigint) => void
  readonly now?: () => number
}

type BoundOutputFileTransaction = ReturnType<typeof bindOutputFileTransaction>['transaction']

/** Owns one revision lease and one output transaction from acquisition through settlement. */
export async function transferV2File(
  options: V2FileTransferOptions,
  pending: PendingFile,
): Promise<void> {
  let acquired: V2OpenedRevision | undefined
  let opened: V2OpenedRevision | undefined
  let transaction: BoundOutputFileTransaction | undefined
  let primaryFailure: NormalizedV2FileTransferFailure | undefined
  try {
    let revisionOpenAttempted = false
    let revisionOpenFailure: Readonly<{ readonly reason: unknown }> | undefined
    const request = snapshotOutputFileRequest({
      source: {
        shareInstance: encodeBase64Url(options.descriptor.shareInstance),
        fileId: pending.entry.idText,
      },
      sourceAuthenticationPath: pending.sourceAuthenticationPath,
      logicalArtifactPath: pending.logicalArtifactPath,
      materializationRelativePath: pending.materializationRelativePath,
      expectedSize: pending.entry.expectedSize,
      ...(pending.parent.kind === 'materialized'
        ? { parentAdmission: pending.parent.admission }
        : {}),
      ...(pending.modifiedTime === undefined || !options.output.capabilities.modificationTime
        ? {}
        : { modifiedTime: pending.modifiedTime }),
      ...(options.performancePipeline === undefined
        ? {}
        : { performancePipeline: options.performancePipeline }),
      openRevision: async (signal) => {
        if (revisionOpenAttempted) {
          throw new OutputTransactionContractError(
            'output adapter invoked the authenticated revision callback more than once',
          )
        }
        revisionOpenAttempted = true
        options.performancePipeline?.transition('revision_open')
        try {
          acquired = await options.revisions.open(pending.entry.id, signal)
          opened = validateOpenedFileRevision(options.descriptor, pending.entry, acquired)
          return snapshotOpenedOutputRevision({
            shareInstance: opened.descriptor.shareInstanceId,
            fileId: opened.descriptor.fileIdText,
            fileRevision: opened.descriptor.fileRevisionText,
            exactSize: opened.descriptor.exactSize,
          })
        } catch (reason) {
          // Opening happens inside the adapter call, but remains source authority.
          // Preserve that boundary so a file-local source fault is never promoted
          // into an output mutation fault merely because the adapter awaited it.
          revisionOpenFailure = Object.freeze({ reason })
          throw reason
        } finally {
          options.performancePipeline?.transition('initial_lineage')
        }
      },
    }, options.directTreeCoordinates)
    const begun = await outputOperation(
      options.signal,
      'Unable to begin the output file transaction',
      'output-write-failed',
      () => options.output.beginFile(request, options.signal),
      () => revisionOpenFailure,
    )
    if (opened === undefined) {
      throw new OutputTransactionContractError(
        'output adapter created a transaction without opening an authenticated revision',
      )
    }
    const outputFile = outputFileFor(
      options.output,
      pending,
      opened,
      options.directTreeCoordinates,
    )
    transaction = ownOutputFileTransaction(begun)
    const bound = bindOutputFileTransaction(begun, outputFile, options.output)
    transaction = bound.transaction
    const activeTransaction = bound.transaction
    options.performancePipeline?.transition('block_read_authentication')
    const wanted = new ByteRangeSet(outputFile.exactSize, [byteRange(0n, outputFile.exactSize)])
    const initialDurable = bound.initialDurable.asRangeSet()
    const missing = initialDurable.missingFrom(wanted)
    const checkpoint = {
      remainingWriteBytes: rangeBytes(missing),
      durableBytes: rangeBytes(initialDurable),
      pendingBytes: 0n,
      pendingSince: undefined as number | undefined,
      nextEvaluationPendingBytes: undefined as bigint | undefined,
      automaticCheckpointFinished: false,
    }
    let wrote = false
    for (const missingRange of missing.ranges) {
      wrote = await transferMissingRange(
        options,
        opened,
        activeTransaction,
        missingRange,
        wrote,
        checkpoint,
      )
    }
    options.performancePipeline?.transition('final_transaction')
    await outputOperation(
      options.signal,
      'Unable to commit the output file',
      'output-commit-failed',
      () => activeTransaction.commit(options.signal),
    )
    const finalDurableBytes = outputFile.exactSize - checkpoint.durableBytes
    if (finalDurableBytes > 0n) {
      options.onRecoverableAcknowledged?.(finalDurableBytes, 'final')
    }
    options.onComplete(outputFile.exactSize)
  } catch (error) {
    primaryFailure = await settleFailedFileTransfer(
      options,
      transaction,
      normalizeV2FileTransferFailure(error, {
        signal: options.signal,
        ...(options.incidentScope === undefined
          ? {}
          : { incidentScope: options.incidentScope }),
      }),
    )
  } finally {
    const releaseFailure = await releaseRevisionLease(
      acquired,
      options.outputSettlementTimeoutMilliseconds,
    )
    throwTransferOrReleaseFailure(options.incidentScope, primaryFailure, releaseFailure)
  }
}

async function settleFailedFileTransfer(
  options: V2FileTransferOptions,
  transaction: BoundOutputFileTransaction | undefined,
  failure: NormalizedV2FileTransferFailure,
): Promise<NormalizedV2FileTransferFailure> {
  if (transaction === undefined) return failure
  if (failure.kind === 'canceled') return pauseCanceledFileTransfer(options, transaction, failure)
  const authorization = authorizeFileRetirement(failure.fault)
  if (authorization === undefined) {
    if (failure.fault.scope === FaultScope.OutputPause ||
        failure.fault.scope === FaultScope.SessionTerminal) return failure
    const promoted = promoteFaultScope(failure.fault, FaultScope.OutputPause)
    return normalizedV2FileTransferFault(promoted, {
      ...(options.incidentScope === undefined
        ? {}
        : { incidentScope: options.incidentScope }),
      stage: 'settlement',
      materializationFailureReason: failure.materializationFailureReason,
    })
  }
  let disposition: Awaited<ReturnType<BoundOutputFileTransaction['retire']>>
  try {
    disposition = await withOutputSettlementTimeout(
      'retire output file transaction',
      options.outputSettlementTimeoutMilliseconds,
      () => retireOutputFileTransaction(transaction, authorization),
    )
  } catch (retirementFailure) {
    normalizeV2FileTransferFailure(retirementFailure, {
      ...(options.incidentScope === undefined
        ? {}
        : { incidentScope: options.incidentScope }),
      relation: 'consequence',
      stage: 'settlement',
    })
    return normalizedV2FileTransferFault(
      outputFault(FaultScope.OutputPause, OutputFaultCode.MutationAmbiguous),
      {
        ...(options.incidentScope === undefined
          ? {}
          : { incidentScope: options.incidentScope }),
        stage: 'settlement',
        materializationFailureReason: failure.materializationFailureReason,
      },
    )
  }
  return disposition === 'JobOutputCompromised'
    ? normalizedV2FileTransferFault(
        outputFault(FaultScope.OutputPause, OutputFaultCode.MutationAmbiguous),
        {
          ...(options.incidentScope === undefined
            ? {}
            : { incidentScope: options.incidentScope }),
          stage: 'settlement',
          materializationFailureReason: failure.materializationFailureReason,
        },
      )
    : failure
}

async function pauseCanceledFileTransfer(
  options: V2FileTransferOptions,
  transaction: BoundOutputFileTransaction,
  failure: Extract<NormalizedV2FileTransferFailure, { kind: 'canceled' }>,
): Promise<NormalizedV2FileTransferFailure> {
  try {
    await withOutputSettlementTimeout(
      'pause output file transaction',
      options.outputSettlementTimeoutMilliseconds,
      () => transaction.pause(failure.diagnostic),
    )
    return failure
  } catch (pauseFailure) {
    normalizeV2FileTransferFailure(pauseFailure, {
      ...(options.incidentScope === undefined
        ? {}
        : { incidentScope: options.incidentScope }),
      relation: 'consequence',
      stage: 'settlement',
    })
    return normalizedV2FileTransferFault(
      outputFault(FaultScope.OutputPause, OutputFaultCode.MutationAmbiguous),
      {
        ...(options.incidentScope === undefined
          ? {}
          : { incidentScope: options.incidentScope }),
        stage: 'settlement',
        materializationFailureReason: 'output-write-failed',
      },
    )
  }
}

async function releaseRevisionLease(
  acquired: V2OpenedRevision | undefined,
  timeoutMilliseconds: number,
): Promise<NormalizedV2FileTransferFailure | undefined> {
  if (acquired === undefined) return undefined
  try {
    await withOutputSettlementTimeout(
      'release revision lease',
      timeoutMilliseconds,
      () => acquired.release(),
    )
    return undefined
  } catch (error) {
    return normalizeV2FileTransferFailure(error)
  }
}

function throwTransferOrReleaseFailure(
  incidentScope: IncidentScopeHandle | undefined,
  primaryFailure: NormalizedV2FileTransferFailure | undefined,
  releaseFailure: NormalizedV2FileTransferFailure | undefined,
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
  throw normalizedV2FileTransferFault(governing, {
    ...(incidentScope === undefined ? {} : { incidentScope }),
    stage: 'settlement',
    materializationFailureReason: primaryFailure.materializationFailureReason,
  }).diagnostic
}

function classifiedFailureError(
  classification: ClassifiedTransferFailure,
): V2ClassifiedTransferFailureError {
  return new V2ClassifiedTransferFailureError(classification)
}

function outputFileFor(
  output: OutputSession,
  pending: PendingFile,
  opened: V2OpenedRevision,
  directTreeCoordinates: DirectTreeCoordinateContract | undefined,
): OutputFile {
  return snapshotOutputFile({
    source: {
      shareInstance: opened.descriptor.shareInstanceId,
      fileId: opened.descriptor.fileIdText,
      fileRevision: opened.descriptor.fileRevisionText,
    },
    sourceAuthenticationPath: pending.sourceAuthenticationPath,
    logicalArtifactPath: pending.logicalArtifactPath,
    materializationRelativePath: pending.materializationRelativePath,
    exactSize: opened.descriptor.exactSize,
    ...(pending.parent.kind === 'materialized'
      ? { parentAdmission: pending.parent.admission }
      : {}),
    ...(pending.modifiedTime === undefined || !output.capabilities.modificationTime
      ? {}
      : { modifiedTime: pending.modifiedTime }),
  }, directTreeCoordinates)
}

async function transferMissingRange(
  options: V2FileTransferOptions,
  opened: V2OpenedRevision,
  transaction: BoundOutputFileTransaction,
  requested: ByteRange,
  wrote: boolean,
  checkpoint: FileCheckpointController,
): Promise<boolean> {
  if (requested.start >= requested.end) {
    throw new V2RangeReaderContractError('requested transfer range is empty')
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
      maximumParallel: V2_BLOCK_BROKER_PARALLEL_READS,
      priority: 'download',
    },
  )) {
    options.signal.throwIfAborted()
    if (typeof slice.offset !== 'bigint' || !(slice.data instanceof Uint8Array) || slice.data.byteLength === 0) {
      throw new V2RangeReaderContractError('range reader returned an empty or malformed slice')
    }
    const end = slice.offset + BigInt(slice.data.byteLength)
    if (slice.offset !== covered || end > requested.end) {
      throw new V2RangeReaderContractError(
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
    throw new V2RangeReaderContractError('range reader returned before covering the requested interval')
  }
  return wrote
}

async function acceptAtomicOutputRange(
  options: V2FileTransferOptions,
  transaction: BoundOutputFileTransaction,
  offset: bigint,
  data: Uint8Array<ArrayBuffer>,
  wrote: boolean,
  checkpoint: FileCheckpointController,
): Promise<boolean> {
  const writtenBytes = BigInt(data.byteLength)
  options.performancePipeline?.transition('writer_lifecycle')
  try {
    await writeAtomicRange(options, transaction, offset, data)
    checkpoint.remainingWriteBytes -= writtenBytes
    checkpoint.pendingBytes += writtenBytes
    if (options.output.executionProfile.automaticCheckpoint.kind === 'bounded') {
      checkpoint.pendingSince ??= checkpointTime(options)
    }
    options.onWriteAcknowledged(writtenBytes, !wrote)
    if (checkpoint.remainingWriteBytes > 0n) {
      await attemptAutomaticCheckpoint(options, transaction, checkpoint)
    }
    return true
  } finally {
    options.performancePipeline?.transition('block_read_authentication')
  }
}

function atomicRangeAt(blockSize: bigint, offset: bigint, requestedEnd: bigint): ByteRange {
  if (blockSize <= 0n || offset < 0n || offset >= requestedEnd) {
    throw new V2RangeReaderContractError('content geometry lost an atomic output range')
  }
  const blockEnd = (offset / blockSize + 1n) * blockSize
  return byteRange(offset, blockEnd < requestedEnd ? blockEnd : requestedEnd)
}

async function writeAtomicRange(
  options: V2FileTransferOptions,
  transaction: BoundOutputFileTransaction,
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

interface FileCheckpointController {
  remainingWriteBytes: bigint
  durableBytes: bigint
  pendingBytes: bigint
  pendingSince: number | undefined
  nextEvaluationPendingBytes: bigint | undefined
  automaticCheckpointFinished: boolean
}

async function attemptAutomaticCheckpoint(
  options: V2FileTransferOptions,
  transaction: BoundOutputFileTransaction,
  checkpoint: FileCheckpointController,
): Promise<void> {
  const policy = options.output.executionProfile.automaticCheckpoint
  if (policy.kind === 'disabled' || checkpoint.automaticCheckpointFinished) return
  const trigger = automaticCheckpointTrigger(options, policy, checkpoint)
  if (trigger === undefined) return
  const scheduleDecision = createCheckpointSchedule(policy.trigger.pendingBytes).evaluate({
    durablePrefixBytes: checkpoint.durableBytes,
    pendingBytes: checkpoint.pendingBytes,
    remainingBytes: checkpoint.remainingWriteBytes,
  })
  if (scheduleDecision.kind === 'wait-for-progress') {
    checkpoint.nextEvaluationPendingBytes = scheduleDecision.nextPendingBytes
    return
  }
  if (scheduleDecision.kind === 'finish-without-further-checkpoint') {
    checkpoint.automaticCheckpointFinished = true
    return
  }
  const result = await outputOperation(
    options.signal,
    'Unable to checkpoint the output file',
    'output-write-failed',
    () => transaction.automaticCheckpoint(trigger, options.signal),
  )
  if (result.kind === 'deferred') {
    checkpoint.nextEvaluationPendingBytes = nextCheckpointRetryPendingBytes(
      checkpoint.durableBytes,
      checkpoint.pendingBytes,
      policy.trigger.pendingBytes,
    )
    return
  }
  if (result.kind === 'finished') {
    checkpoint.automaticCheckpointFinished = true
    return
  }
  const durableBytes = rangeBytes(result.durable.asRangeSet())
  const advancedBytes = durableBytes - checkpoint.durableBytes
  if (advancedBytes <= 0n) {
    throw new OutputTransactionContractError('advanced checkpoint did not increase durable coverage')
  }
  checkpoint.durableBytes = durableBytes
  checkpoint.pendingBytes = 0n
  checkpoint.pendingSince = undefined
  checkpoint.nextEvaluationPendingBytes = nextCheckpointRetryPendingBytes(
    durableBytes,
    0n,
    policy.trigger.pendingBytes,
  )
  options.onRecoverableAcknowledged?.(advancedBytes, 'automatic-checkpoint')
}

function automaticCheckpointTrigger(
  options: V2FileTransferOptions,
  policy: OutputExecutionProfileBoundedCheckpoint,
  checkpoint: FileCheckpointController,
): AutomaticCheckpointTrigger | undefined {
  const nextPendingBytes = checkpoint.nextEvaluationPendingBytes ?? policy.trigger.pendingBytes
  if (checkpoint.pendingBytes >= nextPendingBytes) return 'pending-bytes'
  if (checkpoint.nextEvaluationPendingBytes !== undefined) return undefined
  const pendingSince = checkpoint.pendingSince
  if (pendingSince === undefined) return undefined
  const elapsed = checkpointTime(options) - pendingSince
  if (elapsed < 0) {
    throw new OutputTransactionContractError('output checkpoint clock moved backwards')
  }
  return elapsed >= policy.trigger.pendingMilliseconds ? 'pending-time' : undefined
}

function checkpointTime(options: V2FileTransferOptions): number {
  const current = (options.now ?? Date.now)()
  if (!Number.isFinite(current)) {
    throw new OutputTransactionContractError('output checkpoint clock returned a non-finite time')
  }
  return current
}

function rangeBytes(ranges: ByteRangeSet): bigint {
  return ranges.ranges.reduce((total, range) => total + range.end - range.start, 0n)
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
        cause instanceof V2OutputPausedError ||
        signal.aborted) throw cause
    throw new V2FileOutputError(message, reason, cause)
  }
}
