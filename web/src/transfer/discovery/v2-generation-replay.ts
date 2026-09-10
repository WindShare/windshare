import { V2_CATALOG_PATH_DEPTH } from '../../catalog/path-policy'
import type { V2CatalogClient } from '../../catalog/v2-client'
import type { V2CommittedDirectory } from '../../catalog/v2-page-store'
import { V2_CATALOG_IDENTITY_BYTES, type V2CatalogEntry } from '../../catalog/v2-records'
import { equalBytes } from '../../crypto/bytes'
import { FaultScope } from '../fault'
import {
  V2CatalogTraversalError,
  V2DirectoryTraversalError,
  type DirectoryCursor,
} from '../job/contract'
import { normalizeV2FileTransferFailure } from '../job/failures'
import type { V2CatalogTraversalGuard } from '../job/traversal'

export interface V2GenerationReplay {
  readonly cursor: DirectoryCursor
  readonly committed: V2CommittedDirectory
  readonly replayEntryIds?: ReadonlySet<string>
}

export interface V2DirectoryDiscoveryOptions {
  readonly cursor: DirectoryCursor
  readonly catalog: V2CatalogClient
  readonly traversal: V2CatalogTraversalGuard
  readonly lifetimeSignal: AbortSignal
  readonly discoverySignal: AbortSignal
  readonly validateEntireGeneration: boolean
  readonly rootCommitted?: V2CommittedDirectory
  readonly opaqueSearchSatisfied: () => boolean
  readonly observeDirectory: (directoryId: Uint8Array<ArrayBuffer>) => void
  readonly observeEntry: (entry: V2CatalogEntry) => boolean
  readonly generationCommitted: (committed: V2CommittedDirectory) => void | Promise<void>
  readonly recordDirectoryFailure: (directoryId: string, error: unknown) => void
}

/**
 * Catalog discovery owns authentication and measurement; output replay owns
 * materialization backpressure. Their shared handle refers to cached pages,
 * allowing discovery to finish without retaining every pending file in memory.
 */
export async function discoverV2DirectoryGeneration(
  options: V2DirectoryDiscoveryOptions,
): Promise<V2GenerationReplay | undefined> {
  const { cursor } = options
  if (cursor.path.length > 0 && options.opaqueSearchSatisfied()) return undefined
  options.lifetimeSignal.throwIfAborted()
  if (cursor.path.length > V2_CATALOG_PATH_DEPTH) {
    throw new V2DirectoryTraversalError('Catalog traversal exceeded the protocol path depth')
  }
  const leave = options.traversal.enterDirectory(cursor.idText)
  try {
    const committed = await loadCommittedDirectory(options)
    if (options.opaqueSearchSatisfied()) return undefined
    if (committed === undefined) {
      if (cursor.path.length === 0) throw new V2CatalogTraversalError('Synthetic root discovery failed')
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

interface V2GenerationObservation {
  readonly skipReplay: boolean
  readonly replayEntryIds?: Set<string>
}

async function observeCommittedGeneration(
  options: V2DirectoryDiscoveryOptions,
  committed: V2CommittedDirectory,
): Promise<V2GenerationObservation> {
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
  options: V2DirectoryDiscoveryOptions,
  entries: readonly V2CatalogEntry[],
  selectedEntryIds: Set<string> | undefined,
): boolean {
  for (const entry of entries) {
    if (options.observeEntry(entry) && selectedEntryIds !== undefined) selectedEntryIds.add(entry.idText)
    if (options.opaqueSearchSatisfied()) return true
  }
  return false
}

/** Each consumer validates the same immutable generation without rediscovering or recounting it. */
export async function* replayV2DirectoryEntries(
  generation: V2GenerationReplay,
  input: Readonly<{
    catalog: V2CatalogClient
    traversal: V2CatalogTraversalGuard
    signal: AbortSignal
  }>,
): AsyncGenerator<V2CatalogEntry, void> {
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
  options: V2DirectoryDiscoveryOptions,
): Promise<V2CommittedDirectory | undefined> {
  const { cursor } = options
  if (cursor.path.length === 0 && options.rootCommitted !== undefined) return options.rootCommitted
  try {
    return await options.catalog.loadDirectory(cursor.id, { signal: options.discoverySignal })
  } catch (error) {
    if (options.opaqueSearchSatisfied()) return undefined
    const normalized = normalizeV2FileTransferFailure(error)
    if (normalized.kind === 'fault' && normalized.fault.scope === FaultScope.DirectoryLocal) {
      options.recordDirectoryFailure(cursor.idText, normalized.diagnostic)
      return undefined
    }
    throw normalized.diagnostic
  }
}

function requireCommittedDirectoryAuthority(
  cursor: DirectoryCursor,
  committed: V2CommittedDirectory,
): void {
  if (!equalBytes(committed.directoryId, cursor.id) ||
      committed.generation.byteLength !== V2_CATALOG_IDENTITY_BYTES ||
      committed.generation.every((byte) => byte === 0)) {
    throw new V2CatalogTraversalError('Committed directory identity changed authenticated authority')
  }
  if (committed.omittedCount !== 0n) {
    throw new V2DirectoryTraversalError('Committed directory generation omitted catalog entries')
  }
}
