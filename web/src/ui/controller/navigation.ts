import { V2DirectoryFailureError, type V2CatalogScanProgress } from '../../catalog/v2-client'
import type { V2CatalogEntry } from '../../catalog/v2-records'
import { equalBytes } from '../../crypto/bytes'
import type { V2ReceiverSnapshot } from '../v2-model'
import { breadcrumbsFor, projectBrowsePage, type RetryableV2BrowseRequest } from '../v2-controller-state'
import type {
  V2BrowseDirectory,
  V2BrowsePage,
  V2JoinedBrowserShare,
} from '../v2-gateway'

export interface BrowserNavigationCoordinatorOptions {
  readonly currentJoinedShare: () => V2JoinedBrowserShare | undefined
  readonly isDisposed: () => boolean
  readonly snapshot: () => V2ReceiverSnapshot
  readonly publish: (snapshot: V2ReceiverSnapshot) => void
  readonly publicError: (error: unknown) => string
}

export class BrowserNavigationCoordinator {
  readonly #options: BrowserNavigationCoordinatorOptions
  #page: V2BrowsePage | undefined
  #directories: V2BrowseDirectory[] = []
  #entries = new Map<string, V2CatalogEntry>()
  #navigation: AbortController | undefined
  #pendingNavigationKey: string | undefined
  #loadingDirectory: V2BrowseDirectory | undefined
  #retryableBrowse: RetryableV2BrowseRequest | undefined

  constructor(options: BrowserNavigationCoordinatorOptions) {
    this.#options = options
  }

  get page(): V2BrowsePage | undefined {
    return this.#page
  }

  get entryCount(): number {
    return this.#entries.size
  }

  entry(id: string): V2CatalogEntry | undefined {
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
    let child: V2BrowseDirectory
    try {
      child = joined.childDirectory(page.directory, entry)
    } catch (error) {
      this.#options.publish({
        ...this.#options.snapshot(),
        phase: 'browsing',
        status: 'This directory cannot be opened safely.',
        error: this.#options.publicError(error),
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
    directory: V2BrowseDirectory,
    pageIndex: number,
    route: readonly V2BrowseDirectory[],
    explicitRetry = false,
  ): Promise<void> {
    const joined = this.#options.currentJoinedShare()
    if (joined === undefined) return
    const candidateRoute = Object.freeze([...route])
    if (candidateRoute.at(-1)?.idText !== directory.idText) {
      throw new TypeError('Browse route does not end at its requested directory')
    }
    const navigationKey = JSON.stringify([
      joined.recoveryIdentity,
      candidateRoute.map((candidate) => candidate.idText),
      pageIndex,
      explicitRetry,
    ])
    if (this.#pendingNavigationKey === navigationKey &&
        this.#navigation?.signal.aborted === false) return
    this.#navigation?.abort(new DOMException('A newer browse request replaced this one', 'AbortError'))
    const navigation = new AbortController()
    this.#navigation = navigation
    this.#pendingNavigationKey = navigationKey
    this.#loadingDirectory = directory
    this.#options.publish({
      ...this.#options.snapshot(),
      phase: 'joining',
      status: `Loading ${directory.name}…`,
      error: null,
      directoryRetryable: false,
    })
    try {
      const page = await joined.page(directory, pageIndex, {
        signal: navigation.signal,
        explicitRetry,
      })
      navigation.signal.throwIfAborted()
      if (this.#navigation !== navigation ||
          this.#options.currentJoinedShare() !== joined || this.#options.isDisposed()) return
      // Route, page, rows, and breadcrumbs publish atomically so stale pages stay invisible.
      this.#directories = [...candidateRoute]
      this.#page = page
      this.#retryableBrowse = undefined
      this.publishPage(page)
    } catch (error) {
      if (this.#navigation !== navigation || navigation.signal.aborted) return
      const retryable = error instanceof V2DirectoryFailureError && error.failure.retryable
      this.#retryableBrowse = retryable
        ? Object.freeze({ directory, pageIndex, route: candidateRoute })
        : undefined
      this.#options.publish({
        ...this.#options.snapshot(),
        phase: 'browsing',
        status: 'This directory could not be listed.',
        error: this.#options.publicError(error),
        breadcrumbs: breadcrumbsFor(this.#directories),
        directoryRetryable: retryable,
      })
    } finally {
      if (this.#navigation === navigation) {
        this.#loadingDirectory = undefined
        this.#pendingNavigationKey = undefined
        this.#navigation = undefined
      }
    }
  }

  catalogScanProgress(joined: V2JoinedBrowserShare, progress: V2CatalogScanProgress): void {
    const directory = this.#loadingDirectory
    if (this.#options.currentJoinedShare() !== joined || directory === undefined ||
        !equalBytes(directory.id, progress.directoryId)) return
    this.#options.publish({
      ...this.#options.snapshot(),
      status: `Scanning ${directory.name}… ${progress.discoveredEntries} entries discovered; total still unknown.`,
    })
  }

  publishPage(page: V2BrowsePage): void {
    const joined = this.#options.currentJoinedShare()
    if (joined === undefined) return
    const projection = projectBrowsePage(page, joined.selection, this.#directories)
    this.#entries = projection.entries
    this.#options.publish({ ...this.#options.snapshot(), ...projection.snapshot })
  }

  pageMatches(directory: V2BrowseDirectory): boolean {
    return this.#page?.directory.idText === directory.idText
  }
}
