import type {
  OutputCheckpointJournal,
  OutputJournalPage,
  OutputJournalScan,
  PersistedOutputRecord,
} from '../../src/output/persistence/journal'
import {
  OUTPUT_JOURNAL_PAGE_RECORD_LIMIT,
  outputRecordKey,
  snapshotOutputRecord,
} from '../../src/output/persistence/journal'
import type {
  PersistentDirectoryMaterialization,
  PersistentOutputTree,
  PersistentTreeFile,
} from '../../src/output/persistent-tree/contracts'

interface MemoryFileNode {
  readonly kind: 'file'
  readonly identity: string
  working: Uint8Array
  durable: Uint8Array
}

interface MemoryDirectoryNode {
  readonly kind: 'directory'
  readonly identity: string
  readonly created: boolean
}

type MemoryNode = MemoryFileNode | MemoryDirectoryNode

export class MemoryOutputTree implements PersistentOutputTree {
  readonly events: string[]
  readonly fileModificationTimes = new Map<string, bigint>()
  readonly directoryModificationTimes = new Map<string, bigint>()
  readonly #nodes = new Map<string, MemoryNode>()
  #nextIdentity = 1

  authorizationError: unknown
  writeError: unknown

  constructor(events: string[] = []) {
    this.events = events
  }

  async authorize(): Promise<void> {
    this.events.push('authorize')
    if (this.authorizationError !== undefined) throw this.authorizationError
  }

  async ensureDirectory(path: readonly string[]): Promise<PersistentDirectoryMaterialization> {
    const key = pathKey(path)
    const existing = this.#nodes.get(key)
    if (existing !== undefined) {
      if (existing.kind !== 'directory') throw new Error('file occupies directory path')
      return { identity: existing.identity, created: existing.created }
    }
    const node: MemoryDirectoryNode = {
      kind: 'directory',
      identity: this.#identity(),
      created: true,
    }
    this.#nodes.set(key, node)
    return { identity: node.identity, created: true }
  }

  seedDirectory(path: readonly string[]): void {
    this.#nodes.set(pathKey(path), {
      kind: 'directory',
      identity: this.#identity(),
      created: false,
    })
  }

  async validateDirectory(path: readonly string[], identity: string): Promise<boolean> {
    const node = this.#nodes.get(pathKey(path))
    return node?.kind === 'directory' && node.identity === identity
  }

  async createFileExclusive(path: readonly string[]): Promise<PersistentTreeFile> {
    const key = pathKey(path)
    if (this.#nodes.has(key)) throw new Error('path already exists')
    const node: MemoryFileNode = {
      kind: 'file',
      identity: this.#identity(),
      working: new Uint8Array(),
      durable: new Uint8Array(),
    }
    this.#nodes.set(key, node)
    return new MemoryTreeFile(node, this)
  }

  async openFile(
    path: readonly string[],
    identity: string,
  ): Promise<PersistentTreeFile | undefined> {
    const node = this.#nodes.get(pathKey(path))
    return node?.kind === 'file' && node.identity === identity
      ? new MemoryTreeFile(node, this)
      : undefined
  }

  async removeFile(path: readonly string[], identity: string): Promise<void> {
    const key = pathKey(path)
    const node = this.#nodes.get(key)
    if (node === undefined) return
    if (node.kind !== 'file' || node.identity !== identity) {
      throw new Error('refusing to remove replacement file')
    }
    this.#nodes.delete(key)
  }

  async removeDirectory(path: readonly string[], identity: string): Promise<void> {
    const key = pathKey(path)
    const node = this.#nodes.get(key)
    if (node === undefined) return
    if (node.kind !== 'directory' || node.identity !== identity) {
      throw new Error('refusing to remove replacement directory')
    }
    const prefix = `${key}/`
    if ([...this.#nodes.keys()].some((candidate) => candidate.startsWith(prefix))) {
      throw new Error('directory is not empty')
    }
    this.#nodes.delete(key)
  }

  async setFileModificationTime(
    path: readonly string[],
    identity: string,
    milliseconds: bigint,
  ): Promise<void> {
    if (await this.openFile(path, identity) === undefined) throw new Error('file identity changed')
    this.fileModificationTimes.set(pathKey(path), milliseconds)
  }

  async setDirectoryModificationTime(
    path: readonly string[],
    identity: string,
    milliseconds: bigint,
  ): Promise<void> {
    if (!await this.validateDirectory(path, identity)) throw new Error('directory identity changed')
    this.directoryModificationTimes.set(pathKey(path), milliseconds)
  }

  replaceFile(path: readonly string[], data = Uint8Array.of(99)): void {
    const copy = data.slice()
    this.#nodes.set(pathKey(path), {
      kind: 'file',
      identity: this.#identity(),
      working: copy,
      durable: copy.slice(),
    })
  }

  has(path: readonly string[]): boolean {
    return this.#nodes.has(pathKey(path))
  }

  fileIdentity(path: readonly string[]): string | undefined {
    const node = this.#nodes.get(pathKey(path))
    return node?.kind === 'file' ? node.identity : undefined
  }

  crash(): void {
    for (const node of this.#nodes.values()) {
      if (node.kind === 'file') node.working = node.durable.slice()
    }
  }

  consumeWriteError(): unknown {
    const error = this.writeError
    this.writeError = undefined
    return error
  }

  #identity(): string {
    const identity = `memory-${this.#nextIdentity}`
    this.#nextIdentity += 1
    return identity
  }
}

class MemoryTreeFile implements PersistentTreeFile {
  readonly identity: string
  readonly #node: MemoryFileNode
  readonly #tree: MemoryOutputTree
  #dirty = false

  constructor(node: MemoryFileNode, tree: MemoryOutputTree) {
    this.identity = node.identity
    this.#node = node
    this.#tree = tree
  }

  async writeAt(offset: bigint, data: Uint8Array): Promise<void> {
    this.#tree.events.push('data-write')
    const error = this.#tree.consumeWriteError()
    if (error !== undefined) throw error
    const start = Number(offset)
    const end = start + data.byteLength
    if (!Number.isSafeInteger(start) || !Number.isSafeInteger(end)) throw new RangeError('fake offset')
    if (this.#node.working.byteLength < end) {
      const grown = new Uint8Array(end)
      grown.set(this.#node.working)
      this.#node.working = grown
    }
    this.#node.working.set(data, start)
    this.#dirty = true
  }

  async flush(): Promise<void> {
    if (!this.#dirty) return
    this.#tree.events.push('data-flush')
    this.#node.durable = this.#node.working.slice()
    this.#dirty = false
  }

  async size(): Promise<bigint> {
    return BigInt(this.#node.working.byteLength)
  }

  async close(): Promise<void> {
    await this.flush()
  }

  async read(): Promise<Blob> {
    return new Blob([this.#node.durable.slice()])
  }
}

export class MemoryOutputJournal implements OutputCheckpointJournal {
  readonly events: string[]
  readonly #committed = new Map<string, PersistedOutputRecord>()
  #candidates = new Map<string, PersistedOutputRecord>()
  #flushedCandidates = new Map<string, PersistedOutputRecord>()
  maximumScanPageRecords = 0

  constructor(events: string[] = []) {
    this.events = events
  }

  async scanCommitted(scan: OutputJournalScan): Promise<OutputJournalPage> {
    return this.#scan(this.#committed, scan)
  }

  async scanCandidates(scan: OutputJournalScan): Promise<OutputJournalPage> {
    return this.#scan(this.#flushedCandidates, scan)
  }

  async writeCandidate(record: PersistedOutputRecord): Promise<void> {
    this.events.push('journal-write')
    this.#candidates.set(outputRecordKey(record), snapshotOutputRecord(record))
  }

  async flushCandidate(key: string): Promise<void> {
    this.events.push('journal-flush')
    const record = this.#candidates.get(key)
    if (record === undefined) throw new Error('candidate missing')
    this.#flushedCandidates.set(key, snapshotOutputRecord(record))
  }

  async commitCandidate(key: string): Promise<void> {
    this.events.push('journal-commit')
    const record = this.#flushedCandidates.get(key)
    if (record === undefined) throw new Error('flushed candidate missing')
    this.#committed.set(key, snapshotOutputRecord(record))
    this.#candidates.delete(key)
    this.#flushedCandidates.delete(key)
  }

  async readCommitted(key: string): Promise<PersistedOutputRecord | undefined> {
    this.events.push('journal-reopen')
    const record = this.#committed.get(key)
    return record === undefined ? undefined : snapshotOutputRecord(record)
  }

  async discardCandidate(key: string): Promise<void> {
    this.#candidates.delete(key)
    this.#flushedCandidates.delete(key)
  }

  async deleteCommitted(key: string): Promise<void> {
    this.#committed.delete(key)
  }

  corruptCommitted(
    key: string,
    corrupt: (record: PersistedOutputRecord) => PersistedOutputRecord,
  ): void {
    const record = this.#committed.get(key)
    if (record === undefined) throw new Error('committed record missing')
    // Deliberately bypass snapshot validation to model disk/IndexedDB corruption.
    this.#committed.set(key, corrupt(record))
  }

  crash(): void {
    this.#candidates = new Map(this.#flushedCandidates)
  }

  #scan(
    source: ReadonlyMap<string, PersistedOutputRecord>,
    scan: OutputJournalScan,
  ): OutputJournalPage {
    const ordered = [...source.entries()]
      .filter(([key]) => scan.kind === undefined || key.startsWith(`${scan.kind}:`))
      .sort(([left], [right]) => compareOutputKeys(left, right))
    if (scan.direction === 'descending') ordered.reverse()
    const eligible = scan.cursor === undefined
      ? ordered
      : ordered.filter(([key]) => scan.direction === 'ascending'
        ? compareOutputKeys(key, scan.cursor!) > 0
        : compareOutputKeys(key, scan.cursor!) < 0)
    const page = eligible.slice(0, OUTPUT_JOURNAL_PAGE_RECORD_LIMIT)
    this.maximumScanPageRecords = Math.max(this.maximumScanPageRecords, page.length)
    const records = page.map(([, record]) => snapshotOutputRecord(record))
    const last = page.at(-1)
    return Object.freeze({
      records: Object.freeze(records),
      ...(page.length === OUTPUT_JOURNAL_PAGE_RECORD_LIMIT && last !== undefined
        ? { nextCursor: last[0] }
        : {}),
    })
  }
}

export function pathKey(path: readonly string[]): string {
  return path.map((segment) => encodeURIComponent(segment)).join('/')
}

function compareOutputKeys(left: string, right: string): number {
  if (left === right) return 0
  return left < right ? -1 : 1
}
