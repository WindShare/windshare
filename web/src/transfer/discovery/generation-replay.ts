import { CATALOG_PATH_DEPTH } from '../../catalog/path-policy'
import type { CatalogClient } from '../../catalog/client'
import type { CommittedDirectory } from '../../catalog/page-store'
import { CATALOG_IDENTITY_BYTES, type CatalogEntry } from '../../catalog/records'
import { equalBytes } from '../../crypto/bytes'
import { FaultScope } from '../failure/fault'
import {
  CatalogTraversalError,
  DirectoryTraversalError,
  type DirectoryCursor,
} from '../job/contract'
import { normalizeFileTransferFailure } from '../job/failures'
import type { CatalogTraversalGuard } from '../job/traversal'

export interface GenerationReplay {
  readonly cursor: DirectoryCursor
  readonly committed: CommittedDirectory
  readonly replayEntryIds?: ReadonlySet<string>
}

export interface DirectoryDiscoveryOptions {
  readonly cursor: DirectoryCursor
  readonly catalog: CatalogClient
  readonly traversal: CatalogTraversalGuard
  readonly lifetimeSignal: AbortSignal
  readonly discoverySignal: AbortSignal
  readonly validateEntireGeneration: boolean
  readonly rootCommitted?: CommittedDirectory
  readonly opaqueSearchSatisfied: () => boolean
  readonly observeDirectory: (directoryId: Uint8Array<ArrayBuffer>) => void
  readonly observeEntry: (entry: CatalogEntry) => boolean
  readonly generationCommitted: (committed: CommittedDirectory) => void | Promise<void>
  readonly recordDirectoryFailure: (directoryId: string, error: unknown) => void
}

/**
 * Catalog discovery owns authentication and measurement; output replay owns
 * materialization backpressure. Their shared handle refers to cached pages,
 * allowing discovery to finish without retaining every pending file in memory.
 */
export async function discoverDirectoryGeneration(
  options: DirectoryDiscoveryOptions,
): Promise<GenerationReplay | undefined> {
  const { cursor } = options
  if (cursor.path.length > 0 && options.opaqueSearchSatisfied()) return undefined
  options.lifetimeSignal.throwIfAborted()
  if (cursor.path.length > CATALOG_PATH_DEPTH) {
    throw new DirectoryTraversalError('Catalog traversal exceeded the protocol path depth')
  }
  const leave = options.traversal.enterDirectory(cursor.idText)
  try {
    const committed = await loadCommittedDirectory(options)
    if (options.opaqueSearchSatisfied()) return undefined
    if (committed === undefined) {
      if (cursor.path.length === 0) throw new CatalogTraversalError('Synthetic root discovery failed')
      return undefined
    }
    requireCommittedDirectoryAuthority(cursor, committed)
    options.observeDirectory(committed.directoryId)
    await options.generationCommitted(committed)
    const observation = await observeCommittedGeneration(options, committed)
    if (observation.skipReplay) return undefined
    return Object.freeze({
      cursor,
      committed,
      ...(observation.replayEntryIds === undefined ? {} : { replayEntryIds: observation.replayEntryIds }),
    })
  } catch (error) {
    if (options.validateEntireGeneration || !options.opaqueSearchSatisfied()) throw error
    return undefined
  } finally {
    leave()
  }
}

interface GenerationObservation {
  readonly skipReplay: boolean
  readonly replayEntryIds?: Set<string>
}

async function observeCommittedGeneration(
  options: DirectoryDiscoveryOptions,
  committed: CommittedDirectory,
): Promise<GenerationObservation> {
  const selectedEntryIds = options.validateEntireGeneration ? undefined : new Set<string>()
  const pages = options.traversal.pageCursor(options.cursor, committed)
  let stoppedAfterTargets = false
  for await (const page of options.catalog.pages(committed, options.discoverySignal)) {
    pages.accept(page)
    if (observePageEntries(options, page.entries, selectedEntryIds)) {
      stoppedAfterTargets = true
      break
    }
    if (page.terminal) break
  }
  if (!stoppedAfterTargets) pages.finish()
  const replayEntryIds = stoppedAfterTargets ? selectedEntryIds : undefined
  if (replayEntryIds?.size === 0) return Object.freeze({ skipReplay: true })
  return Object.freeze({
    skipReplay: false,
    ...(replayEntryIds === undefined ? {} : { replayEntryIds }),
  })
}

function observePageEntries(
  options: DirectoryDiscoveryOptions,
  entries: readonly CatalogEntry[],
  selectedEntryIds: Set<string> | undefined,
): boolean {
  for (const entry of entries) {
    if (options.observeEntry(entry) && selectedEntryIds !== undefined) selectedEntryIds.add(entry.idText)
    if (options.opaqueSearchSatisfied()) return true
  }
  return false
}

/** Each consumer validates the same immutable generation without rediscovering or recounting it. */
export async function* replayDirectoryEntries(
  generation: GenerationReplay,
  input: Readonly<{
    catalog: CatalogClient
    traversal: CatalogTraversalGuard
    signal: AbortSignal
  }>,
): AsyncGenerator<CatalogEntry, void> {
  const { cursor, committed } = generation
  const replayEntryIds = generation.replayEntryIds === undefined
    ? undefined : new Set(generation.replayEntryIds)
  const replay = input.traversal.pageCursor(cursor, committed)
  for await (const page of input.catalog.pages(committed, input.signal)) {
    replay.accept(page)
    for (const entry of page.entries) {
      if (replayEntryIds !== undefined && !replayEntryIds.delete(entry.idText)) continue
      yield entry
      if (replayEntryIds?.size === 0) return
    }
  }
  replay.finish()
}

async function loadCommittedDirectory(
  options: DirectoryDiscoveryOptions,
): Promise<CommittedDirectory | undefined> {
  const { cursor } = options
  if (cursor.path.length === 0 && options.rootCommitted !== undefined) return options.rootCommitted
  try {
    return await options.catalog.loadDirectory(cursor.id, { signal: options.discoverySignal })
  } catch (error) {
    if (options.opaqueSearchSatisfied()) return undefined
    const normalized = normalizeFileTransferFailure(error)
    if (normalized.kind === 'fault' && normalized.fault.scope === FaultScope.DirectoryLocal) {
      options.recordDirectoryFailure(cursor.idText, normalized.diagnostic)
      return undefined
    }
    throw normalized.diagnostic
  }
}

function requireCommittedDirectoryAuthority(
  cursor: DirectoryCursor,
  committed: CommittedDirectory,
): void {
  if (!equalBytes(committed.directoryId, cursor.id) ||
      committed.generation.byteLength !== CATALOG_IDENTITY_BYTES ||
      committed.generation.every((byte) => byte === 0)) {
    throw new CatalogTraversalError('Committed directory identity changed authenticated authority')
  }
  if (cursor.path.length === 0 && committed.omittedCount !== 0n) {
    throw new CatalogTraversalError('Synthetic root omitted explicitly selected entries')
  }
}
