import { BLOCK_BROKER_PARALLEL_READS, type BlockRangeReader } from '../content/broker'
import { ByteRangeSet, byteRange, coveredBytes } from '../content/geometry'
import type { FileRevisionDescriptor } from '../content/records'
import { compareRevisionDescriptors, compareRevisionMetadata } from '../content/revision-comparison'
import {
  RemoteRevisionError,
  RevisionCapacityBusyError,
  RevisionReopenedDuringRecoveryError,
  requiresRevisionLeaseReplacement,
  type OpenedRevision,
  type RevisionReader,
} from '../content/session-services'
import { RemoteOperationError } from '../content/session-operations'
import { encodeBase64Url } from '../crypto/bytes'
import { ActiveFileWrite } from './active-file-write'
import type { DiscoveredFile } from './discovery'
import {
  FaultScope,
  SourceFaultCode,
  TransferFaultError,
  TransferPauseError,
  TransferStopError,
  isRecoverableTransferInterruption,
  sourceFault,
  storagePauseReason,
} from './fault'
import type { CompletedOutputFile, TransferOutputFile, TransferOutputSession } from './output-port'
import type { ReceivePauseReason } from './state'

const MAX_CAPACITY_WAIT_MS = 60_000
const MAX_LEASE_RETRIES = 3
// Allow one reopen readmission without newly written bytes (e.g. a reconnect
// before the first block arrives), then stop if a second consecutive reopen
// still makes zero progress.
const MAX_STALLED_REOPEN_RETRIES = 1

export interface FileTransferContext {
  readonly revisions: RevisionReader
  readonly broker: BlockRangeReader
  readonly output: TransferOutputSession
  readonly signal: AbortSignal
  readonly isDetached: () => boolean
  readonly trackWrite?: (file: ActiveFileWrite | undefined) => void
  readonly onUncheckpointedWrite?: () => void
  readonly onOutputFailure?: (error: unknown) => void
  readonly isPaused: () => boolean
  readonly isStopped: () => boolean
  readonly onInitialCoverage?: (bytes: bigint) => void
  readonly onObjectBytes?: (bytes: number) => void
  readonly onWritten?: (bytes: bigint) => void
  readonly onCapacityWait?: (deltaWaiting: number, addedWaitMs: number, addedAttempt: number) => void
  readonly remainingWork?: () => { readonly bytes: number; readonly known: boolean }
}

export type SingleFileTransferOutcome =
  | Readonly<{ kind: 'completed'; size: bigint }>
  | Readonly<{ kind: 'paused'; reason: ReceivePauseReason }>
  | Readonly<{ kind: 'stopped' | 'detached' }>
  | Readonly<{ kind: 'source-changed' }>
  | Readonly<{ kind: 'source-unavailable'; error: unknown }>
  | Readonly<{ kind: 'failed'; error: unknown }>

interface FileTransferAttemptState {
  readonly attemptIndex: number
  readonly leaseRetries: number
  readonly stalledReopens: number
  readonly reopenedFrom?: FileRevisionDescriptor
}

type AttemptResult =
  | SingleFileTransferOutcome
  | Readonly<{ kind: 'retry'; next: FileTransferAttemptState }>

/**
 * Downloads missing byte ranges of one file in parallel, flushing physical writes
 * before committing checkpoints so resumes never rely on unverified metadata.
 */
export async function transferSingleFile(
  ctx: FileTransferContext,
  file: DiscoveredFile,
): Promise<SingleFileTransferOutcome> {
  let state: FileTransferAttemptState = {
    attemptIndex: 0,
    leaseRetries: 0,
    stalledReopens: 0,
  }
  for (;;) {
    const preflight = preflightOutcome(ctx)
    if (preflight !== undefined) return preflight
    if (ctx.signal.aborted) return { kind: 'failed', error: ctx.signal.reason }

    const attempt = await attemptSingleFileTransfer(ctx, file, state)
    if (attempt.kind === 'retry') {
      ctx.trackWrite?.(undefined)
      state = attempt.next
      continue
    }
    return attempt
  }
}

function preflightOutcome(ctx: FileTransferContext): SingleFileTransferOutcome | undefined {
  if (ctx.isDetached()) return { kind: 'detached' }
  if (ctx.isPaused()) return { kind: 'paused', reason: 'user' }
  if (ctx.isStopped()) return { kind: 'stopped' }
  return undefined
}

async function attemptSingleFileTransfer(
  ctx: FileTransferContext,
  file: DiscoveredFile,
  state: FileTransferAttemptState,
): Promise<AttemptResult> {
  let opened: OpenedRevision | undefined
  let writer: TransferOutputFile | undefined
  let initialBytes = 0n
  let active: ActiveFileWrite | undefined
  let release: Promise<void> | undefined
  try {
    const completed = await checkAlreadyCompleted(ctx, file, state.attemptIndex)
    if (completed !== undefined) {
      ctx.onInitialCoverage?.(completed.size)
      return { kind: 'completed', size: completed.size }
    }

    opened = await openRevisionWithBackoff(ctx, file.entry.id)
    const readmissionFailure = verifyReadmittedRevision(state.reopenedFrom, opened.descriptor)
    if (readmissionFailure !== undefined) return readmissionFailure

    const desc = opened.descriptor
    const revisionText = encodeBase64Url(desc.fileRevision)
    writer = await openOutputFile(ctx, file, opened, revisionText)

    if (hasObservedRevisionChange(writer, opened, revisionText)) {
      await abortChangedWriter(writer)
      return { kind: 'source-changed' }
    }

    active = new ActiveFileWrite(writer, desc.exactSize, ctx.signal, ctx.output.capabilities.periodicCheckpoint)
    ctx.trackWrite?.(active)
    initialBytes = active.coverage
    if (state.attemptIndex === 0 && initialBytes > 0n) ctx.onInitialCoverage?.(initialBytes)

    await streamMissingRanges(ctx, opened, active)
    release = releaseLease(opened)
    ctx.onUncheckpointedWrite?.()
    await active.commit()
    return { kind: 'completed', size: desc.exactSize }
  } catch (error) {
    const finish = () => handleFileError(ctx, writer, active?.ranges, initialBytes, active?.failure ?? error, state)
    return await (active === undefined ? finish() : active.finish(finish))
  } finally {
    await (release ?? releaseLease(opened))
  }
}

async function releaseLease(opened: OpenedRevision | undefined): Promise<void> {
  try { await opened?.release() } catch { /* Received bytes can be committed independently of the sender. */ }
}

function checkAlreadyCompleted(
  ctx: FileTransferContext,
  file: DiscoveredFile,
  attemptIndex: number,
): Promise<CompletedOutputFile | undefined> {
  // Only check prior completion on the initial attempt; retry attempts are already streaming.
  if (attemptIndex > 0 || ctx.output.checkCompleted === undefined) {
    return Promise.resolve(undefined)
  }
  return ctx.output.checkCompleted(file.entry.idText, ctx.signal)
}

function verifyReadmittedRevision(
  previous: FileRevisionDescriptor | undefined,
  current: FileRevisionDescriptor,
): SingleFileTransferOutcome | undefined {
  if (previous === undefined) return undefined
  const comparison = compareRevisionDescriptors(previous, current)
  if (comparison.kind === 'metadata-agreement') return undefined
  if (comparison.kind === 'observed-change') return { kind: 'source-changed' }
  return { kind: 'failed', error: new RevisionReopenedDuringRecoveryError(previous, current) }
}

function openOutputFile(
  ctx: FileTransferContext,
  file: DiscoveredFile,
  opened: OpenedRevision,
  revisionText: string,
): Promise<TransferOutputFile> {
  const desc = opened.descriptor
  const modifiedTimeMs = desc.modifiedTime === undefined
    ? undefined
    : desc.modifiedTime.seconds * 1_000n + BigInt(Math.floor(desc.modifiedTime.nanoseconds / 1_000_000))
  return ctx.output.openFile({
    fileId: file.entry.idText,
    path: file.relativePath,
    sourcePath: file.sourcePath,
    revision: revisionText,
    ...(desc.sourceMetadata === undefined ? {} : { sourceMetadata: desc.sourceMetadata }),
    exactSize: desc.exactSize,
    ...(modifiedTimeMs === undefined ? {} : { modifiedTimeMilliseconds: modifiedTimeMs }),
  }, ctx.signal)
}

function hasObservedRevisionChange(
  writer: TransferOutputFile,
  opened: OpenedRevision,
  revisionText: string,
): boolean {
  if (writer.savedRevision === undefined || writer.completedRanges.length === 0) return false
  const desc = opened.descriptor
  const comparison = compareRevisionMetadata(
    {
      fileRevision: writer.savedRevision,
      exactSize: writer.savedSize ?? desc.exactSize,
      ...(writer.savedSourceMetadata === undefined ? {} : { sourceMetadata: writer.savedSourceMetadata }),
    },
    {
      fileRevision: revisionText,
      exactSize: desc.exactSize,
      ...(desc.sourceMetadata === undefined ? {} : { sourceMetadata: desc.sourceMetadata }),
    },
  )
  return comparison.kind !== 'metadata-agreement'
}

async function abortChangedWriter(writer: TransferOutputFile): Promise<void> {
  await writer.abort(new TransferFaultError(
    sourceFault(FaultScope.FileLocal, SourceFaultCode.RevisionChanged),
    'Source file changed',
  )).catch(() => undefined)
}

async function streamMissingRanges(
  ctx: FileTransferContext,
  opened: OpenedRevision,
  active: ActiveFileWrite,
): Promise<void> {
  const size = opened.descriptor.exactSize
  const missing = active.ranges.missingFrom(new ByteRangeSet(size, [byteRange(0n, size)]))
  for (const span of missing.ranges) {
    active.signal.throwIfAborted()
    const stream = ctx.broker.readRange(opened.descriptor, opened.leaseId, span, {
      signal: active.signal,
      maximumParallel: BLOCK_BROKER_PARALLEL_READS,
      ...(ctx.onObjectBytes === undefined ? {} : { onReceive: ctx.onObjectBytes }),
      ...(ctx.remainingWork === undefined ? {} : { remainingWork: ctx.remainingWork }),
    })
    for await (const slice of stream) {
      active.signal.throwIfAborted()
      if (slice.data.byteLength === 0) continue
      ctx.onUncheckpointedWrite?.()
      await active.write(slice.offset, slice.data, ctx.onWritten)
    }
  }
}

async function openRevisionWithBackoff(
  ctx: FileTransferContext,
  fileId: Uint8Array,
): Promise<OpenedRevision> {
  let waitedMs = 0
  for (;;) {
    ctx.signal.throwIfAborted()
    try {
      return await ctx.revisions.open(fileId, ctx.signal)
    } catch (error) {
      if (!(error instanceof RevisionCapacityBusyError)) throw error
      const delayMs = Math.min(Math.max(100, error.retryAfterMilliseconds), MAX_CAPACITY_WAIT_MS - waitedMs)
      if (delayMs <= 0) throw error
      ctx.onCapacityWait?.(1, delayMs, 1)
      try {
        await sleepAbortable(delayMs, ctx.signal)
      } finally {
        ctx.onCapacityWait?.(-1, 0, 0)
      }
      waitedMs += delayMs
    }
  }
}

async function handleFileError(
  ctx: FileTransferContext,
  writer: TransferOutputFile | undefined,
  ranges: ByteRangeSet | undefined,
  initialBytes: bigint,
  error: unknown,
  state: FileTransferAttemptState,
): Promise<AttemptResult> {
  if (ctx.isDetached()) {
    await writer?.abort(error).catch(() => undefined)
    return { kind: 'detached' }
  }
  ctx.onOutputFailure?.(error)
  const pauseReason = pauseReasonFor(ctx, error)
  if (pauseReason !== undefined) {
    return pauseWriter(ctx, writer, ranges, error, pauseReason)
  }
  if (error instanceof TransferStopError || ctx.isStopped()) {
    await writer?.abort(error).catch(() => undefined)
    return { kind: 'stopped' }
  }
  if (isSourceChangedError(error)) {
    await writer?.abort(error).catch(() => undefined)
    return { kind: 'source-changed' }
  }
  const nextRetry = nextRetryStateFor(ctx, ranges, initialBytes, error, state)
  if (nextRetry !== undefined) {
    const checkpointOutcome = await checkpointForRetry(ctx, writer, ranges)
    return checkpointOutcome ?? { kind: 'retry', next: nextRetry }
  }
  if (ctx.output.capabilities.resumable && !ctx.signal.aborted && isRecoverableTransferInterruption(error)) {
    return pauseWriter(ctx, writer, ranges, error, 'interrupted')
  }
  await writer?.abort(error).catch(() => undefined)
  if (ctx.signal.aborted) return preflightOutcome(ctx) ?? { kind: 'failed', error: ctx.signal.reason }
  return error instanceof RemoteRevisionError
    ? { kind: 'source-unavailable', error }
    : { kind: 'failed', error }
}

function nextRetryStateFor(
  ctx: FileTransferContext,
  ranges: ByteRangeSet | undefined,
  initialBytes: bigint,
  error: unknown,
  state: FileTransferAttemptState,
): FileTransferAttemptState | undefined {
  if (!ctx.output.capabilities.resumable || ctx.signal.aborted) return undefined
  const madeProgress = ranges !== undefined && coveredBytes(ranges.ranges) > initialBytes
  if (isReusableReopenedRevisionError(error)) {
    const stalledReopens = madeProgress ? 0 : state.stalledReopens + 1
    if (stalledReopens > MAX_STALLED_REOPEN_RETRIES) return undefined
    return {
      attemptIndex: state.attemptIndex + 1,
      leaseRetries: madeProgress ? 0 : state.leaseRetries,
      stalledReopens,
      reopenedFrom: error.previous,
    }
  }
  if (requiresRevisionLeaseReplacement(error) && state.leaseRetries < MAX_LEASE_RETRIES) {
    return {
      attemptIndex: state.attemptIndex + 1,
      leaseRetries: state.leaseRetries + 1,
      stalledReopens: madeProgress ? 0 : state.stalledReopens,
      ...(state.reopenedFrom === undefined ? {} : { reopenedFrom: state.reopenedFrom }),
    }
  }
  return undefined
}

function isReusableReopenedRevisionError(error: unknown): error is RevisionReopenedDuringRecoveryError {
  return error instanceof RevisionReopenedDuringRecoveryError &&
    error.comparison.kind === 'metadata-agreement'
}

function pauseReasonFor(
  ctx: FileTransferContext,
  error: unknown,
): ReceivePauseReason | undefined {
  if (error instanceof TransferPauseError) return error.reason
  return storagePauseReason(error) ?? (ctx.isPaused() ? 'user' : undefined)
}

async function pauseWriter(
  ctx: FileTransferContext,
  writer: TransferOutputFile | undefined,
  ranges: ByteRangeSet | undefined,
  reason: unknown,
  pauseReason: ReceivePauseReason,
): Promise<SingleFileTransferOutcome> {
  if (writer !== undefined) {
    if (ranges === undefined || ranges.empty) {
      await writer.abort(reason).catch(() => undefined)
    } else {
      try {
        await writer.pause(ranges.ranges)
      } catch (cause) {
        await writer.abort(cause).catch(() => undefined)
        ctx.onOutputFailure?.(cause)
        const storageReason = storagePauseReason(cause)
        return storageReason === undefined ? { kind: 'failed', error: cause } : { kind: 'paused', reason: storageReason }
      }
    }
  }
  return { kind: 'paused', reason: pauseReason }
}

async function checkpointForRetry(
  ctx: FileTransferContext,
  writer: TransferOutputFile | undefined,
  ranges: ByteRangeSet | undefined,
): Promise<SingleFileTransferOutcome | undefined> {
  if (writer === undefined) return undefined
  if (ranges === undefined || ranges.empty) {
    // No bytes need preservation; aborting clears any empty FSA placeholder
    // before the retry attempt opens a fresh revision.
    await writer.abort().catch(() => undefined)
    return undefined
  }
  try {
    const outcome = await writer.checkpoint(ranges.ranges, 'retry')
    if (outcome !== 'saved') throw new Error('Cannot reopen without a durable file checkpoint')
    return undefined
  } catch (checkpointError) {
    await writer.abort(checkpointError).catch(() => undefined)
    ctx.onOutputFailure?.(checkpointError)
    const pauseReason = pauseReasonFor(ctx, checkpointError)
    if (pauseReason !== undefined) {
      return { kind: 'paused', reason: pauseReason }
    }
    if (ctx.signal.aborted) return preflightOutcome(ctx) ?? { kind: 'failed', error: ctx.signal.reason }
    return { kind: 'failed', error: checkpointError }
  }
}

const REVISION_CODE_STALE = 0x3001
const REVISION_CODE_NOT_FOUND = 0x3002
const REVISION_CODE_DRIFT = 0x3007

function isSourceChangedRevisionCode(code: number): boolean {
  return code === REVISION_CODE_STALE || code === REVISION_CODE_NOT_FOUND || code === REVISION_CODE_DRIFT
}

function isSourceChangedError(error: unknown): boolean {
  if (error instanceof RevisionReopenedDuringRecoveryError) {
    return error.comparison.kind === 'observed-change'
  }
  if (error instanceof TransferFaultError) {
    return error.fault.code === SourceFaultCode.RevisionChanged ||
      error.fault.code === SourceFaultCode.RevisionInvalidated
  }
  if (error instanceof RemoteRevisionError) {
    return !error.failure.retryable && isSourceChangedRevisionCode(error.failure.code)
  }
  if (error instanceof RemoteOperationError) {
    return error.scope === 'revision' && !error.retryable && isSourceChangedRevisionCode(error.code)
  }
  return false
}

function sleepAbortable(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) { reject(signal.reason); return }
    const onAbort = () => { clearTimeout(timer); reject(signal.reason) }
    const timer = setTimeout(() => { signal.removeEventListener('abort', onAbort); resolve() }, ms)
    signal.addEventListener('abort', onAbort, { once: true })
  })
}
