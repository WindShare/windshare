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
import {
  OutputTransactionContractError,
  retireOutputFileTransaction,
  bindOutputFileTransaction,
  ownOutputFileTransaction,
} from '../output-file-transaction'
import {
  OutputBudgetExceededError,
  OutputSessionCompromisedError,
  TransferPauseRequestedError,
  snapshotOpenedOutputRevision,
  snapshotOutputFile,
  snapshotOutputFileRequest,
  type OutputFile,
  type OutputSession,
} from '../output-session'
import { V2OutputPausedError, type PendingFile } from './contract'
import {
  V2FileOutputError,
  normalizeV2FileTransferFailure,
  normalizedV2FileTransferFault,
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
import { withOutputSettlementTimeout } from '../settlement/v2-output'

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
  readonly signal: AbortSignal
  readonly outputSettlementTimeoutMilliseconds: number
  readonly onWriteAcknowledged: (bytes: bigint, firstWrite: boolean) => void
  readonly onComplete: (exactSize: bigint) => void
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
      sourcePath: pending.sourcePath,
      artifactPath: pending.artifactPath,
      expectedSize: pending.entry.expectedSize,
      ...(pending.parent.admission === undefined
        ? {}
        : { parentAdmission: pending.parent.admission }),
      ...(pending.modifiedTime === undefined || !options.output.capabilities.modificationTime
        ? {}
        : { modifiedTime: pending.modifiedTime }),
      openRevision: async (signal) => {
        if (revisionOpenAttempted) {
          throw new OutputTransactionContractError(
            'output adapter invoked the authenticated revision callback more than once',
          )
        }
        revisionOpenAttempted = true
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
        }
      },
    })
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
    const outputFile = outputFileFor(options.output, pending, opened)
    transaction = ownOutputFileTransaction(begun)
    const bound = bindOutputFileTransaction(begun, outputFile, options.output)
    transaction = bound.transaction
    const wanted = new ByteRangeSet(outputFile.exactSize, [byteRange(0n, outputFile.exactSize)])
    const missing = bound.durableRanges.asRangeSet().missingFrom(wanted)
    let wrote = false
    for (const missingRange of missing.ranges) {
      wrote = await transferMissingRange(
        options,
        opened,
        bound.transaction,
        missingRange,
        wrote,
      )
    }
    await outputOperation(
      options.signal,
      'Unable to commit the output file',
      'output-commit-failed',
      () => transaction?.commit(options.signal) ?? Promise.resolve(),
    )
    options.onComplete(outputFile.exactSize)
  } catch (error) {
    primaryFailure = await settleFailedFileTransfer(
      options,
      transaction,
      normalizeV2FileTransferFailure(error, options.signal),
    )
  } finally {
    const releaseFailure = await releaseRevisionLease(acquired, options.outputSettlementTimeoutMilliseconds)
    throwTransferOrReleaseFailure(primaryFailure, releaseFailure)
  }
}

async function settleFailedFileTransfer(
  options: V2FileTransferOptions,
  transaction: BoundOutputFileTransaction | undefined,
  failure: NormalizedV2FileTransferFailure,
): Promise<NormalizedV2FileTransferFailure> {
  if (transaction === undefined || failure.kind === 'canceled') return failure
  const authorization = authorizeFileRetirement(failure.fault)
  if (authorization === undefined) {
    if (failure.fault.scope === FaultScope.OutputPause ||
        failure.fault.scope === FaultScope.SessionTerminal) return failure
    const promoted = promoteFaultScope(failure.fault, FaultScope.OutputPause)
    return normalizedV2FileTransferFault(
      promoted,
      failure.diagnostic,
      'Unretired file state requires a stable output pause',
    )
  }
  let disposition: Awaited<ReturnType<BoundOutputFileTransaction['retire']>>
  try {
    disposition = await withOutputSettlementTimeout(
      'retire output file transaction',
      options.outputSettlementTimeoutMilliseconds,
      () => retireOutputFileTransaction(transaction, authorization),
    )
  } catch (retirementFailure) {
    return normalizedV2FileTransferFault(
      outputFault(FaultScope.OutputPause, OutputFaultCode.MutationAmbiguous),
      new AggregateError(
        [failure.diagnostic, retirementFailure],
        'Transfer and authorized output retirement failed',
      ),
      'Output transaction could not establish retirement isolation',
    )
  }
  return disposition === 'JobOutputCompromised'
    ? normalizedV2FileTransferFault(
        outputFault(FaultScope.OutputPause, OutputFaultCode.MutationAmbiguous),
        failure.diagnostic,
        'Output backend cannot isolate an authorized file retirement',
      )
    : failure
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
  primaryFailure: NormalizedV2FileTransferFailure | undefined,
  releaseFailure: NormalizedV2FileTransferFailure | undefined,
): void {
  if (releaseFailure === undefined) {
    if (primaryFailure !== undefined) throw primaryFailure.diagnostic
    return
  }
  if (primaryFailure === undefined) throw releaseFailure.diagnostic
  // Cancellation controls the run outside the fault lattice. A concurrent
  // settlement fault must not turn that exact control decision into fault policy.
  if (primaryFailure.kind === 'canceled') throw primaryFailure.diagnostic
  if (releaseFailure.kind === 'canceled') throw releaseFailure.diagnostic
  const governing = joinFaults(primaryFailure.fault, releaseFailure.fault)
  if (governing === undefined) throw primaryFailure.diagnostic
  throw normalizedV2FileTransferFault(
    governing,
    new AggregateError(
      [primaryFailure.diagnostic, releaseFailure.diagnostic],
      'File transfer and revision lease settlement failed',
    ),
    'Revision lease could not establish bounded terminal settlement',
  ).diagnostic
}

function outputFileFor(
  output: OutputSession,
  pending: PendingFile,
  opened: V2OpenedRevision,
): OutputFile {
  return snapshotOutputFile({
    source: {
      shareInstance: opened.descriptor.shareInstanceId,
      fileId: opened.descriptor.fileIdText,
      fileRevision: opened.descriptor.fileRevisionText,
    },
    sourcePath: pending.sourcePath,
    artifactPath: pending.artifactPath,
    exactSize: opened.descriptor.exactSize,
    ...(pending.parent.admission === undefined
      ? {}
      : { parentAdmission: pending.parent.admission }),
    ...(pending.modifiedTime === undefined || !output.capabilities.modificationTime
      ? {}
      : { modifiedTime: pending.modifiedTime }),
  })
}

async function transferMissingRange(
  options: V2FileTransferOptions,
  opened: V2OpenedRevision,
  transaction: BoundOutputFileTransaction,
  requested: ByteRange,
  wrote: boolean,
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
      await writeAtomicRange(options, transaction, atomic.start, data)
      options.onWriteAcknowledged(BigInt(data.byteLength), !wrote)
      wrote = true
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
  await outputOperation(
    options.signal,
    'Unable to checkpoint the output file',
    'output-write-failed',
    () => transaction.checkpoint(options.signal),
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
        cause instanceof OutputBudgetExceededError ||
        cause instanceof OutputSessionCompromisedError ||
        cause instanceof V2OutputPausedError ||
        signal.aborted) throw cause
    throw new V2FileOutputError(message, reason, { cause })
  }
}
