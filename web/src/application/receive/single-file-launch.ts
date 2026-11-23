import type { CatalogEntry } from '../../catalog/records'
import type { ArtifactChoice, ReceiveOperationDisplay } from '../../output/model'
import type { BrowserOutputCapabilities } from '../../output/backends/contract'
import type { ReceiveTaskRecord } from '../../output/store/types'
import type { FrozenSelectionPolicy } from '../../catalog/selection'
import type { SelectionSpec } from '../../transfer/selection'
import type { BrowseDirectory, JoinedBrowserShare } from '../browser-share/gateway'
import type {
  ActiveActivationSnapshot,
  BrowserCapabilitiesState,
  ReceiveAction,
  ReceiveAdmission,
  ReceiveBlockReason,
  RetainedTaskSnapshot,
  SingleFileDownloadOutcome,
} from './contracts'
import { retainedSnapshot } from './retained'
import {
  ReceiveBlockedError,
  SaveUnavailableError,
  decideSingleFileReuse,
  isSingleFileReuseCandidateRecord,
  mergeRetainedCandidates,
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

export interface SingleFileLaunchHost {
  readonly isDisposed: () => boolean
  readonly startBlockedReason: () => ReceiveBlockReason | null
  readonly currentJoinedShare: () => JoinedBrowserShare | undefined
  readonly capabilitiesState: () => BrowserCapabilitiesState
  readonly capabilitiesValue: () => BrowserOutputCapabilities | undefined
  readonly retryCapabilities: () => void
  readonly ensureCapabilities: () => void
  readonly preloadShareTasks: (shareInstance: string) => void
  readonly pendingShareIndex: (shareInstance: string) => Promise<readonly ReceiveTaskRecord[]> | undefined
  readonly shareIndexRecords: (shareInstance: string, key: string) => readonly ReceiveTaskRecord[]
  readonly retainedTasks: () => readonly RetainedTaskSnapshot[]
  readonly hasPendingRetained: () => boolean
  readonly currentSettledCandidate: (shareInstance: string, key: string) => RetainedTaskSnapshot | undefined
  readonly admitCandidate: (task: RetainedTaskSnapshot, action: ReceiveAction, ignoreActivation: boolean) => ReceiveAdmission
  readonly claimActivation: (snapshot: ActiveActivationSnapshot) => Readonly<{ token: symbol; activation: ActiveActivationSnapshot }>
  readonly ownsActivation: (token: symbol) => boolean
  readonly clearActivationAndPublish: () => void
  readonly clearActivationOnly: () => void
  readonly normalizeRecords: (token: symbol, records: readonly ReceiveTaskRecord[]) => Promise<readonly RetainedTaskSnapshot[]>
  readonly runRetainedSave: (taskId: string, options: SingleFileDownloadOptions | undefined) => void
  readonly runRetainedContinue: (taskId: string, options: SingleFileDownloadOptions | undefined) => void
  readonly launchChoice: (
    request: SingleFileLaunchRequest,
    options: SingleFileDownloadOptions | undefined,
  ) => Extract<SingleFileDownloadOutcome, { kind: 'started' | 'waiting-picker' | 'failed' }>
  readonly openWithActivation: (
    token: symbol,
    activation: ActiveActivationSnapshot,
    request: SingleFileLaunchRequest,
    options: SingleFileDownloadOptions | undefined,
  ) => Promise<void>
  readonly dispatchAsyncError: (error: unknown, custom?: (error: unknown) => void) => void
}

export function startSingleFileDownloadFlow(
  host: SingleFileLaunchHost,
  entry: Extract<CatalogEntry, { kind: 'file' }>,
  directory: BrowseDirectory,
  options?: SingleFileDownloadOptions,
): SingleFileDownloadOutcome {
  const ready = checkSingleFileReady(host)
  if (ready.kind !== 'ready') return ready.outcome

  let planned: SingleFileDownloadPlan
  try {
    planned = planSingleFileDownload(ready.joined, entry, directory, ready.capabilities)
  } catch (error) {
    return Object.freeze({ kind: 'failed', error })
  }

  const shareInstance = ready.joined.descriptor.shareInstanceId
  host.preloadShareTasks(shareInstance)
  const pendingIndex = host.pendingShareIndex(shareInstance)
  if (pendingIndex !== undefined) {
    return startWhileShareIndexLoading(host, ready.joined, entry, planned, pendingIndex, options)
  }

  const collected = collectKnownCandidates(host, shareInstance, planned.selectionKey)
  if (collected.rawNeedingNormalization.length > 0) {
    const claimed = claimSingleFileLookup(host, entry, planned)
    void resolveSingleFileAsync(host, claimed.token, claimed.activation, ready.joined, planned, undefined, collected.rawNeedingNormalization, options)
    return Object.freeze({ kind: 'started' })
  }

  const decision = decideSingleFileReuse(
    collected.resolved,
    shareInstance,
    planned.selectionKey,
    (task, action) => host.admitCandidate(task, action, false),
  )
  const reused = applySyncReuseDecision(host, decision, options)
  if (reused !== undefined) return reused

  return launchFreshSingleFile(host, ready.joined, planned, options)
}

function checkSingleFileReady(
  host: SingleFileLaunchHost,
):
  | Readonly<{ kind: 'ready'; joined: JoinedBrowserShare; capabilities: BrowserOutputCapabilities }>
  | Readonly<{ kind: 'stop'; outcome: SingleFileDownloadOutcome }> {
  if (host.isDisposed()) return { kind: 'stop', outcome: Object.freeze({ kind: 'blocked', reason: 'actionUnavailable' }) }
  const blocked = host.startBlockedReason()
  if (blocked !== null) return { kind: 'stop', outcome: Object.freeze({ kind: 'blocked', reason: blocked }) }
  const joined = host.currentJoinedShare()
  if (joined === undefined) return { kind: 'stop', outcome: Object.freeze({ kind: 'blocked', reason: 'connectFirst' }) }
  if (host.capabilitiesState() === 'failed') {
    host.retryCapabilities()
    return { kind: 'stop', outcome: Object.freeze({ kind: 'probing' }) }
  }
  const capabilities = host.capabilitiesValue()
  if (host.capabilitiesState() === 'probing' || capabilities === undefined) {
    host.ensureCapabilities()
    return { kind: 'stop', outcome: Object.freeze({ kind: 'probing' }) }
  }
  return { kind: 'ready', joined, capabilities }
}

function claimSingleFileLookup(
  host: SingleFileLaunchHost,
  entry: Extract<CatalogEntry, { kind: 'file' }>,
  planned: SingleFileDownloadPlan,
) {
  return host.claimActivation({
    kind: 'committing',
    choiceId: planned.offers.kind === 'ready' ? planned.offers.primary.choiceId : 'single-file:lookup',
    suggestedName: entry.name,
    singleFileId: entry.idText,
  })
}

function startWhileShareIndexLoading(
  host: SingleFileLaunchHost,
  joined: JoinedBrowserShare,
  entry: Extract<CatalogEntry, { kind: 'file' }>,
  planned: SingleFileDownloadPlan,
  pendingIndex: Promise<readonly ReceiveTaskRecord[]>,
  options: SingleFileDownloadOptions | undefined,
): SingleFileDownloadOutcome {
  if (planned.offers.kind === 'ready' && planned.offers.primary.backend === 'fsa') {
    return Object.freeze({ kind: 'probing' })
  }
  const claimed = claimSingleFileLookup(host, entry, planned)
  void resolveSingleFileAsync(host, claimed.token, claimed.activation, joined, planned, pendingIndex, undefined, options)
  return Object.freeze({ kind: 'started' })
}

function collectKnownCandidates(
  host: SingleFileLaunchHost,
  shareInstance: string,
  key: string,
): Readonly<{ resolved: readonly RetainedTaskSnapshot[]; rawNeedingNormalization: readonly ReceiveTaskRecord[] }> {
  const retainedMatches = host.retainedTasks().filter(
    task => task.shareInstance === shareInstance && task.selectionKey === key,
  )
  const settled = host.currentSettledCandidate(shareInstance, key)
  const known = mergeRetainedCandidates([retainedMatches, settled === undefined ? [] : [settled]])
  const knownById = new Map(known.map(task => [task.taskId, task]))
  const rawNeedingNormalization: ReceiveTaskRecord[] = []
  const rawSettled: RetainedTaskSnapshot[] = []
  for (const record of host.shareIndexRecords(shareInstance, key).filter(isSingleFileReuseCandidateRecord)) {
    const existing = knownById.get(record.taskId)
    if (existing !== undefined && existing.lifecycle.generation >= record.lifecycle.generation) continue
    if (record.lifecycle.kind === 'downloading' || record.lifecycle.kind === 'preparing') {
      rawNeedingNormalization.push(record)
    } else {
      rawSettled.push(retainedSnapshot(record, false))
    }
  }
  return { resolved: mergeRetainedCandidates([known, rawSettled]), rawNeedingNormalization }
}

function applySyncReuseDecision(
  host: SingleFileLaunchHost,
  decision: SingleFileReuseDecision,
  options: SingleFileDownloadOptions | undefined,
): SingleFileDownloadOutcome | undefined {
  if (decision.kind === 'export') {
    host.runRetainedSave(decision.task.taskId, options)
    return Object.freeze({ kind: 'exported' })
  }
  if (decision.kind === 'resume') {
    host.runRetainedContinue(decision.task.taskId, options)
    return Object.freeze({ kind: 'resumed' })
  }
  if (decision.kind === 'blocked') {
    return Object.freeze({ kind: 'blocked', reason: decision.reason })
  }
  return undefined
}

function launchFreshSingleFile(
  host: SingleFileLaunchHost,
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
  return host.launchChoice({
    joined,
    choice: planned.offers.primary,
    spec: planned.spec,
    selection: planned.selection,
    key: planned.selectionKey,
    display: options?.display,
  }, options)
}

async function resolveSingleFileAsync(
  host: SingleFileLaunchHost,
  token: symbol,
  activation: ActiveActivationSnapshot,
  joined: JoinedBrowserShare,
  planned: SingleFileDownloadPlan,
  pendingIndex: Promise<readonly ReceiveTaskRecord[]> | undefined,
  preMatchedRaw: readonly ReceiveTaskRecord[] | undefined,
  options: SingleFileDownloadOptions | undefined,
): Promise<void> {
  const shareInstance = joined.descriptor.shareInstanceId
  const rawRecords = pendingIndex !== undefined
    ? (await pendingIndex).filter(record => record.selectionKey === planned.selectionKey && isSingleFileReuseCandidateRecord(record))
    : (preMatchedRaw ?? [])
  if (!host.ownsActivation(token) || host.currentJoinedShare() !== joined) return

  const normalized = rawRecords.length > 0 ? await host.normalizeRecords(token, rawRecords) : Object.freeze([])
  if (!host.ownsActivation(token) || host.currentJoinedShare() !== joined) return

  const retainedMatches = host.retainedTasks().filter(
    task => task.shareInstance === shareInstance && task.selectionKey === planned.selectionKey,
  )
  const settled = host.currentSettledCandidate(shareInstance, planned.selectionKey)
  const candidates = mergeRetainedCandidates([retainedMatches, normalized, settled === undefined ? [] : [settled]])
  const decision = decideSingleFileReuse(
    candidates,
    shareInstance,
    planned.selectionKey,
    (task, action) => host.admitCandidate(task, action, true),
  )

  if (dispatchAsyncReuseDecision(host, decision, options)) return
  const asyncError = asyncFreshStartError(host, planned)
  if (asyncError !== undefined) {
    host.clearActivationAndPublish()
    host.dispatchAsyncError(asyncError, options?.onAsyncError)
    return
  }
  if (planned.offers.kind !== 'ready') return
  await host.openWithActivation(token, activation, {
    joined,
    choice: planned.offers.primary,
    spec: planned.spec,
    selection: planned.selection,
    key: planned.selectionKey,
    display: options?.display,
  }, options)
}

function dispatchAsyncReuseDecision(
  host: SingleFileLaunchHost,
  decision: SingleFileReuseDecision,
  options: SingleFileDownloadOptions | undefined,
): boolean {
  if (decision.kind === 'export') {
    host.clearActivationOnly()
    host.runRetainedSave(decision.task.taskId, options)
    return true
  }
  if (decision.kind === 'resume') {
    host.clearActivationOnly()
    host.runRetainedContinue(decision.task.taskId, options)
    return true
  }
  if (decision.kind === 'blocked') {
    host.clearActivationAndPublish()
    host.dispatchAsyncError(new ReceiveBlockedError(decision.reason), options?.onAsyncError)
    return true
  }
  return false
}

function asyncFreshStartError(
  host: SingleFileLaunchHost,
  planned: SingleFileDownloadPlan,
): Error | undefined {
  if (host.hasPendingRetained()) return new ReceiveBlockedError('busy')
  if (planned.offers.kind === 'unavailable') return new SaveUnavailableError(planned.offers.reason)
  if (planned.offers.kind !== 'ready' || planned.offers.primary.backend === 'fsa') {
    // FSA requires synchronous user activation; if an abandoned record vanished mid-lookup, let the user click once more.
    return new ReceiveBlockedError('actionUnavailable')
  }
  return undefined
}
