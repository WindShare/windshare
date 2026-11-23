import type { CatalogEntry } from '../../catalog/records'
import type { FrozenSelectionPolicy } from '../../catalog/selection'
import type { BrowserOutputCapabilities } from '../../output/backends/contract'
import { emitOutputTrace, type OutputTraceSource } from '../../output/diagnostics'
import type { ArtifactChoice, ReceiveOperationDisplay } from '../../output/model'
import { isAbortError } from '../../transfer/fault'
import type { SelectionSpec } from '../../transfer/selection'
import type { BrowseDirectory, JoinedBrowserShare } from '../browser-share/gateway'
import type { BrowserCapabilityCache } from './capability-cache'
import type {
  ReceiveAction,
  ReceiveAdmission,
  ReceiveBlockReason,
  RetainedTaskSnapshot,
  SingleFileDownloadOutcome,
} from './contracts'
import type { ExecutionState } from './execution-state'
import type { RetainedTaskActions } from './retained-actions'
import type { RetainedTaskInventory } from './retained-inventory'
import {
  ReceiveBlockedError,
  SaveUnavailableError,
  decideSingleFileReuse,
  planSingleFileDownload,
  type SingleFileDownloadPlan,
  type SingleFileReuseDecision,
} from './single-file'

export interface SingleFileDownloadOptions {
  readonly display?: ReceiveOperationDisplay
  readonly onActivated?: () => void
  readonly onExported?: () => void
  readonly onAsyncError?: (error: unknown) => void
}

export interface SingleFileLaunchRequest {
  readonly joined: JoinedBrowserShare
  readonly choice: ArtifactChoice
  readonly spec: SelectionSpec
  readonly selection: FrozenSelectionPolicy
  readonly key: string
  readonly display: ReceiveOperationDisplay | undefined
}

/** Narrow internal collaboration surface owned by `ReceiveCoordinator`. */
export interface SingleFileLaunchContext {
  readonly execution: ExecutionState
  readonly inventory: RetainedTaskInventory
  readonly capabilities: BrowserCapabilityCache
  readonly retainedActions: RetainedTaskActions
  readonly outputTrace?: OutputTraceSource
  isDisposed(): boolean
  currentJoinedShare(): JoinedBrowserShare | undefined
  startBlockedReason(): ReceiveBlockReason | null
  preloadShareTasks(shareInstance: string): void
  admitCandidate(task: RetainedTaskSnapshot, action: ReceiveAction, ignoreTransferSlot: boolean): ReceiveAdmission
  publish(): void
  launchChoice(
    request: SingleFileLaunchRequest,
    options: SingleFileDownloadOptions | undefined,
  ): Extract<SingleFileDownloadOutcome, { kind: 'started' | 'waiting-picker' | 'failed' }>
  openWithTransferToken(
    token: symbol,
    request: SingleFileLaunchRequest,
    options: SingleFileDownloadOptions | undefined,
  ): Promise<void>
  dispatchAsyncError(error: unknown, custom?: (error: unknown) => void): void
}

export function startSingleFileDownloadFlow(
  ctx: SingleFileLaunchContext,
  entry: Extract<CatalogEntry, { kind: 'file' }>,
  directory: BrowseDirectory,
  options?: SingleFileDownloadOptions,
): SingleFileDownloadOutcome {
  const ready = checkSingleFileReady(ctx, options)
  if (ready.kind !== 'ready') return ready.outcome

  let planned: SingleFileDownloadPlan
  try {
    planned = planSingleFileDownload(ready.joined, entry, directory, ready.capabilities)
  } catch (error) {
    return Object.freeze({ kind: 'failed', error })
  }

  const shareInstance = ready.joined.descriptor.shareInstanceId
  ctx.preloadShareTasks(shareInstance)
  const pendingIndex = ctx.inventory.pendingSharePromise(shareInstance)
  if (pendingIndex !== undefined) {
    return startWhileShareIndexLoading(ctx, ready.joined, entry, planned, pendingIndex, options)
  }

  const inspected = ctx.inventory.inspectKeyCandidates(shareInstance, planned.selectionKey)
  if (inspected.kind === 'none') {
    return launchFreshSingleFile(ctx, ready.joined, planned, options)
  }
  if (inspected.kind === 'needs-resolution') {
    return startAsyncCandidateResolution(ctx, ready.joined, entry, planned, undefined, inspected.taskIds, options)
  }

  const decision = decideSingleFileReuse(
    inspected.candidates,
    shareInstance,
    planned.selectionKey,
    (task, action) => ctx.admitCandidate(task, action, false),
  )
  const reused = applySyncReuseDecision(ctx, entry.idText, decision, options)
  if (reused !== undefined) return reused

  return launchFreshSingleFile(ctx, ready.joined, planned, options)
}

function checkSingleFileReady(
  ctx: SingleFileLaunchContext,
  options: SingleFileDownloadOptions | undefined,
):
  | Readonly<{ kind: 'ready'; joined: JoinedBrowserShare; capabilities: BrowserOutputCapabilities }>
  | Readonly<{ kind: 'stop'; outcome: SingleFileDownloadOutcome }> {
  if (ctx.isDisposed()) return { kind: 'stop', outcome: Object.freeze({ kind: 'blocked', reason: 'actionUnavailable' }) }
  const blocked = ctx.startBlockedReason()
  if (blocked !== null) return { kind: 'stop', outcome: Object.freeze({ kind: 'blocked', reason: blocked }) }
  const joined = ctx.currentJoinedShare()
  if (joined === undefined) return { kind: 'stop', outcome: Object.freeze({ kind: 'blocked', reason: 'connectFirst' }) }
  if (ctx.capabilities.state === 'failed') {
    ctx.capabilities.retry(error => ctx.dispatchAsyncError(error, options?.onAsyncError))
    return { kind: 'stop', outcome: Object.freeze({ kind: 'probing' }) }
  }
  const capabilities = ctx.capabilities.value
  if (ctx.capabilities.state === 'probing' || capabilities === undefined) {
    void ctx.capabilities.ensure().catch(() => undefined)
    return { kind: 'stop', outcome: Object.freeze({ kind: 'probing' }) }
  }
  return { kind: 'ready', joined, capabilities }
}

function startWhileShareIndexLoading(
  ctx: SingleFileLaunchContext,
  joined: JoinedBrowserShare,
  entry: Extract<CatalogEntry, { kind: 'file' }>,
  planned: SingleFileDownloadPlan,
  pendingIndex: Promise<void>,
  options: SingleFileDownloadOptions | undefined,
): SingleFileDownloadOutcome {
  if (planned.offers.kind === 'ready' && planned.offers.primary.backend === 'fsa') {
    return Object.freeze({ kind: 'probing' })
  }
  return startAsyncCandidateResolution(ctx, joined, entry, planned, pendingIndex, undefined, options)
}

function startAsyncCandidateResolution(
  ctx: SingleFileLaunchContext,
  joined: JoinedBrowserShare,
  entry: Extract<CatalogEntry, { kind: 'file' }>,
  planned: SingleFileDownloadPlan,
  pendingIndex: Promise<void> | undefined,
  preMatchedTaskIds: readonly string[] | undefined,
  options: SingleFileDownloadOptions | undefined,
): SingleFileDownloadOutcome {
  const choiceId = planned.offers.kind === 'ready' ? planned.offers.primary.choiceId : 'single-file:lookup'
  const token = ctx.execution.claimResolvingReuse(choiceId, entry.name, entry.idText)
  if (token === null) return Object.freeze({ kind: 'blocked', reason: 'busy' })
  ctx.publish()
  void resolveSingleFileAsync(ctx, token, joined, entry.idText, planned, pendingIndex, preMatchedTaskIds, options)
  return Object.freeze({ kind: 'started' })
}

function applySyncReuseDecision(
  ctx: SingleFileLaunchContext,
  singleFileId: string,
  decision: SingleFileReuseDecision,
  options: SingleFileDownloadOptions | undefined,
): SingleFileDownloadOutcome | undefined {
  if (decision.kind === 'none') return undefined
  traceReuseDecision(ctx.outputTrace, decision)
  if (decision.kind === 'export') {
    void ctx.retainedActions.runLocalAction(decision.task.taskId, 'save', singleFileId, exportCallbacks(options))
    return Object.freeze({ kind: 'exported' })
  }
  if (decision.kind === 'resume') {
    void ctx.retainedActions.runContinue(decision.task.taskId, singleFileId, resumeCallbacks(options))
    return Object.freeze({ kind: 'resumed' })
  }
  return Object.freeze({ kind: 'blocked', reason: decision.reason })
}

function launchFreshSingleFile(
  ctx: SingleFileLaunchContext,
  joined: JoinedBrowserShare,
  planned: SingleFileDownloadPlan,
  options: SingleFileDownloadOptions | undefined,
): SingleFileDownloadOutcome {
  if (planned.offers.kind === 'unavailable') {
    return Object.freeze({ kind: 'unavailable', error: new SaveUnavailableError(planned.offers.reason) })
  }
  if (planned.offers.kind !== 'ready') {
    return Object.freeze({ kind: 'failed', error: new Error('Single-file download options are unavailable') })
  }
  return ctx.launchChoice({
    joined,
    choice: planned.offers.primary,
    spec: planned.spec,
    selection: planned.selection,
    key: planned.selectionKey,
    display: options?.display,
  }, options)
}

async function resolveSingleFileAsync(
  ctx: SingleFileLaunchContext,
  token: symbol,
  joined: JoinedBrowserShare,
  singleFileId: string,
  planned: SingleFileDownloadPlan,
  pendingIndex: Promise<void> | undefined,
  preMatchedTaskIds: readonly string[] | undefined,
  options: SingleFileDownloadOptions | undefined,
): Promise<void> {
  try {
    const shareInstance = joined.descriptor.shareInstanceId
    if (pendingIndex !== undefined) {
      await pendingIndex
      if (!ensureActiveForJoined(ctx, token, joined)) return
    }

    const candidates = await resolveCandidatesForAsyncLookup(ctx, shareInstance, planned.selectionKey, preMatchedTaskIds)
    if (!ensureActiveForJoined(ctx, token, joined)) return

    const decision = decideSingleFileReuse(
      candidates,
      shareInstance,
      planned.selectionKey,
      (task, action) => ctx.admitCandidate(task, action, true),
    )
    if (dispatchAsyncReuseDecision(ctx, token, singleFileId, decision, options)) return

    const asyncError = asyncFreshStartError(ctx, planned)
    if (asyncError !== undefined) {
      releaseAndReportError(ctx, token, asyncError, options)
      return
    }
    if (planned.offers.kind !== 'ready' || !ctx.execution.transitionToOpening(token)) return
    await ctx.openWithTransferToken(token, {
      joined,
      choice: planned.offers.primary,
      spec: planned.spec,
      selection: planned.selection,
      key: planned.selectionKey,
      display: options?.display,
    }, options)
  } catch (error) {
    if (!ctx.execution.ownsTransfer(token)) return
    ctx.execution.releaseTransferIfOwned(token)
    ctx.publish()
    if (!ctx.isDisposed() && !isAbortError(error)) {
      ctx.dispatchAsyncError(error, options?.onAsyncError)
    }
  }
}

function ensureActiveForJoined(
  ctx: SingleFileLaunchContext,
  token: symbol,
  joined: JoinedBrowserShare,
): boolean {
  if (!ctx.execution.ownsTransfer(token)) return false
  if (ctx.currentJoinedShare() !== joined) {
    ctx.execution.releaseTransferIfOwned(token)
    ctx.publish()
    return false
  }
  return true
}

async function resolveCandidatesForAsyncLookup(
  ctx: SingleFileLaunchContext,
  shareInstance: string,
  key: string,
  preMatchedTaskIds: readonly string[] | undefined,
): Promise<readonly RetainedTaskSnapshot[]> {
  if (preMatchedTaskIds !== undefined) {
    return ctx.inventory.resolveKeyCandidates(shareInstance, key, preMatchedTaskIds)
  }
  const inspected = ctx.inventory.inspectKeyCandidates(shareInstance, key)
  if (inspected.kind === 'none') return Object.freeze([])
  if (inspected.kind === 'ready') return inspected.candidates
  return ctx.inventory.resolveKeyCandidates(shareInstance, key, inspected.taskIds)
}

function dispatchAsyncReuseDecision(
  ctx: SingleFileLaunchContext,
  token: symbol,
  singleFileId: string,
  decision: SingleFileReuseDecision,
  options: SingleFileDownloadOptions | undefined,
): boolean {
  if (decision.kind === 'none') return false
  traceReuseDecision(ctx.outputTrace, decision)
  if (decision.kind === 'export') {
    void ctx.retainedActions.runLocalAction(decision.task.taskId, 'save', singleFileId, exportCallbacks(options), token)
    return true
  }
  if (decision.kind === 'resume') {
    void ctx.retainedActions.runContinue(decision.task.taskId, singleFileId, resumeCallbacks(options), token)
    return true
  }
  releaseAndReportError(ctx, token, new ReceiveBlockedError(decision.reason), options)
  return true
}

function asyncFreshStartError(
  ctx: SingleFileLaunchContext,
  planned: SingleFileDownloadPlan,
): Error | undefined {
  if (ctx.execution.hasLocalAction) return new ReceiveBlockedError('busy')
  if (planned.offers.kind === 'unavailable') return new SaveUnavailableError(planned.offers.reason)
  if (planned.offers.kind !== 'ready' || planned.offers.primary.backend === 'fsa') {
    // FSA requires synchronous user activation; once the vanished record is pruned, the next click opens the picker.
    return new ReceiveBlockedError('actionUnavailable')
  }
  return undefined
}

function releaseAndReportError(
  ctx: SingleFileLaunchContext,
  token: symbol,
  error: unknown,
  options: SingleFileDownloadOptions | undefined,
): void {
  ctx.execution.releaseTransferIfOwned(token)
  ctx.publish()
  ctx.dispatchAsyncError(error, options?.onAsyncError)
}

function exportCallbacks(options: SingleFileDownloadOptions | undefined) {
  return {
    ...(options?.onExported === undefined ? {} : { onCompleted: options.onExported }),
    ...(options?.onAsyncError === undefined ? {} : { onError: options.onAsyncError }),
  }
}

function resumeCallbacks(options: SingleFileDownloadOptions | undefined) {
  return {
    ...(options?.onActivated === undefined ? {} : { onActivated: options.onActivated }),
    ...(options?.onAsyncError === undefined ? {} : { onError: options.onAsyncError }),
  }
}

function traceReuseDecision(trace: OutputTraceSource | undefined, decision: Exclude<SingleFileReuseDecision, { kind: 'none' }>): void {
  emitOutputTrace(trace, 'continuation', {
    task_id: decision.task.taskId,
    transition: 'single_file_reuse',
    decision: decision.kind,
    ...(decision.kind === 'blocked' ? { reason: decision.reason } : {}),
  })
}
