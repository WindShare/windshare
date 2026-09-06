import { V2DirectoryFailureError, type V2CatalogClient } from '../../catalog/v2-client'
import type { V2CatalogEntry, V2ShareDescriptor } from '../../catalog/v2-records'
import type { V2FrozenSelectionPolicy } from '../../catalog/v2-selection'
import { snapshotPortableCatalogPath } from '../../catalog/path-policy'
import { projectAuthenticatedV2Generation } from '../../transfer/discovery/v2-projection-evidence'
import {
  RetryableProjectionDiscoveryError,
  createAuthenticatedProjectionEvidence,
  type AuthenticatedDiscoveryRequest,
  type AuthenticatedDiscoverySource,
  type AuthenticatedProjectionEvidence,
} from '../../transfer/projection'
import { ProjectionDiscoverySummary } from '../v2-projection-summary'
import { ProjectionDemand } from './demand'

// One folder and one descendant provide useful lower bounds without a recursive summary scan.
export const DRAFT_PREFETCH_DIRECTORY_LIMIT = 2

interface ProjectionDirectoryCursor {
  readonly id: Uint8Array<ArrayBuffer>
  readonly idText: string
  readonly path: readonly string[]
  readonly ancestry: readonly string[]
  readonly selected: boolean
  readonly selectedDirectoryRoot?: Readonly<{
    directoryId: string
    sourcePath: string
  }>
}

export class V2JoinedProjectionSource implements AuthenticatedDiscoverySource {
  readonly #descriptor: V2ShareDescriptor
  readonly #catalog: V2CatalogClient
  readonly #selection: V2FrozenSelectionPolicy
  readonly #protocolSessionId: () => string
  readonly #capturedProtocolSessionId: string
  readonly #explicitRetry: boolean
  readonly #demand: ProjectionDemand
  #prefetchedDirectories = 0
  #bounded = false

  constructor(options: {
    readonly descriptor: V2ShareDescriptor
    readonly catalog: V2CatalogClient
    readonly selection: V2FrozenSelectionPolicy
    readonly protocolSessionId: () => string
    readonly explicitRetry: boolean
  }) {
    this.#descriptor = options.descriptor
    this.#catalog = options.catalog
    this.#selection = options.selection
    this.#demand = new ProjectionDemand(options.selection)
    this.#protocolSessionId = options.protocolSessionId
    this.#capturedProtocolSessionId = options.protocolSessionId()
    this.#explicitRetry = options.explicitRetry
  }

  async *discover(request: AuthenticatedDiscoveryRequest) {
    request.signal.throwIfAborted()
    this.#requireSameProtocolSession()
    const rootSelected = this.#selection.directorySelected(
      this.#descriptor.syntheticRootId,
      [],
    )
    if (!this.#selection.defaultSelected && !this.#selection.canonicalRules.some(rule => rule.selected)) {
      return Object.freeze({ kind: 'complete' as const })
    }
    const summary = new ProjectionDiscoverySummary()
    const seen = new Set<string>()
    const root: ProjectionDirectoryCursor = Object.freeze({
      id: this.#descriptor.syntheticRoot.slice(),
      idText: this.#descriptor.syntheticRootId,
      path: Object.freeze([]),
      ancestry: Object.freeze([this.#descriptor.syntheticRootId]),
      selected: rootSelected,
    })
    yield* this.#discoverDirectory(root, request, summary, seen)
    request.signal.throwIfAborted()
    this.#requireSameProtocolSession()
    if (!this.#demand.settled) {
      throw new TypeError('Selection target path was not found in the authenticated catalog')
    }
    if (this.#bounded) return Object.freeze({ kind: 'bounded' as const })
    const layoutBasis = summary.layoutBasis()
    const workspaceCostObservation = summary.workspaceCostObservation(layoutBasis)
    // Committed generation evidence owns target settlement. Completion only
    // closes discovery with the cross-generation layout proof; replaying the
    // request here would claim targets whose authority was already consumed.
    return Object.freeze({
      kind: 'complete' as const,
      ...(layoutBasis === undefined ? {} : { layoutBasis }),
      ...(workspaceCostObservation === undefined
        ? {}
        : { workspaceCostObservation }),
    })
  }

  async *#discoverDirectory(
    cursor: ProjectionDirectoryCursor,
    request: AuthenticatedDiscoveryRequest,
    summary: ProjectionDiscoverySummary,
    seen: Set<string>,
  ): AsyncGenerator<AuthenticatedProjectionEvidence, void> {
    request.signal.throwIfAborted()
    this.#requireSameProtocolSession()
    if (seen.has(cursor.idText)) {
      throw new TypeError('Catalog projection contains a repeated directory identity')
    }
    seen.add(cursor.idText)

    const committed = await this.#loadCommittedDirectory(cursor, request)
    const evidence = await this.#projectDirectory(committed, cursor, request, summary)
    summary.observe(evidence)
    const layoutBasis = this.#demand.settled ? summary.layoutBasis() : undefined
    yield layoutBasis === undefined ? evidence : createAuthenticatedProjectionEvidence({
      ...evidence, earlyLayoutBasis: layoutBasis,
    })

    for await (const child of this.#discoverableChildren(committed, cursor, request, summary)) {
      const required = this.#demand.needsDirectory(child.ancestry)
      if (!required && !child.selected) continue
      if (!required && this.#prefetchedDirectories >= DRAFT_PREFETCH_DIRECTORY_LIMIT) {
        this.#bounded = true
        continue
      }
      if (!required) this.#prefetchedDirectories += 1
      yield* this.#discoverDirectory(child, request, summary, seen)
    }
  }

  async #loadCommittedDirectory(
    cursor: ProjectionDirectoryCursor,
    request: AuthenticatedDiscoveryRequest,
  ) {
    try {
      const committed = await this.#catalog.loadDirectory(cursor.id, {
        signal: request.signal,
        explicitRetry: this.#explicitRetry,
      })
      request.signal.throwIfAborted()
      this.#requireSameProtocolSession()
      return committed
    } catch (error) {
      if (error instanceof V2DirectoryFailureError && error.failure.retryable) {
        throw new RetryableProjectionDiscoveryError('catalog-temporarily-unavailable', {
          cause: error,
        })
      }
      throw error
    }
  }

  #projectDirectory(
    committed: Awaited<ReturnType<V2CatalogClient['loadDirectory']>>,
    cursor: ProjectionDirectoryCursor,
    request: AuthenticatedDiscoveryRequest,
    summary: ProjectionDiscoverySummary,
  ): Promise<AuthenticatedProjectionEvidence> {
    return projectAuthenticatedV2Generation({
      committed,
      pages: this.#authenticatedPages(committed, cursor, request, summary),
      selection: this.#selection,
      directoryAncestry: cursor.ancestry,
      directoryPath: cursor.path,
      containingDirectorySelected: cursor.selected,
      unsettledTargets: request.unsettledTargets,
      signal: request.signal,
    })
  }

  async *#authenticatedPages(
    committed: Awaited<ReturnType<V2CatalogClient['loadDirectory']>>,
    cursor: ProjectionDirectoryCursor,
    request: AuthenticatedDiscoveryRequest,
    summary: ProjectionDiscoverySummary,
  ) {
    for await (const page of this.#catalog.pages(committed, request.signal)) {
      for (const entry of page.entries) {
        this.#demand.observe(entry, cursor.ancestry)
        if (cursor.selectedDirectoryRoot !== undefined &&
            !this.#selection.selected(entry, cursor.ancestry)) {
          summary.markDirectoryRootPartial(cursor.selectedDirectoryRoot.directoryId)
        }
      }
      yield page
    }
  }

  async *#discoverableChildren(
    committed: Awaited<ReturnType<V2CatalogClient['loadDirectory']>>,
    cursor: ProjectionDirectoryCursor,
    request: AuthenticatedDiscoveryRequest,
    summary: ProjectionDiscoverySummary,
  ): AsyncGenerator<ProjectionDirectoryCursor, void> {
    for await (const page of this.#catalog.pages(committed, request.signal)) {
      for (const entry of page.entries) {
        request.signal.throwIfAborted()
        const selected = this.#selection.selected(entry, cursor.ancestry)
        summary.observeCatalogEntry([...cursor.path, entry.name], entry, selected)
        const child = this.#projectionChild(cursor, entry, summary, selected)
        if (child !== undefined) yield child
      }
    }
  }

  #projectionChild(
    cursor: ProjectionDirectoryCursor,
    entry: V2CatalogEntry,
    summary: ProjectionDiscoverySummary,
    selected: boolean,
  ): ProjectionDirectoryCursor | undefined {
    if (cursor.selectedDirectoryRoot !== undefined && !selected) {
      summary.markDirectoryRootPartial(cursor.selectedDirectoryRoot.directoryId)
    }
    if (entry.kind !== 'directory') return undefined
    const ancestry = [...cursor.ancestry, entry.idText]
    if (!selected && !this.#demand.needsDirectory(ancestry)) return undefined

    const path = snapshotPortableCatalogPath([...cursor.path, entry.name])
    const selectedDirectoryRoot = projectionDirectoryRoot(cursor, entry.idText, path, selected)
    return Object.freeze({
      id: entry.id.slice(),
      idText: entry.idText,
      path,
      ancestry: Object.freeze([...cursor.ancestry, entry.idText]),
      selected,
      ...(selectedDirectoryRoot === undefined ? {} : { selectedDirectoryRoot }),
    })
  }

  #requireSameProtocolSession(): void {
    if (this.#protocolSessionId() !== this.#capturedProtocolSessionId) {
      throw new RetryableProjectionDiscoveryError('receiver-reconnecting')
    }
  }
}

function projectionDirectoryRoot(
  cursor: ProjectionDirectoryCursor,
  directoryId: string,
  path: readonly string[],
  selected: boolean,
): ProjectionDirectoryCursor['selectedDirectoryRoot'] {
  if (!selected) return undefined
  if (cursor.selectedDirectoryRoot !== undefined) return cursor.selectedDirectoryRoot
  if (cursor.selected && cursor.path.length !== 0) return undefined
  return Object.freeze({ directoryId, sourcePath: path.join('/') })
}
