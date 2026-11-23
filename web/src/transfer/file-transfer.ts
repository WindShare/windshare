import { BLOCK_BROKER_PARALLEL_READS, type BlockRangeReader } from '../content/broker'
import { ByteRangeSet, byteRange, type ByteRange } from '../content/geometry'
import { compareRevisionMetadata } from '../content/revision-comparison'
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
import type { DiscoveredFile } from './discovery'
import {
  FaultScope,
  SourceFaultCode,
  TransferFaultError,
  TransferPauseError,
  TransferStopError,
  isAbortError,
  isPermissionDeniedError,
  isQuotaExceededError,
  sourceFault,
} from './fault'
import type { TransferOutputFile, TransferOutputSession } from './output-port'

const MAX_CAPACITY_WAIT_MS = 60_000
const MAX_LEASE_RETRIES = 3

export interface FileTransferContext {
  readonly revisions: RevisionReader
  readonly broker: BlockRangeReader
  readonly output: TransferOutputSession
  readonly signal: AbortSignal
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
  | Readonly<{ kind: 'paused'; reason: 'user' | 'storage-full' | 'permission-needed' }>
  | Readonly<{ kind: 'stopped' }>
  | Readonly<{ kind: 'source-changed' }>
  | Readonly<{ kind: 'source-unavailable'; error: unknown }>
  | Readonly<{ kind: 'failed'; error: unknown }>

/**
 * Downloads missing byte ranges of one file in parallel, flushing physical writes
 * before committing checkpoints so resumes never rely on unverified metadata.
 */
export async function transferSingleFile(
  ctx: FileTransferContext,
  file: DiscoveredFile,
): Promise<SingleFileTransferOutcome> {
  let leaseAttempt = 0
  for (;;) {
    const preflight = preflightOutcome(ctx)
    if (preflight !== undefined) return preflight
    ctx.signal.throwIfAborted()

    const attempt = await attemptSingleFileTransfer(ctx, file, leaseAttempt)
    if (attempt === 'retry-lease') {
      leaseAttempt += 1
      continue
    }
    return attempt
  }
}

function preflightOutcome(ctx: FileTransferContext): SingleFileTransferOutcome | undefined {
  if (ctx.isPaused()) return { kind: 'paused', reason: 'user' }
  if (ctx.isStopped()) return { kind: 'stopped' }
  return undefined
}

async function attemptSingleFileTransfer(
  ctx: FileTransferContext,
  file: DiscoveredFile,
  leaseAttempt: number,
): Promise<SingleFileTransferOutcome | 'retry-lease'> {
  let opened: OpenedRevision | undefined
  let writer: TransferOutputFile | undefined
  const rangeState: { current: ByteRangeSet | undefined } = { current: undefined }
  try {
    opened = await openRevisionWithBackoff(ctx, file.entry.id)
    const desc = opened.descriptor
    const revisionText = encodeBase64Url(desc.fileRevision)
    writer = await openOutputFile(ctx, file, opened, revisionText)

    if (writer.alreadyCompleted) {
      ctx.onInitialCoverage?.(desc.exactSize)
      return { kind: 'completed', size: desc.exactSize }
    }
    if (hasObservedRevisionChange(writer, opened, revisionText)) {
      await abortChangedWriter(writer)
      return { kind: 'source-changed' }
    }

    rangeState.current = new ByteRangeSet(desc.exactSize, writer.completedRanges)
    const initialBytes = rangesBytes(rangeState.current.ranges)
    if (initialBytes > 0n) ctx.onInitialCoverage?.(initialBytes)

    await streamMissingRanges(ctx, opened, writer, rangeState)
    await writer.commit(rangeState.current.ranges)
    return { kind: 'completed', size: desc.exactSize }
  } catch (error) {
    return handleFileError(ctx, writer, rangeState.current, error, leaseAttempt)
  } finally {
    await opened?.release().catch(() => undefined)
  }
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

interface CheckpointClock {
  pendingBytes: bigint
  lastCheckpointAt: number
}

async function streamMissingRanges(
  ctx: FileTransferContext,
  opened: OpenedRevision,
  writer: TransferOutputFile,
  rangeState: { current: ByteRangeSet | undefined },
): Promise<void> {
  const exactSize = opened.descriptor.exactSize
  const initial = rangeState.current
  if (exactSize === 0n || initial === undefined) return

  const missing = initial.missingFrom(new ByteRangeSet(exactSize, [byteRange(0n, exactSize)]))
  if (missing.empty) return

  const clock: CheckpointClock = { pendingBytes: 0n, lastCheckpointAt: Date.now() }
  for (const span of missing.ranges) {
    throwIfInterrupted(ctx)
    await streamSingleSpan(ctx, opened, writer, span, exactSize, rangeState, clock)
  }
}

async function streamSingleSpan(
  ctx: FileTransferContext,
  opened: OpenedRevision,
  writer: TransferOutputFile,
  span: ByteRange,
  exactSize: bigint,
  rangeState: { current: ByteRangeSet | undefined },
  clock: CheckpointClock,
): Promise<void> {
  const stream = ctx.broker.readRange(opened.descriptor, opened.leaseId, span, {
    signal: ctx.signal,
    maximumParallel: BLOCK_BROKER_PARALLEL_READS,
    ...(ctx.onObjectBytes === undefined ? {} : { onReceive: ctx.onObjectBytes }),
    ...(ctx.remainingWork === undefined ? {} : { remainingWork: ctx.remainingWork }),
  })
  for await (const slice of stream) {
    throwIfInterrupted(ctx)
    if (slice.data.byteLength === 0) continue
    const len = BigInt(slice.data.byteLength)
    await writer.writeAt(slice.offset, slice.data)
    const next = rangeState.current!.union(new ByteRangeSet(exactSize, [byteRange(slice.offset, slice.offset + len)]))
    rangeState.current = next
    ctx.onWritten?.(len)
    await maybeCheckpoint(ctx, writer, next, len, clock)
  }
}

async function maybeCheckpoint(
  ctx: FileTransferContext,
  writer: TransferOutputFile,
  current: ByteRangeSet,
  writtenBytes: bigint,
  clock: CheckpointClock,
): Promise<void> {
  const policy = ctx.output.capabilities.periodicCheckpoint
  if (policy === undefined) return
  clock.pendingBytes += writtenBytes
  const now = Date.now()
  if (clock.pendingBytes < policy.intervalBytes && now - clock.lastCheckpointAt < policy.intervalMilliseconds) {
    return
  }
  await writer.checkpoint(current.ranges)
  clock.pendingBytes = 0n
  clock.lastCheckpointAt = now
}

function throwIfInterrupted(ctx: FileTransferContext): void {
  if (ctx.isPaused()) throw new TransferPauseError('user')
  if (ctx.isStopped()) throw new TransferStopError()
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
  error: unknown,
  leaseAttempt: number,
): Promise<SingleFileTransferOutcome | 'retry-lease'> {
  const pauseReason = pauseReasonFor(ctx, error)
  if (pauseReason !== undefined) {
    await pauseWriterSafely(writer, ranges)
    return { kind: 'paused', reason: pauseReason }
  }
  if (error instanceof TransferStopError || ctx.isStopped()) {
    await writer?.abort(error).catch(() => undefined)
    return { kind: 'stopped' }
  }
  if (isSourceChangedError(error)) {
    await writer?.abort(error).catch(() => undefined)
    return { kind: 'source-changed' }
  }
  if (requiresRevisionLeaseReplacement(error) && leaseAttempt < MAX_LEASE_RETRIES && !ctx.signal.aborted) {
    await checkpointWriterSafely(writer, ranges)
    return 'retry-lease'
  }
  await writer?.abort(error).catch(() => undefined)
  if (isAbortError(error) && ctx.signal.aborted) {
    return ctx.isPaused() ? { kind: 'paused', reason: 'user' } : { kind: 'stopped' }
  }
  return error instanceof RemoteRevisionError
    ? { kind: 'source-unavailable', error }
    : { kind: 'failed', error }
}

function pauseReasonFor(
  ctx: FileTransferContext,
  error: unknown,
): 'user' | 'storage-full' | 'permission-needed' | undefined {
  if (error instanceof TransferPauseError) return error.reason
  if (ctx.isPaused()) return 'user'
  if (isQuotaExceededError(error)) return 'storage-full'
  if (isPermissionDeniedError(error)) return 'permission-needed'
  return undefined
}

async function pauseWriterSafely(
  writer: TransferOutputFile | undefined,
  ranges: ByteRangeSet | undefined,
): Promise<void> {
  if (writer !== undefined && ranges !== undefined) {
    await writer.pause(ranges.ranges).catch(() => undefined)
  }
}

async function checkpointWriterSafely(
  writer: TransferOutputFile | undefined,
  ranges: ByteRangeSet | undefined,
): Promise<void> {
  if (writer !== undefined && ranges !== undefined) {
    await writer.checkpoint(ranges.ranges).catch(() => undefined)
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

function rangesBytes(ranges: readonly ByteRange[]): bigint {
  let total = 0n
  for (const range of ranges) total += range.end - range.start
  return total
}

function sleepAbortable(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) { reject(signal.reason); return }
    const onAbort = () => { clearTimeout(timer); reject(signal.reason) }
    const timer = setTimeout(() => { signal.removeEventListener('abort', onAbort); resolve() }, ms)
    signal.addEventListener('abort', onAbort, { once: true })
  })
}
