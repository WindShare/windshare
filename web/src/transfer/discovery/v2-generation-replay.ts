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

export interface V2GenerationReplayConsumer<T> {
  readonly materializeSelectedDirectory: () => Promise<void>
  readonly prepare: (entry: V2CatalogEntry) => Promise<T | undefined>
}

export interface V2DirectoryDiscoveryOptions<T> {
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
  readonly generationCommitted: (committed: V2CommittedDirectory) => void
  readonly recordDirectoryFailure: (directoryId: string, error: unknown) => void
  readonly replayConsumer: (committed: V2CommittedDirectory) => V2GenerationReplayConsumer<T>
}

/**
 * Authenticates a complete committed generation before exposing output work.
 * Opaque target searches may stop early, but only selected identities from that
 * authenticated prefix are replayed, keeping memory and output authority bounded.
 */
export async function* discoverV2DirectoryGeneration<T>(
  options: V2DirectoryDiscoveryOptions<T>,
): AsyncGenerator<T, void> {
  const { cursor } = options
  if (cursor.path.length > 0 && options.opaqueSearchSatisfied()) return
  options.lifetimeSignal.throwIfAborted()
  if (cursor.path.length > V2_CATALOG_PATH_DEPTH) {
    throw new V2DirectoryTraversalError('Catalog traversal exceeded the protocol path depth')
  }
  let ignoreSatisfiedSearchFailure = !options.validateEntireGeneration
  const leave = options.traversal.enterDirectory(cursor.idText)
  try {
    const committed = await loadCommittedDirectory(options)
    if (options.opaqueSearchSatisfied()) return
    if (committed === undefined) {
      if (cursor.path.length === 0) throw new V2CatalogTraversalError('Synthetic root discovery failed')
      return
    }
    requireCommittedDirectoryAuthority(cursor, committed)
    options.observeDirectory(committed.directoryId)
    options.generationCommitted(committed)

    const consumer = options.replayConsumer(committed)
    const observation = await observeCommittedGeneration(options, committed)
    if (observation.skipReplay) return

    // Replay uses the bounded page-store handle so output failures cannot erase
    // already-authenticated discovery or require an unbounded entry accumulator.
    ignoreSatisfiedSearchFailure = false
    yield* replayCommittedGeneration(options, committed, consumer, observation.replayEntryIds)
  } catch (error) {
    if (!ignoreSatisfiedSearchFailure || !options.opaqueSearchSatisfied()) throw error
  } finally {
    leave()
  }
}

interface V2GenerationObservation {
  readonly skipReplay: boolean
  readonly replayEntryIds?: Set<string>
}

async function observeCommittedGeneration<T>(
  options: V2DirectoryDiscoveryOptions<T>,
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
    // The authenticated terminal page completes the committed cursor. Closing
    // this observation iterator avoids pulling past authority while replay owns
    // bounded output backpressure on a fresh page-store iterator.
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

function observePageEntries<T>(
  options: V2DirectoryDiscoveryOptions<T>,
  entries: readonly V2CatalogEntry[],
  selectedEntryIds: Set<string> | undefined,
): boolean {
  for (const entry of entries) {
    if (options.observeEntry(entry) && selectedEntryIds !== undefined) selectedEntryIds.add(entry.idText)
    if (options.opaqueSearchSatisfied()) return true
  }
  return false
}

async function* replayCommittedGeneration<T>(
  options: V2DirectoryDiscoveryOptions<T>,
  committed: V2CommittedDirectory,
  consumer: V2GenerationReplayConsumer<T>,
  replayEntryIds?: Set<string>,
): AsyncGenerator<T, void> {
  const { cursor } = options
  if (cursor.path.length > 0 && cursor.selected) await consumer.materializeSelectedDirectory()
  const replay = options.traversal.pageCursor(cursor, committed)
  for await (const page of options.catalog.pages(committed, options.lifetimeSignal)) {
    replay.accept(page)
    for (const entry of page.entries) {
      if (replayEntryIds !== undefined && !replayEntryIds.delete(entry.idText)) continue
      const child = await consumer.prepare(entry)
      if (child !== undefined) yield child
      if (replayEntryIds?.size === 0) return
    }
  }
  replay.finish()
}

async function loadCommittedDirectory<T>(
  options: V2DirectoryDiscoveryOptions<T>,
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
