import { foldPortableName15 } from '../../unicode/case-fold'
import type { ReceiveStore } from '../store/database'
import { fileRecordId, type ReceiveFileRecord } from '../store/types'
import { sameFSAEntry } from './fsa-cleanup'

export type FSAEntryOwnerKind = 'completed' | 'unfinished' | 'reclaimed'

interface CompletedOwnershipIndex {
  readonly byFoldedPath: ReadonlyMap<string, readonly ReceiveFileRecord[]>
  readonly unindexed: readonly ReceiveFileRecord[]
}

/**
 * In-session file-record and placeholder-ownership authority for an open FSA task
 * while its Web Lock is held. Tracks prior completed/unfinished rows, reclaimed
 * placeholders, and (on resumed directory sessions) same-session 0-byte commits so
 * placeholder reclamation never adopts an entry already owned by another file.
 */
export class FSAFileRecords {
  readonly completedAtOpen = new Map<string, ReceiveFileRecord>()
  readonly #invalidatedCompleted = new Map<string, string>()
  readonly #unfinished = new Map<string, ReceiveFileRecord>()
  readonly #committedEmptyByPath = new Map<string, ReceiveFileRecord[]>()
  readonly #reclaimedByPath = new Map<string, FileSystemFileHandle[]>()
  readonly #store: ReceiveStore
  readonly #trackEmptyCommits: boolean
  #completedIndex: CompletedOwnershipIndex | undefined

  constructor(store: ReceiveStore, prior: readonly ReceiveFileRecord[], trackEmptyCommits = false) {
    this.#store = store
    this.#trackEmptyCommits = trackEmptyCommits
    for (const file of prior) {
      if (file.status === 'completed') this.completedAtOpen.set(file.fileId, file)
      else this.#unfinished.set(file.id, file)
    }
  }

  find(taskId: string, fileId: string): ReceiveFileRecord | undefined {
    return this.#unfinished.get(fileRecordId(taskId, fileId)) ?? this.completedAtOpen.get(fileId)
  }

  invalidateCompleted(record: ReceiveFileRecord): boolean {
    if (this.#invalidatedCompleted.has(record.fileId)) return false
    this.#invalidatedCompleted.set(record.fileId, record.id)
    return true
  }

  isInvalidatedCompleted(file: ReceiveFileRecord): boolean {
    return file.status === 'completed' && this.#invalidatedCompleted.has(file.fileId)
  }

  async putFile(record: ReceiveFileRecord): Promise<void> {
    await this.#store.putFile(record)
    if (record.status === 'completed') {
      this.#unfinished.delete(record.id)
      this.#invalidatedCompleted.delete(record.fileId)
      // Retain only 0-byte same-session commits on resumed runs so ordinary non-empty
      // successes keep zero per-file retention while coarse-mtime 0-byte commits stay
      // protected from placeholder reclamation.
      if (this.#trackEmptyCommits && record.size === 0n) {
        upsertPathBucket(this.#committedEmptyByPath, recordPath(record), record)
      }
      return
    }
    this.#unfinished.set(record.id, record)
  }

  async deleteFiles(ids: readonly string[]): Promise<void> {
    await this.#store.deleteFiles(ids)
    const deleted = new Set(ids)
    for (const id of ids) this.#unfinished.delete(id)
    for (const [fileId, id] of this.#invalidatedCompleted) {
      if (deleted.has(id)) {
        this.#invalidatedCompleted.delete(fileId)
        this.completedAtOpen.delete(fileId)
        this.#completedIndex = undefined
      }
    }
  }

  recordReclaimedPlaceholder(handle: FileSystemFileHandle, physicalPath: readonly string[]): void {
    const key = foldedPathKey(physicalPath)
    const bucket = this.#reclaimedByPath.get(key)
    if (bucket === undefined) this.#reclaimedByPath.set(key, [handle])
    else bucket.push(handle)
  }

  /**
   * Returns the owning category when `handle` / `physicalPath` already belongs to a
   * reclaimed placeholder, unfinished row, or completed record of this task.
   *
   * Built lazily and bucketed by Unicode-15 folded path (matching Chromium's path-based
   * `isSameEntry` while preserving case-sensitive FS accuracy via `sameFSAEntry`) so
   * ordinary downloads pay zero cost and resumes never pay O(N) IPC over unrelated files.
   */
  async findOwnerKind(
    handle: FileSystemFileHandle,
    physicalPath: readonly string[],
  ): Promise<FSAEntryOwnerKind | undefined> {
    const key = foldedPathKey(physicalPath)
    if (await matchesAnyHandle(this.#reclaimedByPath.get(key), handle)) return 'reclaimed'
    for (const row of this.#unfinished.values()) {
      if (await matchesOwnedRecord(row, handle, physicalPath)) return 'unfinished'
    }
    const index = this.#ensureCompletedIndex()
    if (await matchesAnyRecord(index.byFoldedPath.get(key), handle, physicalPath)) return 'completed'
    if (await matchesAnyRecord(this.#committedEmptyByPath.get(key), handle, physicalPath)) return 'completed'
    if (await matchesAnyRecord(index.unindexed, handle, physicalPath)) return 'completed'
    return undefined
  }

  #ensureCompletedIndex(): CompletedOwnershipIndex {
    if (this.#completedIndex !== undefined) return this.#completedIndex
    const byFoldedPath = new Map<string, ReceiveFileRecord[]>()
    const unindexed: ReceiveFileRecord[] = []
    for (const row of this.completedAtOpen.values()) {
      if (row.fsaPath === undefined && row.handle !== undefined) {
        unindexed.push(row)
      } else {
        upsertPathBucket(byFoldedPath, recordPath(row), row)
      }
    }
    this.#completedIndex = { byFoldedPath, unindexed }
    return this.#completedIndex
  }
}

function recordPath(record: ReceiveFileRecord): readonly string[] {
  return record.fsaPath ?? record.path
}

function foldedPathKey(path: readonly string[]): string {
  return JSON.stringify(path.map(foldPortableName15))
}

function sameExactPath(left: readonly string[], right: readonly string[]): boolean {
  if (left.length !== right.length) return false
  for (let i = 0; i < left.length; i += 1) {
    if (left[i] !== right[i]) return false
  }
  return true
}

function upsertPathBucket(
  buckets: Map<string, ReceiveFileRecord[]>,
  path: readonly string[],
  record: ReceiveFileRecord,
): void {
  const key = foldedPathKey(path)
  const existing = buckets.get(key)
  if (existing === undefined) {
    buckets.set(key, [record])
    return
  }
  const index = existing.findIndex(entry => entry.id === record.id)
  if (index >= 0) existing[index] = record
  else existing.push(record)
}

async function matchesAnyHandle(
  handles: readonly FileSystemFileHandle[] | undefined,
  handle: FileSystemFileHandle,
): Promise<boolean> {
  if (handles === undefined) return false
  for (const prior of handles) {
    if (await sameFSAEntry(prior, handle)) return true
  }
  return false
}

async function matchesAnyRecord(
  records: readonly ReceiveFileRecord[] | undefined,
  handle: FileSystemFileHandle,
  physicalPath: readonly string[],
): Promise<boolean> {
  if (records === undefined) return false
  for (const row of records) {
    if (await matchesOwnedRecord(row, handle, physicalPath)) return true
  }
  return false
}

async function matchesOwnedRecord(
  row: ReceiveFileRecord,
  handle: FileSystemFileHandle,
  physicalPath: readonly string[],
): Promise<boolean> {
  if (row.handle !== undefined) return sameFSAEntry(row.handle, handle)
  return sameExactPath(recordPath(row), physicalPath)
}
