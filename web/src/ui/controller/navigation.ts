import { DirectoryFailureError, type CatalogScanProgress } from '../../catalog/client'
import type { CatalogEntry } from '../../catalog/records'
import { RemoteOperationError } from '../../content/session-operations'
import { equalBytes } from '../../crypto/bytes'
import { unclassifiedFailureFact } from '../../diagnostics/incident/fact'
import {
  excludedPresentationDecision,
  incidentPresentationDecision,
  type PresentationDecision,
  type PresentationExclusionReason,
} from '../../diagnostics/incident/presentation'
import type {
  IncidentScopeHandle,
  IncidentScopeOwner,
} from '../../diagnostics/incident/scope'
import type { ReceiverSnapshot } from '../model'
import { breadcrumbsFor, projectBrowsePage, type RetryableBrowseRequest } from '../controller-state'
import type {
  BrowseDirectory,
  BrowsePage,
  JoinedBrowserShare,
} from '../../application/browser-share/gateway'

interface BrowserNavigationIncidentPort {
  openScope(kind: 'browse'): IncidentScopeOwner
  submitDecision(scope: IncidentScopeHandle, decision: PresentationDecision): void
}

interface PendingNavigation {
  readonly controller: AbortController
  readonly scope?: IncidentScopeOwner
  decisionSettled: boolean
}

export interface BrowserNavigationCoordinatorOptions {
  readonly currentJoinedShare: () => JoinedBrowserShare | undefined
  readonly isDisposed: () => boolean
  readonly snapshot: () => ReceiverSnapshot
  readonly publish: (snapshot: ReceiverSnapshot) => void
  readonly publicError: (error: unknown) => string
  readonly onPageCommitted?: (page: BrowsePage) => void
  readonly incidents?: BrowserNavigationIncidentPort
}

export class BrowserNavigationCoordinator {
  readonly #options: BrowserNavigationCoordinatorOptions
  #page: BrowsePage | undefined
  #directories: BrowseDirectory[] = []
  #entries = new Map<string, CatalogEntry>()
  #navigation: PendingNavigation | undefined
  #pendingNavigationKey: string | undefined
  #loadingDirectory: BrowseDirectory | undefined
  #retryableBrowse: RetryableBrowseRequest | undefined

  constructor(options: BrowserNavigationCoordinatorOptions) {
    this.#options = options
  }

  get page(): BrowsePage | undefined {
    return this.#page
  }

  get entryCount(): number {
    return this.#entries.size
  }

  entry(id: string): CatalogEntry | undefined {
    return this.#entries.get(id)
  }

  cancel(reason: unknown): void {
    const navigation = this.#navigation
    if (navigation !== undefined) {
      navigation.controller.abort(reason)
      this.#exclude(navigation, 'cancelled')
      this.#closeScope(navigation)
    }
    this.#navigation = undefined
    this.#pendingNavigationKey = undefined
    this.#loadingDirectory = undefined
    this.#retryableBrowse = undefined
  }

  clearCatalog(): void {
    this.#page = undefined
    this.#directories = []
    this.#entries.clear()
  }

  openDirectory(id: string): void {
    const joined = this.#options.currentJoinedShare()
    const page = this.#page
    const entry = this.#entries.get(id)
    if (joined === undefined || page === undefined || entry?.kind !== 'directory') return
    let child: BrowseDirectory
    try {
      child = joined.childDirectory(page.directory, entry)
    } catch (error) {
      const attempt = this.#newAttempt()
      try {
        this.#recordFailure(attempt, false, error)
        this.#options.publish({
          ...this.#options.snapshot(),
          browse: { kind: 'failed', status: 'This directory cannot be opened safely.',
            error: this.#options.publicError(error) },
          directoryRetryable: false,
        })
      } finally {
        this.#closeScope(attempt)
      }
      return
    }
    const route = Object.freeze([...this.#directories, child])
    this.loadPage(child, 0, route).catch(() => undefined)
  }

  openBreadcrumb(index: number): void {
    const directory = this.#directories[index]
    if (directory === undefined || index === this.#directories.length - 1) return
    const route = Object.freeze(this.#directories.slice(0, index + 1))
    this.loadPage(directory, 0, route).catch(() => undefined)
  }

  showPage(index: number): void {
    const directory = this.#page?.directory
    if (directory === undefined || index < 0 || index >= this.#options.snapshot().pageCount) return
    this.loadPage(directory, index, this.#directories).catch(() => undefined)
  }

  retryDirectory(): void {
    const retry = this.#retryableBrowse
    if (retry !== undefined && this.#options.snapshot().directoryRetryable) {
      this.loadPage(retry.directory, retry.pageIndex, retry.route, true).catch(() => undefined)
    }
  }

  async loadPage(
    directory: BrowseDirectory,
    pageIndex: number,
    route: readonly BrowseDirectory[],
    explicitRetry = false,
  ): Promise<void> {
    const joined = this.#options.currentJoinedShare()
    if (joined === undefined) return
    const candidateRoute = Object.freeze([...route])
    if (candidateRoute.at(-1)?.idText !== directory.idText) {
      throw new TypeError('Browse route does not end at its requested directory')
    }
    const navigationKey = this.#navigationKey(
      joined,
      candidateRoute,
      pageIndex,
      explicitRetry,
    )
    if (this.#isDuplicateNavigation(navigationKey)) return

    this.#replacePendingNavigation()
    const navigation = this.#beginNavigation(navigationKey, directory)
    try {
      const page = await joined.page(directory, pageIndex, {
        signal: navigation.controller.signal,
        explicitRetry,
      })
      navigation.controller.signal.throwIfAborted()
      if (!this.#canPublish(navigation, joined)) {
        this.#excludeInactiveResult(navigation)
        return
      }
      // Route, page, rows, and breadcrumbs publish atomically so stale pages stay invisible.
      this.#directories = [...candidateRoute]
      this.#page = page
      this.#options.onPageCommitted?.(page)
      this.#retryableBrowse = undefined
      this.publishPage(page)
      this.#exclude(navigation, 'success')
    } catch (error) {
      this.#settleNavigationFailure(
        navigation,
        directory,
        pageIndex,
        candidateRoute,
        error,
      )
    } finally {
      this.#finishNavigation(navigation)
    }
  }

  #navigationKey(
    joined: JoinedBrowserShare,
    route: readonly BrowseDirectory[],
    pageIndex: number,
    explicitRetry: boolean,
  ): string {
    return JSON.stringify([
      joined.recoveryIdentity,
      route.map((candidate) => candidate.idText),
      pageIndex,
      explicitRetry,
    ])
  }

  #isDuplicateNavigation(key: string): boolean {
    return (
      this.#pendingNavigationKey === key &&
      this.#navigation?.controller.signal.aborted === false
    )
  }

  #replacePendingNavigation(): void {
    const replaced = this.#navigation
    if (replaced === undefined) return
    replaced.controller.abort(
      new DOMException('A newer browse request replaced this one', 'AbortError'),
    )
    this.#exclude(replaced, 'stale_replacement')
    this.#closeScope(replaced)
  }

  #beginNavigation(
    key: string,
    directory: BrowseDirectory,
  ): PendingNavigation {
    const navigation = this.#newAttempt()
    this.#navigation = navigation
    this.#pendingNavigationKey = key
    this.#loadingDirectory = directory
    try {
      this.#options.publish({
        ...this.#options.snapshot(),
        browse: { kind: 'loading', status: `Loading ${directory.name}…`, error: null },
        directoryRetryable: false,
      })
    } catch (error) {
      this.#exclude(navigation, 'not_user_visible')
      this.#closeScope(navigation)
      throw error
    }
    return navigation
  }

  #canPublish(
    navigation: PendingNavigation,
    joined: JoinedBrowserShare,
  ): boolean {
    return (
      this.#navigation === navigation &&
      this.#options.currentJoinedShare() === joined &&
      !this.#options.isDisposed()
    )
  }

  #excludeInactiveResult(navigation: PendingNavigation): void {
    this.#exclude(
      navigation,
      navigation.controller.signal.aborted || this.#options.isDisposed()
        ? 'cancelled'
        : 'stale_replacement',
    )
  }

  #settleNavigationFailure(
    navigation: PendingNavigation,
    directory: BrowseDirectory,
    pageIndex: number,
    route: readonly BrowseDirectory[],
    error: unknown,
  ): void {
    if (this.#navigation !== navigation) {
      this.#exclude(navigation, 'stale_replacement')
      return
    }
    if (
      navigation.controller.signal.aborted ||
      this.#options.isDisposed()
    ) {
      this.#exclude(navigation, 'cancelled')
      return
    }
    const retryable =
      error instanceof DirectoryFailureError && error.failure.retryable
    this.#retryableBrowse = retryable
      ? Object.freeze({ directory, pageIndex, route })
      : undefined
    this.#recordFailure(navigation, retryable, error)
    this.#options.publish({
      ...this.#options.snapshot(),
      phase: 'browsing',
      browse: { kind: 'failed', status: 'This directory could not be listed.',
        error: this.#options.publicError(error) },
      breadcrumbs: breadcrumbsFor(this.#directories),
      directoryRetryable: retryable,
    })
  }

  #finishNavigation(navigation: PendingNavigation): void {
    if (!navigation.decisionSettled) {
      this.#exclude(
        navigation,
        navigation.controller.signal.aborted ? 'cancelled' : 'not_user_visible',
      )
    }
    this.#closeScope(navigation)
    if (this.#navigation !== navigation) return
    this.#loadingDirectory = undefined
    this.#pendingNavigationKey = undefined
    this.#navigation = undefined
  }

  catalogScanProgress(joined: JoinedBrowserShare, progress: CatalogScanProgress): void {
    const directory = this.#loadingDirectory
    if (
      this.#options.currentJoinedShare() !== joined ||
      directory === undefined ||
      !equalBytes(directory.id, progress.directoryId)
    ) {
      return
    }
    this.#options.publish({
      ...this.#options.snapshot(),
      browse: { kind: 'loading', error: null,
        status: `Scanning ${directory.name}… ${progress.discoveredEntries} entries discovered; total still unknown.` },
    })
  }

  publishPage(page: BrowsePage): void {
    const joined = this.#options.currentJoinedShare()
    if (joined === undefined) return
    const projection = projectBrowsePage(page, joined.selection, this.#directories)
    this.#entries = projection.entries
    this.#options.publish({ ...this.#options.snapshot(), ...projection.snapshot })
  }

  pageMatches(directory: BrowseDirectory): boolean {
    return this.#page?.directory.idText === directory.idText
  }

  #newAttempt(): PendingNavigation {
    let scope: IncidentScopeOwner | undefined
    try {
      scope = this.#options.incidents?.openScope('browse')
    } catch {
      // Diagnostics cannot acquire authority over navigation.
    }
    return {
      controller: new AbortController(),
      ...(scope === undefined ? {} : { scope }),
      decisionSettled: false,
    }
  }

  #recordFailure(
    attempt: PendingNavigation,
    retryable: boolean,
    error?: unknown,
  ): void {
    const scope = attempt.scope
    if (scope === undefined || attempt.decisionSettled) return
    try {
      const trigger = scope.facts.record(
        error instanceof RemoteOperationError
          ? error.failureFact
          : unclassifiedFailureFact({
              stage: 'browse',
              recoveryDisposition: retryable ? 'retryable' : 'none',
            }),
        'contributor',
      )
      this.#submit(
        attempt,
        incidentPresentationDecision('browse', 'failed', trigger),
      )
    } catch {
      attempt.decisionSettled = true
      // A diagnostic classifier or sink failure cannot change the visible browse result.
    }
  }

  #exclude(
    attempt: PendingNavigation,
    reason: PresentationExclusionReason,
  ): void {
    if (attempt.scope === undefined || attempt.decisionSettled) return
    try {
      this.#submit(
        attempt,
        excludedPresentationDecision('browse', reason),
      )
    } catch {
      attempt.decisionSettled = true
      // An exclusion-construction failure cannot acquire navigation authority.
    }
  }

  #submit(attempt: PendingNavigation, decision: PresentationDecision): void {
    const scope = attempt.scope
    if (scope === undefined || attempt.decisionSettled) return
    attempt.decisionSettled = true
    try {
      this.#options.incidents?.submitDecision(scope.handle, decision)
    } catch {
      // Reporter isolation is part of the product boundary, not a caller concern.
    }
  }

  #closeScope(attempt: PendingNavigation): void {
    try {
      attempt.scope?.close()
    } catch {
      // Scope closure is observational and must not affect catalog ownership.
    }
  }
}
