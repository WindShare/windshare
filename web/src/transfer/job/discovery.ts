import type { CatalogClient } from '../../catalog/client'
import type { CommittedDirectory } from '../../catalog/page-store'
import type { CatalogEntry } from '../../catalog/records'
import type { FrozenSelectionPolicy } from '../../catalog/selection'
import { discoverDirectoryGeneration, replayDirectoryEntries } from '../discovery/generation-replay'
import { generationReplayMetadataBytes, type DirectoryReplay } from '../discovery/directory-replay'
import type {
  AuthenticatedDirectory,
  AuthenticatedLogicalSiblingMembership,
  DirectoryCursor,
  DirectoryWork,
  PendingFile,
  TransferJobOptions,
} from './contract'
import type { DirectTreeFileProjection } from './coordinate/direct-tree'
import type { ExactPreparationCollector } from './preparation'
import { AsyncBoundedQueue } from './scheduler'
import type { ExplicitSelectionTargetLedger } from './selection'
import type { CatalogTraversalGuard } from './traversal'

/** Retains the raw committed handle so membership cannot be reconstructed from text projections. */
export function createAuthenticatedLogicalSiblingMembership(
  catalog: Pick<CatalogClient, 'hasCommittedName'>,
  committed: CommittedDirectory,
  signal: AbortSignal,
): AuthenticatedLogicalSiblingMembership {
  return Object.freeze({
    directoryId: committed.directoryIdText,
    generation: committed.generationText,
    hasCommittedName: (candidate: string) => catalog.hasCommittedName(committed, candidate, signal),
  })
}

export class JobDiscovery {
  readonly #catalog: TransferJobOptions['catalog']
  readonly #selection: FrozenSelectionPolicy
  readonly #traversal: CatalogTraversalGuard
  readonly #explicitTargets: ExplicitSelectionTargetLedger
  readonly #signal: AbortSignal
  readonly #rootCommitted: () => CommittedDirectory | undefined
  readonly #observeSelectedFile: (entry: Extract<CatalogEntry, { kind: 'file' }>) => void
  readonly #observeSourceOmissions: (count: bigint) => void
  readonly #recordDirectoryFailure: (identity: string, error: unknown) => void
  readonly #authenticateDirectory: (
    cursor: DirectoryCursor,
    committed: CommittedDirectory,
    parent?: AuthenticatedDirectory,
  ) => Promise<AuthenticatedDirectory>
  readonly #projectFile: (sourcePath: readonly string[]) => DirectTreeFileProjection
  readonly #generationCommitted: ((cursor: DirectoryCursor, committed: CommittedDirectory) => Promise<void>) | undefined
  readonly #prepareDirectory: (
    collector: ExactPreparationCollector,
    cursor: DirectoryCursor,
    committed: CommittedDirectory,
    role: 'selected' | 'ancestor',
  ) => AuthenticatedDirectory

  constructor(input: {
    readonly catalog: TransferJobOptions['catalog']
    readonly selection: FrozenSelectionPolicy
    readonly traversal: CatalogTraversalGuard
    readonly explicitTargets: ExplicitSelectionTargetLedger
    readonly signal: AbortSignal
    readonly rootCommitted: () => CommittedDirectory | undefined
    readonly observeSelectedFile: (entry: Extract<CatalogEntry, { kind: 'file' }>) => void
    readonly observeSourceOmissions: (count: bigint) => void
    readonly recordDirectoryFailure: (identity: string, error: unknown) => void
    readonly authenticateDirectory: (
      cursor: DirectoryCursor,
      committed: CommittedDirectory,
      parent?: AuthenticatedDirectory,
    ) => Promise<AuthenticatedDirectory>
    readonly projectFile: (sourcePath: readonly string[]) => DirectTreeFileProjection
    readonly generationCommitted?: (cursor: DirectoryCursor, committed: CommittedDirectory) => Promise<void>
    readonly prepareDirectory: (
      collector: ExactPreparationCollector,
      cursor: DirectoryCursor,
      committed: CommittedDirectory,
      role: 'selected' | 'ancestor',
    ) => AuthenticatedDirectory
  }) {
    this.#catalog = input.catalog
    this.#selection = input.selection
    this.#traversal = input.traversal
    this.#explicitTargets = input.explicitTargets
    this.#signal = input.signal
    this.#rootCommitted = input.rootCommitted
    this.#observeSelectedFile = input.observeSelectedFile
    this.#observeSourceOmissions = input.observeSourceOmissions
    this.#recordDirectoryFailure = input.recordDirectoryFailure
    this.#authenticateDirectory = input.authenticateDirectory
    this.#projectFile = input.projectFile
    this.#generationCommitted = input.generationCommitted
    this.#prepareDirectory = input.prepareDirectory
  }

  async *discoverDirectory(
    work: DirectoryWork,
    replays: AsyncBoundedQueue<DirectoryReplay>,
    collector?: ExactPreparationCollector,
  ): AsyncGenerator<DirectoryWork, void> {
    const { cursor } = work
    const validateEntireGeneration = cursor.selected ||
      (cursor.path.length === 0 && !this.#explicitTargets.hasPendingTargets)
    const discoverySignal = this.#explicitTargets.discoverySignal(validateEntireGeneration)
    const rootCommitted = this.#rootCommitted()
    const generation = await discoverDirectoryGeneration({
      cursor,
      catalog: this.#catalog,
      traversal: this.#traversal,
      lifetimeSignal: this.#signal,
      discoverySignal,
      validateEntireGeneration,
      ...(rootCommitted === undefined ? {} : { rootCommitted }),
      opaqueSearchSatisfied: () => this.#explicitTargets.opaqueSearchSatisfied(validateEntireGeneration),
      observeDirectory: (identity) => this.#explicitTargets.observeDirectory(identity),
      observeEntry: (entry) => this.#observeCatalogEntry(cursor, entry),
      generationCommitted: async committed => {
        await this.#generationCommitted?.(cursor, committed)
        collector?.observeGeneration(cursor, committed)
      },
      recordDirectoryFailure: this.#recordDirectoryFailure,
    })
    if (generation === undefined) return
    if (cursor.selected && generation.committed.omittedCount > 0n) {
      this.#observeSourceOmissions(generation.committed.omittedCount)
    }
    const materialize = this.#directoryMaterializer(work, cursor, generation.committed, collector)
    const replayInput = { catalog: this.#catalog, traversal: this.#traversal, signal: this.#signal }
    await replays.push({
      cursor,
      metadataBytes: generationReplayMetadataBytes(generation),
      run: async files => {
        if (cursor.path.length > 0 && cursor.selected) await materialize('selected')
        for await (const entry of replayDirectoryEntries(generation, replayInput)) {
          if (entry.kind === 'file') await this.#prepareFile(cursor, materialize, entry, files, collector)
        }
      },
    }, this.#signal)
    // Child discovery reads the committed pages independently of output admission.
    // A full file queue can hold back replay without withholding the selection total.
    for await (const entry of replayDirectoryEntries(generation, replayInput)) {
      if (entry.kind !== 'directory') continue
      const child = this.#prepareChildDirectory(cursor, materialize, entry)
      if (child !== undefined) yield child
    }
  }

  #observeCatalogEntry(cursor: DirectoryCursor, entry: CatalogEntry): boolean {
    this.#traversal.entryPath(cursor, entry)
    this.#traversal.claimNode(entry.id)
    this.#explicitTargets.observe(entry)
    const selected = this.#selection.selected(entry, cursor.ancestry)
    if (entry.kind === 'file' && selected) this.#observeSelectedFile(entry)
    return selected
  }

  async #prepareFile(
    cursor: DirectoryCursor,
    materialize: (role?: 'selected' | 'ancestor') => Promise<AuthenticatedDirectory>,
    entry: Extract<CatalogEntry, { kind: 'file' }>,
    files: AsyncBoundedQueue<PendingFile>,
    collector?: ExactPreparationCollector,
  ): Promise<void> {
    this.#signal.throwIfAborted()
    if (!this.#selection.selected(entry, cursor.ancestry)) return
    const parent = await materialize('ancestor')
    const projection = this.#projectFile(this.#traversal.entryPath(cursor, entry))
    if (collector !== undefined) {
      collector.addFile(entry, projection.sourceAuthenticationPath, projection.logicalArtifactPath, parent)
    } else {
      await this.#enqueueFile(entry, projection, parent, files)
    }
  }

  #prepareChildDirectory(
    cursor: DirectoryCursor,
    materialize: (role?: 'selected' | 'ancestor') => Promise<AuthenticatedDirectory>,
    entry: Extract<CatalogEntry, { kind: 'directory' }>,
  ): DirectoryWork | undefined {
    const sourcePath = this.#traversal.entryPath(cursor, entry)
    const selected = this.#selection.selected(entry, cursor.ancestry)
    if (!selected && !this.#explicitTargets.hasPendingTargets) return undefined
    if (!this.#selection.shouldDiscover(entry.idText, cursor.ancestry)) return undefined
    return {
      cursor: {
        id: entry.id.slice(),
        idText: entry.idText,
        path: sourcePath,
        ancestry: Object.freeze([...cursor.ancestry, entry.idText]),
        selected,
        ...(entry.modifiedTime === undefined ? {} : { modifiedTime: entry.modifiedTime }),
      },
      materializeParent: materialize,
    }
  }

  async #enqueueFile(
    entry: Extract<CatalogEntry, { kind: 'file' }>,
    projection: DirectTreeFileProjection,
    parent: AuthenticatedDirectory,
    files: AsyncBoundedQueue<PendingFile>,
  ): Promise<void> {
    let release: () => void = () => undefined
    const ready = new Promise<void>((resolve) => { release = resolve })
    try {
      await files.push(Object.freeze({
        entry,
        sourceAuthenticationPath: projection.sourceAuthenticationPath,
        logicalArtifactPath: projection.logicalArtifactPath,
        materializationRelativePath: projection.relativePath,
        parent,
        ready,
        ...(entry.modifiedTime === undefined ? {} : { modifiedTime: entry.modifiedTime }),
      }), this.#signal)
    } finally {
      release()
    }
  }

  #directoryMaterializer(
    work: DirectoryWork,
    cursor: DirectoryCursor,
    committed: CommittedDirectory,
    collector?: ExactPreparationCollector,
  ): (role?: 'selected' | 'ancestor') => Promise<AuthenticatedDirectory> {
    if (cursor.path.length === 0) return work.materializeParent
    let materialized: Promise<AuthenticatedDirectory> | undefined
    let initialRole: 'selected' | 'ancestor' | undefined
    return async (role = 'ancestor') => {
      if (materialized === undefined) {
        initialRole = role
        materialized = (async () => {
          const parent = await work.materializeParent('ancestor')
          return collector === undefined
            ? this.#authenticateDirectory(cursor, committed, parent)
            : this.#prepareDirectory(collector, cursor, committed, role)
        })()
      }
      const result = await materialized
      if (collector !== undefined && initialRole === 'selected' && role === 'ancestor') {
        return this.#prepareDirectory(collector, cursor, committed, role)
      }
      return result
    }
  }
}
