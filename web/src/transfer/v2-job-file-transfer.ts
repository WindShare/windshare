import { V2_MAXIMUM_CHUNK_BYTES, type V2ShareDescriptor } from '../catalog/v2-records'
import { ByteRangeSet, bigintToSafeNumber, byteRange, type ByteRange } from '../content/geometry'
import type { V2BlockRangeReader } from '../content/v2-broker'
import type {
  V2OpenedRevision,
  V2RevisionReader,
} from '../content/v2-session-services'
import {
  OutputTransactionContractError,
  abortOutputFileTransaction,
  bindOutputFileTransaction,
  ownOutputFileTransaction,
} from './output-file-transaction'
import {
  OutputBudgetExceededError,
  OutputSessionCompromisedError,
  OutputSessionSuspendedError,
  snapshotOutputFile,
  type OutputFile,
  type OutputSession,
} from './output-session'
import { V2OutputPausedError, type PendingFile } from './v2-job-contract'
import {
  V2FileLeaseSettlementError,
  V2FileOutputError,
  isV2FileScopedTransferFailure,
} from './v2-job-failures'
import { validateOpenedFileRevision } from './v2-job-file-authority'
import { withOutputSettlementTimeout } from './settlement/v2-output'

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
  let primaryFailure: unknown
  try {
    acquired = await options.revisions.open(pending.entry.id, options.signal)
    opened = validateOpenedFileRevision(
      options.descriptor,
      pending.entry,
      acquired,
    )
    const outputFile = outputFileFor(options.output, pending, opened)
    const begun = await outputOperation(
      options.signal,
      'Unable to begin the output file transaction',
      () => options.output.beginFile(outputFile, options.signal),
    )
    transaction = ownOutputFileTransaction(begun)
    const bound = bindOutputFileTransaction(begun, outputFile, options.output)
    transaction = bound.transaction
    const wanted = new ByteRangeSet(outputFile.exactSize, [byteRange(0n, outputFile.exactSize)])
    const missing = bound.durableRanges.asRangeSet().missingFrom(wanted)
    let wrote = false
    for (const missingRange of missing.ranges) {
      const plan = opened.descriptor.geometry.plan(missingRange)
      for (let index = plan.blocks.first; index < plan.blocks.end; index += 1n) {
        const requested = plan.sliceForBlock(index)?.requestedBytes
        if (requested === undefined) {
          throw new V2RangeReaderContractError('content geometry lost a requested block range')
        }
        const data = await readAtomicRange(options, opened, requested)
        await outputOperation(
          options.signal,
          'Unable to write the output file range',
          () => transaction?.writeRange(requested.start, data, options.signal) ?? Promise.resolve(),
        )
        await outputOperation(
          options.signal,
          'Unable to checkpoint the output file',
          () => transaction?.checkpoint(options.signal) ?? Promise.resolve(undefined),
        )
        options.onWriteAcknowledged(BigInt(data.byteLength), !wrote)
        wrote = true
      }
    }
    await outputOperation(
      options.signal,
      'Unable to commit the output file',
      () => transaction?.commit(options.signal) ?? Promise.resolve(),
    )
    options.onComplete(outputFile.exactSize)
  } catch (error) {
    primaryFailure = await settleFailedFileTransfer(options, transaction, error)
  } finally {
    const releaseFailure = await releaseRevisionLease(acquired, options.outputSettlementTimeoutMilliseconds)
    throwTransferOrReleaseFailure(primaryFailure, releaseFailure)
  }
}

async function settleFailedFileTransfer(
  options: V2FileTransferOptions,
  transaction: BoundOutputFileTransaction | undefined,
  error: unknown,
): Promise<unknown> {
  const isolateFile = !options.signal.aborted && isV2FileScopedTransferFailure(error)
  if (transaction === undefined || (!isolateFile && !(error instanceof OutputTransactionContractError))) {
    return error
  }
  let disposition: Awaited<ReturnType<BoundOutputFileTransaction['abort']>>
  try {
    disposition = await withOutputSettlementTimeout(
      'abort output file transaction',
      options.outputSettlementTimeoutMilliseconds,
      () => abortOutputFileTransaction(transaction, error),
    )
  } catch (abortFailure) {
    const cause = new AggregateError([error, abortFailure], 'Transfer and output transaction abort failed')
    if (abortFailure instanceof OutputTransactionContractError) {
      return new OutputTransactionContractError('Output transaction abort contract failed', { cause })
    }
    return new V2OutputPausedError('Output transaction could not establish abort isolation', { cause })
  }
  return disposition === 'JobOutputCompromised'
    ? new V2OutputPausedError('Output backend cannot isolate a file failure', { cause: error })
    : error
}

async function releaseRevisionLease(
  acquired: V2OpenedRevision | undefined,
  timeoutMilliseconds: number,
): Promise<unknown> {
  if (acquired === undefined) return undefined
  try {
    await withOutputSettlementTimeout(
      'release revision lease',
      timeoutMilliseconds,
      () => acquired.release(),
    )
    return undefined
  } catch (error) {
    return error
  }
}

function throwTransferOrReleaseFailure(primaryFailure: unknown, releaseFailure: unknown): void {
  if (releaseFailure === undefined) {
    if (primaryFailure !== undefined) throw primaryFailure
    return
  }
  if ((primaryFailure === undefined || isV2FileScopedTransferFailure(primaryFailure)) &&
      isV2FileScopedTransferFailure(releaseFailure)) {
    throw new V2FileLeaseSettlementError(primaryFailure, releaseFailure)
  }
  const cause = primaryFailure === undefined
    ? releaseFailure
    : new AggregateError([primaryFailure, releaseFailure], 'File transfer and revision lease settlement failed')
  throw new V2OutputPausedError('Revision lease could not establish bounded terminal settlement', { cause })
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
    path: pending.path,
    exactSize: opened.descriptor.exactSize,
    parentAdmission: pending.parentAdmission,
    ...(pending.modifiedTime === undefined || !output.capabilities.modificationTime
      ? {}
      : { modifiedTime: pending.modifiedTime }),
  })
}

async function readAtomicRange(
  options: V2FileTransferOptions,
  opened: V2OpenedRevision,
  requested: ByteRange,
): Promise<Uint8Array<ArrayBuffer>> {
  const length = requested.end - requested.start
  if (length <= 0n || length > BigInt(V2_MAXIMUM_CHUNK_BYTES)) {
    throw new V2RangeReaderContractError('requested atomic range exceeds the protocol chunk bound')
  }
  const data = new Uint8Array(bigintToSafeNumber(length, 'requested atomic range'))
  let covered = requested.start
  for await (const slice of options.broker.readRange(
    opened.descriptor,
    opened.leaseId,
    requested,
    { signal: options.signal, priority: 'download' },
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
    data.set(slice.data, bigintToSafeNumber(slice.offset - requested.start, 'range slice offset'))
    covered = end
  }
  options.signal.throwIfAborted()
  if (covered !== requested.end) {
    throw new V2RangeReaderContractError('range reader returned before covering the requested interval')
  }
  return data
}

async function outputOperation<T>(
  signal: AbortSignal,
  message: string,
  operation: () => Promise<T>,
): Promise<T> {
  try {
    return await operation()
  } catch (cause) {
    if (cause instanceof OutputTransactionContractError ||
        cause instanceof OutputSessionSuspendedError ||
        cause instanceof OutputBudgetExceededError ||
        cause instanceof OutputSessionCompromisedError ||
        cause instanceof V2OutputPausedError ||
        signal.aborted) throw cause
    throw new V2FileOutputError(message, { cause })
  }
}
