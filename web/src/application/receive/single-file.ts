import type { CatalogEntry } from '../../catalog/records'
import { snapshotCatalogPath } from '../../catalog/path-policy'
import { SelectionPolicy, type FrozenSelectionPolicy } from '../../catalog/selection'
import type { ArtifactChoice, ArtifactOffers, ReceiveOperationDisplay } from '../../output/model'
import {
  planArtifactOffers,
  type BrowserOutputCapabilities,
} from '../../output/backends/contract'
import type { SelectionShape } from '../../transfer/discovery'
import {
  selectionKey,
  selectionSpecFromPolicy,
  type SelectionSpec,
} from '../../transfer/selection'
import type { BrowseDirectory, JoinedBrowserShare } from '../browser-share/gateway'
import type { BrowserCapabilityCache } from './capability-cache'
import type {
  ReceiveBlockReason,
  SaveUnavailableReason,
  SingleFileDownloadOutcome,
} from './contracts'

export class SaveUnavailableError extends Error {
  readonly reason: SaveUnavailableReason

  constructor(reason: SaveUnavailableReason, message = `Single-file save unavailable: ${reason}`) {
    super(message)
    this.name = 'SaveUnavailableError'
    this.reason = reason
  }
}

export class ReceiveBlockedError extends Error {
  readonly reason: ReceiveBlockReason

  constructor(reason: ReceiveBlockReason, message = `Receive action blocked: ${reason}`) {
    super(message)
    this.name = 'ReceiveBlockedError'
    this.reason = reason
  }
}

export interface SingleFileDownloadPlan {
  readonly selection: FrozenSelectionPolicy
  readonly spec: SelectionSpec
  readonly selectionKey: string
  readonly offers: ArtifactOffers
}

export interface SingleFileDownloadOptions {
  readonly display?: ReceiveOperationDisplay
  readonly onActivated?: () => void
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
  readonly capabilities: BrowserCapabilityCache
  isDisposed(): boolean
  currentJoinedShare(): JoinedBrowserShare | undefined
  startBlockedReason(): ReceiveBlockReason | null
  launchChoice(
    request: SingleFileLaunchRequest,
    options: SingleFileDownloadOptions | undefined,
  ): Extract<SingleFileDownloadOutcome, { kind: 'started' | 'waiting-picker' | 'failed' }>
  dispatchAsyncError(error: unknown, custom?: (error: unknown) => void): void
}

/**
 * Synchronously validates the target file path and plans single-file offers
 * using a throwaway SelectionPolicy so `joined.selection` draft state is untouched.
 */
export function planSingleFileDownload(
  joined: JoinedBrowserShare,
  entry: Extract<CatalogEntry, { kind: 'file' }>,
  directory: BrowseDirectory,
  capabilities: BrowserOutputCapabilities,
): SingleFileDownloadPlan {
  const sourcePath = snapshotCatalogPath([...directory.path, entry.name])
  const policy = new SelectionPolicy(false)
  policy.set(entry, directory.ancestry, true)
  const selection = policy.snapshot()
  const spec = selectionSpecFromPolicy(
    joined.descriptor.shareInstanceId,
    joined.descriptor.syntheticRootId,
    selection,
  )
  const key = selectionKey(spec)
  const shape: Extract<SelectionShape, { kind: 'single-file' }> = Object.freeze({
    kind: 'single-file',
    file: entry,
    sourcePath,
    exactBytes: entry.expectedSize,
  })
  const offers = planArtifactOffers({ epoch: 0n, selectionKey: key, shape, capabilities })
  return Object.freeze({ selection, spec, selectionKey: key, offers })
}

/**
 * Synchronously launches a fresh single-file download from the sender.
 * Never repurposes the download action to export or resume historical tasks,
 * keeping behavior predictable and preserving synchronous user activation for FSA pickers.
 */
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
