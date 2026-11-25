import { BLOCK_BROKER_PARALLEL_READS, type BlockRangeReader } from '../content/broker'
import { REVISION_CODE_BUSY } from '../content/flow'
import { ByteRangeSet, byteRange } from '../content/geometry'
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
  RetryableFilePublishError,
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
  readonly getExit: () => TransferExit | undefined
  readonly trackWrite?: (file: ActiveFileWrite | undefined) => void
  readonly onUncheckpointedWrite?: () => void
  readonly onOutputFailure?: (error: unknown) => void
  readonly onInitialCoverage?: (bytes: bigint) => void
  readonly onObjectBytes?: (bytes: number) => void
  readonly onWriteCoverageChange?: (delta: bigint) => void
  readonly onCapacityWait?: (deltaWaiting: number, addedWaitMs: number, addedAttempt: number) => void
  readonly remainingWork?: () => { readonly bytes: number; readonly known: boolean }
}

export type TransferPause = Readonly<{
  kind: 'paused'
  reason: ReceivePauseReason
  sharedOutputFailure?: TransferFaultError
}>

export type TransferExit =
  | TransferPause
  | Readonly<{ kind: 'stopped' | 'detached' }>
  | Readonly<{ kind: 'failed'; error: unknown }>

export type SingleFileTransferOutcome = TransferExit
  | Readonly<{ kind: 'completed'; size: bigint }>
  | Readonly<{ kind: 'source-changed' }>
  | Readonly<{ kind: 'source-unavailable'; error: unknown }>
  | Readonly<{ kind: 'source-busy'; error: unknown }>

interface FileTransferAttemptState {
  readonly attemptIndex: number
  readonly leaseRetries: number
  readonly stalledReopens: number
  readonly retriedPublication: boolean
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
    retriedPublication: false,
  }
  for (;;) {
    const preflight = ctx.getExit()
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
      throw new TransferFaultError(sourceFault(FaultScope.FileLocal, SourceFaultCode.RevisionChanged), 'Source file changed')
    }

    active = new ActiveFileWrite(writer, desc.exactSize, ctx.signal, ctx.output.capabilities.periodicCheckpoint)
    ctx.trackWrite?.(active)
    initialBytes = active.coverage
    if (state.attemptIndex === 0 && initialBytes > 0n) ctx.onInitialCoverage?.(initialBytes)

    await streamMissingRanges(ctx, opened, active)
    release = releaseLease(opened)
    ctx.onUncheckpointedWrite?.()
    try {
      await active.commit()
    } catch (error) {
      if (!(error instanceof RetryableFilePublishError) || initialBytes !== 0n) throw error
      return retryDiscardedPublication(ctx, active.coverage, desc, state, error)
    }
    return { kind: 'completed', size: desc.exactSize }
  } catch (error) {
    await active?.finishWrites()
    return await handleFileError(ctx, writer, active, initialBytes, active?.failure ?? error, state)
  } finally {
    await (release ?? releaseLease(opened))
  }
}

function retryDiscardedPublication(
  ctx: FileTransferContext,
  discardedBytes: bigint,
  revision: FileRevisionDescriptor,
  state: FileTransferAttemptState,
  error: RetryableFilePublishError,
): AttemptResult {
  // The backend has already removed this attempt's entire output, even if a pending exit or the retry limit wins.
  ctx.trackWrite?.(undefined)
  ctx.onWriteCoverageChange?.(-discardedBytes)
  const exit = ctx.getExit()
  if (exit !== undefined) return exit
  if (ctx.signal.aborted || !ctx.output.capabilities.resumable || state.retriedPublication) return { kind: 'failed', error }
  return {
    kind: 'retry',
    next: { ...state, attemptIndex: state.attemptIndex + 1, retriedPublication: true, reopenedFrom: revision },
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
      await active.write(slice.offset, slice.data, ctx.onWriteCoverageChange)
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
  active: ActiveFileWrite | undefined,
  initialBytes: bigint,
  error: unknown,
  state: FileTransferAttemptState,
): Promise<AttemptResult> {
  const exit = ctx.getExit()
  if (exit !== undefined && exit.kind !== 'paused') return abortFile(writer, error, exit)
  if (error instanceof TransferStopError) return abortFile(writer, error, { kind: 'stopped' })
  if (isSourceChangedError(error)) return abortFile(writer, error, { kind: 'source-changed' })
  if (exit?.sharedOutputFailure !== undefined) return abortFile(writer, exit.sharedOutputFailure, exit)
  if (active?.failure !== undefined || error instanceof TransferFaultError || storagePauseReason(error) !== undefined) {
    return abortFile(writer, error, outputFailureOutcome(ctx, error))
  }

  const pauseReason = exit?.reason ?? (error instanceof TransferPauseError ? error.reason : undefined)
  if (pauseReason !== undefined) return pauseFile(ctx, writer, active, error, pauseReason)
  const next = nextRetryStateFor(ctx, (active?.coverage ?? 0n) > initialBytes, error, state)
  if (next !== undefined) {
    try {
      await active?.checkpointForRetry()
      return { kind: 'retry', next }
    } catch (cause) {
      return abortFile(writer, cause, outputFailureOutcome(ctx, cause))
    }
  }
  if (ctx.output.capabilities.resumable && !ctx.signal.aborted && isRecoverableTransferInterruption(error)) {
    return pauseFile(ctx, writer, active, error, 'interrupted')
  }
  return abortFile(writer, error, terminalFileOutcome(error))
}

function terminalFileOutcome(error: unknown): SingleFileTransferOutcome {
  if (isSourceBusyError(error)) return { kind: 'source-busy', error }
  if (error instanceof RemoteRevisionError && !(error instanceof RevisionCapacityBusyError)) {
    return { kind: 'source-unavailable', error }
  }
  return { kind: 'failed', error }
}

/** A storage failure retires its writer. Healthy siblings can still save their own ranges when the job pauses. */
function outputFailureOutcome(ctx: FileTransferContext, error: unknown): TransferExit {
  const exit = ctx.getExit()
  if (exit !== undefined && exit.kind !== 'paused') return exit
  ctx.onOutputFailure?.(error)
  const reason = storagePauseReason(error)
  return ctx.output.capabilities.resumable && reason !== undefined ? { kind: 'paused', reason } : { kind: 'failed', error }
}

async function abortFile(
  writer: TransferOutputFile | undefined,
  cause: unknown,
  outcome: SingleFileTransferOutcome,
): Promise<SingleFileTransferOutcome> {
  try { await writer?.abort(cause) } catch { /* Cleanup cannot replace the cause that retired this writer. */ }
  return outcome
}

async function pauseFile(
  ctx: FileTransferContext,
  writer: TransferOutputFile | undefined,
  active: ActiveFileWrite | undefined,
  error: unknown,
  reason: ReceivePauseReason,
): Promise<SingleFileTransferOutcome> {
  if (active === undefined) return abortFile(writer, error, { kind: 'paused', reason })
  try {
    await active.pause(error)
    return { kind: 'paused', reason }
  } catch (cause) {
    return abortFile(writer, cause, outputFailureOutcome(ctx, cause))
  }
}

function nextRetryStateFor(
  ctx: FileTransferContext,
  madeProgress: boolean,
  error: unknown,
  state: FileTransferAttemptState,
): FileTransferAttemptState | undefined {
  if (!ctx.output.capabilities.resumable || ctx.signal.aborted) return undefined
  if (isReusableReopenedRevisionError(error)) {
    const stalledReopens = madeProgress ? 0 : state.stalledReopens + 1
    if (stalledReopens > MAX_STALLED_REOPEN_RETRIES) return undefined
    return {
      ...state,
      attemptIndex: state.attemptIndex + 1,
      leaseRetries: madeProgress ? 0 : state.leaseRetries,
      stalledReopens,
      reopenedFrom: error.previous,
    }
  }
  if (requiresRevisionLeaseReplacement(error)) {
    const leaseRetries = madeProgress ? 0 : state.leaseRetries + 1
    if (leaseRetries > MAX_LEASE_RETRIES) return undefined
    return {
      ...state,
      attemptIndex: state.attemptIndex + 1,
      leaseRetries,
      stalledReopens: madeProgress ? 0 : state.stalledReopens,
    }
  }
  return undefined
}

function isReusableReopenedRevisionError(error: unknown): error is RevisionReopenedDuringRecoveryError {
  return error instanceof RevisionReopenedDuringRecoveryError &&
    error.comparison.kind === 'metadata-agreement'
}

const REVISION_CODE_STALE = 0x3001
const REVISION_CODE_NOT_FOUND = 0x3002
const REVISION_CODE_DRIFT = 0x3007

function isSourceChangedRevisionCode(code: number): boolean {
  return code === REVISION_CODE_STALE || code === REVISION_CODE_NOT_FOUND || code === REVISION_CODE_DRIFT
}

function isSourceBusyError(error: unknown): boolean {
  if (error instanceof RemoteRevisionError) {
    return error.failure.code === REVISION_CODE_BUSY
  }
  if (error instanceof RemoteOperationError) {
    return error.scope === 'revision' && error.code === REVISION_CODE_BUSY
  }
  return false
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
