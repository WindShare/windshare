import { DirectoryFailureError, type CatalogScanProgress } from '../../catalog/client'
import type { CatalogEntry } from '../../catalog/records'
import { RemoteOperationError } from '../../content/session-operations'
import { equalBytes } from '../../crypto/bytes'
import type { FailureReporter } from '../../application/diagnostics/receive-trace'
import type {
  BrowseDirectory,
  BrowsePage,
  JoinedBrowserShare,
} from '../../application/browser-share/gateway'
import { breadcrumbsFor, projectBrowsePage, type RetryableBrowseRequest } from '../controller-state'
import type { ReceiverSnapshot } from '../model'

export interface BrowserNavigationCoordinatorOptions {
  readonly currentJoinedShare: () => JoinedBrowserShare | undefined
  readonly isDisposed: () => boolean
  readonly snapshot: () => ReceiverSnapshot
  readonly publish: (snapshot: ReceiverSnapshot) => void
  readonly publicError: (error: unknown) => string
  readonly onPageCommitted?: (page: BrowsePage) => void
  readonly recordFailure?: FailureReporter
}

export class BrowserNavigationCoordinator {
  readonly #options: BrowserNavigationCoordinatorOptions
  #page: BrowsePage | undefined
  #directories: BrowseDirectory[] = []
  #entries = new Map<string, CatalogEntry>()
  #navigation: AbortController | undefined
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
    this.#navigation?.abort(reason)
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
      this.#reportFailure(error, false)
      this.#options.publish({
        ...this.#options.snapshot(),
        browse: { kind: 'failed', error: this.#options.publicError(error) },
        directoryRetryable: false,
      })
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
    const navigationKey = this.#navigationKey(joined, candidateRoute, pageIndex, explicitRetry)
    if (this.#isDuplicateNavigation(navigationKey)) return

    this.#navigation?.abort(new DOMException('A newer browse request replaced this one', 'AbortError'))
    const navigation = new AbortController()
    this.#navigation = navigation
    this.#pendingNavigationKey = navigationKey
    this.#loadingDirectory = directory
    this.#options.publish({
      ...this.#options.snapshot(),
      browse: { kind: 'loading', directoryName: directory.path.at(-1) ?? null, error: null },
      directoryRetryable: false,
    })
    try {
      const page = await joined.page(directory, pageIndex, {
        signal: navigation.signal,
        explicitRetry,
      })
      navigation.signal.throwIfAborted()
      if (!this.#canPublish(navigation, joined)) return
      // Route, page, rows, and breadcrumbs publish atomically so stale pages stay invisible.
      this.#directories = [...candidateRoute]
      this.#page = page
      this.#options.onPageCommitted?.(page)
      this.#retryableBrowse = undefined
      this.publishPage(page)
    } catch (error) {
      this.#settleNavigationFailure(navigation, directory, pageIndex, candidateRoute, error)
    } finally {
      if (this.#navigation === navigation) {
        this.#loadingDirectory = undefined
        this.#pendingNavigationKey = undefined
        this.#navigation = undefined
      }
    }
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
      browse: {
        kind: 'loading',
        error: null,
        directoryName: directory.path.at(-1) ?? null,
        discoveredEntries: progress.discoveredEntries,
      },
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
    return this.#pendingNavigationKey === key && this.#navigation?.signal.aborted === false
  }

  #canPublish(navigation: AbortController, joined: JoinedBrowserShare): boolean {
    return (
      this.#navigation === navigation &&
      this.#options.currentJoinedShare() === joined &&
      !this.#options.isDisposed()
    )
  }

  #settleNavigationFailure(
    navigation: AbortController,
    directory: BrowseDirectory,
    pageIndex: number,
    route: readonly BrowseDirectory[],
    error: unknown,
  ): void {
    if (this.#navigation !== navigation || navigation.signal.aborted || this.#options.isDisposed()) {
      return
    }
    const retryable =
      (error instanceof DirectoryFailureError && error.failure.retryable) ||
      (error instanceof RemoteOperationError && error.scope === 'directory' && error.retryable)
    this.#retryableBrowse = retryable ? Object.freeze({ directory, pageIndex, route }) : undefined
    this.#reportFailure(error, retryable)
    this.#options.publish({
      ...this.#options.snapshot(),
      phase: 'browsing',
      browse: { kind: 'failed', error: this.#options.publicError(error) },
      breadcrumbs: breadcrumbsFor(this.#directories),
      directoryRetryable: retryable,
    })
  }

  #reportFailure(error: unknown, retryable: boolean): void {
    try {
      this.#options.recordFailure?.({ stage: 'browse', error, retryable })
    } catch {
      // Diagnostic reporting cannot alter visible browse results.
    }
  }
}
