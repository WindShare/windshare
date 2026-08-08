import { V2_MAXIMUM_CHUNK_BYTES, type V2ShareDescriptor } from '../../catalog/v2-records'
import { ByteRangeSet, bigintToSafeNumber, byteRange, type ByteRange } from '../../content/geometry'
import type { V2BlockRangeReader } from '../../content/v2-broker'
import type {
  V2OpenedRevision,
  V2RevisionReader,
} from '../../content/v2-session-services'
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
  snapshotOutputFile,
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
        cause instanceof BoundaryFaultError ||
        cause instanceof TransferPauseRequestedError ||
        cause instanceof OutputBudgetExceededError ||
        cause instanceof OutputSessionCompromisedError ||
        cause instanceof V2OutputPausedError ||
        signal.aborted) throw cause
    throw new V2FileOutputError(message, { cause })
  }
}
