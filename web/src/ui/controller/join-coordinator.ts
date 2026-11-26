import { capabilityInputFingerprint } from '../../application/browser-share/capability-input'
import type {
  CapabilityInputLifecycle,
  CapabilityJoinLease,
} from '../../application/browser-share/capability-lifecycle'
import { CapabilityNavigation } from '../../application/browser-share/capability-navigation'
import type {
  BrowsePage,
  BrowserReceiverGateway,
  JoinedBrowserShare,
} from '../../application/browser-share/gateway'
import { terminalConnectionBlock } from '../../application/receive/admission'
import type { ReceiveCoordinator } from '../../application/receive/coordinator'
import { InitialJoinControl } from '../../receiver/initial-join'
import {
  cancelledReceiverSnapshot,
  joiningReceiverSnapshot,
  type SuspendedShareBrowse,
} from '../controller-state'
import { projectDraft } from '../draft/model'
import type { SelectionDraftScope } from '../draft/scope'
import type { ReceiverFailure, ReceiverSnapshot, ShareIdentity } from '../model'
import type { PreviewController } from '../preview-controller'
import type { ReceiverControllerOptions } from './contracts'
import { joinFailure } from './failures'
import type { JoinedShareObservation } from './joined-observation'
import type { BrowserNavigationCoordinator } from './navigation'
import type { ShareReportScope } from './share-report'

export interface ShareJoinCoordinatorOptions {
  readonly gateway: BrowserReceiverGateway
  readonly capabilityLifecycle: CapabilityInputLifecycle
  readonly receive: ReceiveCoordinator
  readonly browse: BrowserNavigationCoordinator
  readonly draftScope: SelectionDraftScope
  readonly joinedObservation: JoinedShareObservation
  readonly shareReport: ShareReportScope
  readonly previews: PreviewController
  readonly cancelPendingReplacement: () => void
  readonly currentJoinedShare: () => JoinedBrowserShare | undefined
  readonly setJoinedShare: (joined: JoinedBrowserShare | undefined) => void
  readonly isDisposed: () => boolean
  readonly snapshot: () => ReceiverSnapshot
  readonly setSnapshot: (snapshot: ReceiverSnapshot) => void
  readonly publish: (snapshot: ReceiverSnapshot) => void
  readonly publicError: (error: unknown) => string
  readonly onPageUrl: (pageUrl: string) => void
  readonly recordIntent: (action: string) => void
  readonly requestReconnect: () => void
  readonly beginSelectionProjection: (joined: JoinedBrowserShare) => void
  readonly openAutomaticPhotoPreview: (joined: JoinedBrowserShare, share: ShareIdentity | null, page: BrowsePage) => void
  readonly recordFailure?: ReceiverControllerOptions['recordFailure']
  readonly trace?: ReceiverControllerOptions['trace']
}

/**
 * Owns capability join, replacement, cancellation, and rollback transitions.
 * Replacement joins suspend the committed share's view while keeping its session
 * alive, so both failure and user cancellation restore it atomically.
 */
export class ShareJoinCoordinator {
  readonly #options: ShareJoinCoordinatorOptions
  readonly #capabilityNavigation: CapabilityNavigation
  #joinNavigation: AbortController | undefined
  #joinRecovery: InitialJoinControl | undefined
  #suspendedBrowse: SuspendedShareBrowse | undefined

  constructor(options: ShareJoinCoordinatorOptions) {
    this.#options = options
    this.#capabilityNavigation = new CapabilityNavigation(() => {
      const connectionKind = this.#options.snapshot().connection.kind
      return this.#joinNavigation !== undefined || connectionKind === 'connected' || connectionKind === 'reconnecting'
    })
  }

  get isJoining(): boolean {
    return this.#joinNavigation !== undefined
  }

  open(input: string, pageUrl: string): Promise<void> {
    const lease = this.#options.capabilityLifecycle.beginJoin(input, pageUrl)
    return this.#capabilityNavigation.open(capabilityInputFingerprint(input, pageUrl), async () => {
      if (this.#options.isDisposed()) return 'blocked'
      const reason = this.#options.receive.shareNavigationBlockedReason()
      if (reason !== null) {
        this.#options.publish({ ...this.#options.snapshot(), error: { kind: 'blocked', reason } })
        return 'blocked'
      }
      this.#options.onPageUrl(pageUrl)
      return await this.#joinOwned(lease, pageUrl) ? 'joined' : 'failed'
    }).then(decision => {
      if (this.#options.isDisposed()) return
      this.#options.recordIntent(`share-navigation-${decision}`)
      if (decision === 'reused' && this.#options.snapshot().connection.kind === 'reconnecting') {
        this.#options.requestReconnect()
      }
    }).finally(() => lease.release())
  }

  requestReconnect(): boolean {
    if (this.#joinRecovery === undefined) return false
    this.#joinRecovery.request('retry')
    return true
  }

  continueWaiting(): void {
    this.#joinRecovery?.request('continue')
  }

  cancel(): void {
    const navigation = this.#joinNavigation
    if (this.#options.isDisposed() || navigation === undefined) return
    const reason = new DOMException('Connection waiting cancelled', 'AbortError')
    this.#joinNavigation = undefined
    this.#joinRecovery = undefined
    this.#capabilityNavigation.clear()
    navigation.abort(reason)
    this.#options.browse.cancel(reason)
    const previous = this.#options.currentJoinedShare()
    if (previous !== undefined && this.#suspendedBrowse?.joined === previous) {
      this.#options.shareReport.clearPending()
      if (this.#restoreSuspendedShare(previous, null)) return
    }
    this.#suspendedBrowse = undefined
    if (this.#options.snapshot().share === null && previous !== undefined) {
      this.#options.setJoinedShare(undefined)
      this.#options.joinedObservation.close()
      this.#options.shareReport.clear()
      previous.close().catch(() => undefined)
    } else {
      this.#options.shareReport.clearPending()
    }
    this.#options.browse.clearCatalog()
    this.#options.draftScope.reset()
    this.#options.receive.observeConnection(null)
    this.#options.publish(cancelledReceiverSnapshot(this.#options.snapshot()))
  }

  dispose(): void {
    this.#suspendedBrowse = undefined
    this.#capabilityNavigation.clear()
    this.#joinNavigation?.abort(new DOMException('Receiver disposed', 'AbortError'))
    this.#joinNavigation = undefined
    this.#joinRecovery = undefined
  }

  async #joinOwned(
    lease: CapabilityJoinLease,
    pageUrl: string,
  ): Promise<boolean> {
    let navigation: AbortController | undefined
    let previous: JoinedBrowserShare | undefined
    let candidate: JoinedBrowserShare | undefined
    let joinedReplacementInstalled = false
    this.#traceJoin('started')
    try {
      this.#joinNavigation?.abort(new DOMException('A newer join replaced this one', 'AbortError'))
      this.#options.browse.cancel(new DOMException('A newer join replaced this one', 'AbortError'))
      this.#options.cancelPendingReplacement()
      this.#options.shareReport.clearPending()
      navigation = new AbortController()
      this.#joinNavigation = navigation
      previous = this.#options.currentJoinedShare()
      const currentSnapshot = this.#options.snapshot()
      if (previous !== undefined && currentSnapshot.share !== null) {
        // Capture committed share identity and draft mode once; a superseding replacement
        // join sees share === null and inherits this suspended state instead of overwriting it.
        this.#suspendedBrowse = Object.freeze({
          joined: previous,
          share: currentSnapshot.share,
          draftMode: currentSnapshot.draft.mode,
        })
      }
      lease.activate()
      if (previous === undefined) this.#options.receive.observeConnection(null)
      this.#options.receive.suspendForNewShare()
      await this.#options.previews.close()
      navigation.signal.throwIfAborted()
      this.#options.publish(joiningReceiverSnapshot(this.#options.snapshot()))
      this.#options.joinedObservation.suspendForJoin()
      const activeNavigation = navigation
      const recovery = new InitialJoinControl()
      this.#joinRecovery = recovery
      const joined = await lease.handoff(ownedInput =>
        this.#options.gateway.join(ownedInput, pageUrl, activeNavigation.signal, {
          control: recovery,
          onReport: report => this.#joinReplacementIsCurrent(activeNavigation) && this.#options.shareReport.adoptPending(report),
          onState: join => {
            if (!this.#joinReplacementIsCurrent(activeNavigation)) return
            this.#options.publish({ ...this.#options.snapshot(), connection: { kind: 'idle', join } })
          },
        }))
      candidate = joined
      if (!this.#joinReplacementIsCurrent(navigation)) {
        this.#traceJoin('stale_replacement')
        return false
      }
      this.#options.setJoinedShare(joined)
      this.#suspendedBrowse = undefined
      this.#options.shareReport.commit(joined.report)
      this.#options.draftScope.reset()
      this.#joinRecovery = undefined
      joinedReplacementInstalled = true
      await previous?.close().catch(() => undefined)
      if (!this.#joinReplacementIsCurrent(navigation)) return false
      this.#traceJoin('joined')
      this.#options.joinedObservation.observe(joined)
      this.#options.receive.warmCapabilities()
      await this.#openInitialBrowsePage(joined)
      return this.#joinReplacementIsCurrent(navigation)
    } catch (error) {
      this.#handleJoinFailure(error, lease, navigation, previous, joinedReplacementInstalled)
      return false
    } finally {
      this.#finishJoinAttempt(navigation, lease, candidate, joinedReplacementInstalled)
    }
  }

  async #openInitialBrowsePage(joined: JoinedBrowserShare): Promise<void> {
    const root = joined.rootDirectory()
    this.#options.browse.clearCatalog()
    await this.#options.browse.loadPage(root, 0, Object.freeze([root]))
    if (this.#options.currentJoinedShare() === joined && this.#options.browse.pageMatches(root)) {
      const share = this.#options.snapshot().share
      if (share?.kind === 'browser' && share.singleFolder) {
        this.#options.browse.openDirectory(share.homeDirectoryId)
      }
    }
  }

  #finishJoinAttempt(
    navigation: AbortController | undefined,
    lease: CapabilityJoinLease,
    candidate: JoinedBrowserShare | undefined,
    installed: boolean,
  ): void {
    if (this.#joinNavigation === navigation) {
      this.#joinNavigation = undefined
      this.#joinRecovery = undefined
    }
    lease.release()
    if (candidate !== undefined && !installed) void candidate.close().catch(() => undefined)
  }

  #joinReplacementIsCurrent(navigation: AbortController): boolean {
    return this.#joinNavigation === navigation && !navigation.signal.aborted && !this.#options.isDisposed()
  }

  #handleJoinFailure(
    error: unknown,
    lease: CapabilityJoinLease,
    navigation: AbortController | undefined,
    previous: JoinedBrowserShare | undefined,
    joinedReplacementInstalled: boolean,
  ): void {
    if (navigation === undefined || this.#joinNavigation !== navigation || navigation.signal.aborted) {
      return
    }
    this.#joinNavigation = undefined
    this.#joinRecovery = undefined
    this.#reportJoinFailure(lease.diagnosticError(error))
    this.#traceJoin('failed')
    if (!joinedReplacementInstalled && previous !== undefined && this.#options.currentJoinedShare() === previous) {
      const failure = joinFailure(error, lease.publicError(error))
      if (this.#restoreSuspendedShare(previous, failure)) return
      this.#options.joinedObservation.observe(previous)
    }
    this.#fail(error, lease)
  }

  #restoreSuspendedShare(previous: JoinedBrowserShare, error: ReceiverFailure | null): boolean {
    const suspended = this.#suspendedBrowse
    this.#suspendedBrowse = undefined
    const page = this.#options.browse.page
    const directory = this.#options.draftScope.directory
    if (suspended?.joined !== previous || page === undefined || directory === undefined) {
      return false
    }
    // Re-attach observation and offer planning before publishing the restored page so
    // subscribers transition atomically from joining back to an actionable share view.
    this.#options.joinedObservation.observe(previous)
    this.#options.beginSelectionProjection(previous)
    const draft = projectDraft(suspended.draftMode, directory, previous.selection, suspended.share)
    this.#options.setSnapshot(Object.freeze({
      ...this.#options.snapshot(),
      share: suspended.share,
      draft,
      status: null,
      error,
    }))
    this.#options.browse.publishPage(page)
    this.#options.openAutomaticPhotoPreview(previous, suspended.share, page)
    return true
  }

  #fail(error: unknown, lease?: CapabilityJoinLease): void {
    const detail = lease?.publicError(error) ?? this.#options.publicError(error)
    const failure = joinFailure(error, detail)
    let connection = this.#options.snapshot().connection
    // A failed replacement must not block downloads on the previously joined share.
    if (this.#options.currentJoinedShare() === undefined && typeof failure !== 'string' && failure.kind === 'relayAccessDenied') {
      connection = { kind: 'unavailable', reason: 'access-denied' }
      this.#options.receive.observeConnection(terminalConnectionBlock(connection))
    }
    this.#options.publish({
      ...this.#options.snapshot(),
      phase: 'failed',
      status: 'stopped',
      connection,
      error: failure,
    })
  }

  #reportJoinFailure(error: unknown): void {
    try {
      this.#options.recordFailure?.({ stage: 'join', error })
    } catch {
      // Diagnostic sinks cannot affect join control flow.
    }
  }

  #traceJoin(transition: 'started' | 'joined' | 'failed' | 'stale_replacement'): void {
    try {
      this.#options.trace?.current?.(Object.freeze({ name: 'join_transition', transition }))
    } catch {
      // Diagnostic sinks cannot affect join control flow.
    }
  }
}
