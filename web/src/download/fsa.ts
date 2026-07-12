import type {
  CanonicalPath,
  ChunkIndex,
  FileManifestEntry,
  RandomWriteBlockSink,
  UnixMilliseconds,
} from '../contracts'
import { ChunkAvailabilityMap } from './availability'
import { DownloadError } from './errors'
import { FsaTree } from './fsa-tree'
import type { DownloadSinkContext } from './model'
import { BlockProjector } from './projector'

export interface FsaMetadataWriter {
  setFileMtime(
    handle: FileSystemFileHandle,
    mtime: UnixMilliseconds,
  ): Promise<void>
  setDirectoryMtime(
    handle: FileSystemDirectoryHandle,
    mtime: UnixMilliseconds,
  ): Promise<void>
}

type SinkState = 'open' | 'finalized' | 'aborted'
export const FSA_CLEANUP_ATTEMPTS = 3

export class FileSystemDownloadSink implements RandomWriteBlockSink {
  readonly deliveryOrder = 'any' as const

  readonly #projector: BlockProjector
  readonly #tree: FsaTree
  readonly #metadata: FsaMetadataWriter | undefined
  readonly #filesByPath: ReadonlyMap<CanonicalPath, FileManifestEntry>
  readonly #have: ChunkAvailabilityMap
  readonly #writing = new Set<number>()
  readonly #activeWrites = new Set<Promise<void>>()

  #state: SinkState = 'open'
  #cleanupTask: Promise<void> | undefined

  constructor(
    projector: BlockProjector,
    root: FileSystemDirectoryHandle,
    metadata?: FsaMetadataWriter,
  ) {
    this.#projector = projector
    this.#tree = new FsaTree(root)
    this.#metadata = metadata
    this.#filesByPath = new Map(projector.files.map((entry) => [entry.path, entry]))
    this.#have = new ChunkAvailabilityMap(projector.chunks)
  }

  has(index: ChunkIndex): boolean {
    return this.#have.has(index)
  }

  async writeBlock(index: ChunkIndex, plaintext: Uint8Array): Promise<void> {
    this.#assertOpen()
    if (this.#have.has(index) || this.#writing.has(index)) {
      throw new DownloadError('duplicate-block', 'The block was already accepted')
    }
    this.#writing.add(index)
    const operation = this.#writeProjectedBlock(index, plaintext)
    this.#activeWrites.add(operation)
    operation.then(
      () => {
        this.#activeWrites.delete(operation)
      },
      () => {
        this.#activeWrites.delete(operation)
      },
    )
    return operation
  }

  async #writeProjectedBlock(index: ChunkIndex, plaintext: Uint8Array): Promise<void> {
    try {
      const slices = this.#projector.project(index, plaintext)
      for (const slice of slices) {
        this.#assertOpen()
        const file = this.#filesByPath.get(slice.path)
        if (file === undefined) {
          throw new DownloadError('invalid-layout', 'A block references an unselected file')
        }
        const target = await this.#tree.file(file)
        this.#assertOpen()
        await target.writeAt(slice.offset, slice.data)
        this.#assertOpen()
      }
      this.#assertOpen()
      this.#have.add(index)
    } finally {
      this.#writing.delete(index)
    }
  }

  async finalize(): Promise<void> {
    this.#assertOpen()
    if (this.#writing.size !== 0) {
      throw new DownloadError('invalid-state', 'Cannot finalize while block writes are active')
    }
    try {
      const files = []
      for (const entry of this.#projector.files) {
        const target = await this.#tree.file(entry)
        await target.ensureComplete()
        files.push({ entry, target })
      }
      const directories = await Promise.all(
        this.#projector.directories.map(async (entry) => ({
          entry,
          handle: await this.#tree.directory(entry.path),
        })),
      )

      if (this.#metadata !== undefined) {
        for (const { entry, target } of files) {
          await this.#metadata.setFileMtime(target.handle, entry.mtime)
        }
        directories.sort((left, right) => {
          const depth = right.entry.path.split('/').length - left.entry.path.split('/').length
          if (depth !== 0) {
            return depth
          }
          if (left.entry.path < right.entry.path) {
            return -1
          }
          return left.entry.path > right.entry.path ? 1 : 0
        })
        for (const { entry, handle } of directories) {
          if (this.#tree.ownsDirectory(entry.path)) {
            await this.#metadata.setDirectoryMtime(handle, entry.mtime)
          }
        }
      }
      this.#tree.commit()
      this.#state = 'finalized'
    } catch (error) {
      let cause: unknown = error
      try {
        await this.abort(error)
      } catch (cleanupError) {
        cause = new AggregateError([error, cleanupError])
      }
      throw new DownloadError('output-finalize', 'Could not finalize filesystem output', cause)
    }
  }

  async abort(reason: unknown): Promise<void> {
    if (this.#state === 'finalized') {
      return
    }
    if (this.#state === 'open') {
      this.#state = 'aborted'
    }
    if (this.#cleanupTask !== undefined) {
      await this.#cleanupTask
      return
    }
    const cleanupTask = this.#cleanUpWithRetries(reason)
    this.#cleanupTask = cleanupTask
    try {
      await cleanupTask
    } catch (error) {
      // Failed removals retain their ownership records, so a caller can retry
      // without ever broadening cleanup to pre-existing user entries.
      this.#cleanupTask = undefined
      throw error
    }
  }

  async #cleanUpWithRetries(reason: unknown): Promise<void> {
    let lastFailure: unknown
    for (let attempt = 0; attempt < FSA_CLEANUP_ATTEMPTS; attempt += 1) {
      try {
        await this.#cleanUp(reason)
        return
      } catch (error) {
        lastFailure = error
      }
    }
    // Exact ownership remains recorded after exhaustion so an explicit later
    // retry can remove only this transaction's nodes without scanning user data.
    throw lastFailure
  }

  async #cleanUp(reason: unknown): Promise<void> {
    const cleanup = this.#tree.abort(reason)
    try {
      const [, cleanupResult] = await Promise.allSettled([
        Promise.allSettled([...this.#activeWrites]),
        cleanup,
      ])
      if (cleanupResult?.status === 'rejected') {
        throw cleanupResult.reason
      }
    } finally {
      this.#have.clear()
    }
  }

  #assertOpen(): void {
    if (this.#state !== 'open') {
      throw new DownloadError('invalid-state', 'The filesystem output is no longer open')
    }
  }
}

export function createFileSystemDownloadSink(
  context: DownloadSinkContext,
  root: FileSystemDirectoryHandle,
  metadata?: FsaMetadataWriter,
): FileSystemDownloadSink {
  const projector = new BlockProjector(context)
  return new FileSystemDownloadSink(projector, root, metadata)
}
