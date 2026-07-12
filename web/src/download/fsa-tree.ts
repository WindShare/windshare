import type { CanonicalPath, FileManifestEntry } from '../contracts'
import { DownloadError } from './errors'

interface CreatedNode {
  readonly parent: FileSystemDirectoryHandle
  readonly name: string
  readonly depth: number
  readonly kind: 'file' | 'directory'
}

function hasErrorName(error: unknown, name: string): boolean {
  return (
    typeof error === 'object' &&
    error !== null &&
    'name' in error &&
    (error as { readonly name?: unknown }).name === name
  )
}

function splitPath(path: CanonicalPath): readonly string[] {
  return path.split('/')
}

export class FsaFileTarget {
  readonly handle: FileSystemFileHandle

  readonly #writable: FileSystemWritableFileStream
  readonly #size: number
  readonly #ranges: Array<{ readonly start: number; readonly end: number }> = []
  #tail: Promise<void> = Promise.resolve()
  #covered = 0
  #finished = false
  #aborted = false

  constructor(
    handle: FileSystemFileHandle,
    writable: FileSystemWritableFileStream,
    size: number,
  ) {
    this.handle = handle
    this.#writable = writable
    this.#size = size
  }

  writeAt(position: number, data: Uint8Array): Promise<void> {
    const snapshot = data.slice()
    const operation = this.#tail.then(async () => {
      if (this.#finished || this.#aborted) {
        throw new DownloadError('invalid-state', 'The selected file is no longer writable')
      }
      const end = position + snapshot.byteLength
      if (!Number.isSafeInteger(end) || position < 0 || end > this.#size) {
        throw new DownloadError('invalid-layout', 'A filesystem write exceeds its selected file')
      }
      let insertion = 0
      while (
        insertion < this.#ranges.length &&
        (this.#ranges[insertion]?.start ?? Number.POSITIVE_INFINITY) < position
      ) {
        insertion += 1
      }
      const previous = this.#ranges[insertion - 1]
      const next = this.#ranges[insertion]
      if (
        (previous !== undefined && previous.end > position) ||
        (next !== undefined && next.start < end)
      ) {
        throw new DownloadError('invalid-layout', 'Selected filesystem ranges overlap')
      }
      try {
        await this.#writable.write({ type: 'write', position, data: snapshot })
      } catch (error) {
        throw new DownloadError('output-write', 'Could not write filesystem output', error)
      }
      let mergedStart = position
      let mergedEnd = end
      if (previous?.end === position) {
        mergedStart = previous.start
        this.#ranges.splice(insertion - 1, 1)
        insertion -= 1
      }
      const following = this.#ranges[insertion]
      if (following?.start === end) {
        mergedEnd = following.end
        this.#ranges.splice(insertion, 1)
      }
      this.#ranges.splice(insertion, 0, { start: mergedStart, end: mergedEnd })
      this.#covered += snapshot.byteLength
      if (this.#covered === this.#size) {
        try {
          await this.#finishWritable()
        } catch (error) {
          throw new DownloadError('output-write', 'Could not commit filesystem output', error)
        }
      }
    })
    this.#tail = operation
    return operation
  }

  ensureComplete(): Promise<void> {
    const operation = this.#tail.then(async () => {
      if (this.#covered !== this.#size) {
        throw new DownloadError('output-finalize', 'A selected filesystem file is incomplete')
      }
      await this.#finishWritable()
    })
    this.#tail = operation
    return operation
  }

  async abort(reason: unknown): Promise<void> {
    const pending = this.#tail.catch(() => undefined)
    if (this.#finished || this.#aborted) {
      await pending
      return
    }
    const aborting = Promise.resolve().then(() => this.#writable.abort(reason))
    const [, abortResult] = await Promise.allSettled([pending, aborting])
    if (abortResult?.status === 'rejected') {
      throw new DownloadError(
        'cleanup-failed',
        'Could not abort a filesystem output stream',
        abortResult.reason,
      )
    }
    this.#aborted = true
  }

  async #finishWritable(): Promise<void> {
    if (this.#finished) {
      return
    }
    await this.#writable.truncate(this.#size)
    await this.#writable.close()
    this.#finished = true
  }
}

/**
 * Lazily opens one capability-rooted handle per selected path. Creation is
 * recorded so abort can remove only nodes this transaction introduced, never a
 * pre-existing user entry or an unrelated descendant.
 */
export class FsaTree {
  readonly #directories = new Map<string, Promise<FileSystemDirectoryHandle>>()
  readonly #files = new Map<string, Promise<FsaFileTarget>>()
  readonly #created: CreatedNode[] = []
  readonly #createdDirectories = new Set<string>()

  constructor(root: FileSystemDirectoryHandle) {
    this.#directories.set('', Promise.resolve(root))
  }

  directory(path: CanonicalPath): Promise<FileSystemDirectoryHandle> {
    const key = path as string
    const cached = this.#directories.get(key)
    if (cached !== undefined) {
      return cached
    }
    const segments = splitPath(path)
    const name = segments.at(-1)
    const parentPath = segments.slice(0, -1).join('/')
    if (name === undefined || name === '') {
      return Promise.reject(
        new DownloadError('invalid-plan', 'A selected directory has no final path segment'),
      )
    }
    const promise = this.#openDirectory(parentPath, name, segments.length)
    this.#directories.set(key, promise)
    return promise
  }

  file(entry: FileManifestEntry): Promise<FsaFileTarget> {
    const key = entry.path as string
    const cached = this.#files.get(key)
    if (cached !== undefined) {
      return cached
    }
    const segments = splitPath(entry.path)
    const name = segments.at(-1)
    const parentPath = segments.slice(0, -1).join('/')
    if (name === undefined || name === '') {
      return Promise.reject(
        new DownloadError('invalid-plan', 'A selected file has no final path segment'),
      )
    }
    const promise = this.#openFile(parentPath, name, segments.length, entry.size)
    this.#files.set(key, promise)
    return promise
  }

  ownsDirectory(path: CanonicalPath): boolean {
    return this.#createdDirectories.has(path)
  }

  commit(): void {
    this.#created.length = 0
    this.#createdDirectories.clear()
  }

  async abort(reason: unknown): Promise<void> {
    const files = await Promise.allSettled(this.#files.values())
    const aborts = await Promise.allSettled(
      files
        .filter((result): result is PromiseFulfilledResult<FsaFileTarget> =>
          result.status === 'fulfilled')
        .map((result) => result.value.abort(reason)),
    )
    await Promise.allSettled(this.#directories.values())
    const failures = aborts
      .filter((result): result is PromiseRejectedResult => result.status === 'rejected')
      .map((result) => result.reason)
    try {
      await this.#removeCreatedNodes()
    } catch (error) {
      failures.push(error)
    } finally {
      this.#createdDirectories.clear()
    }
    if (failures.length > 0) {
      throw new DownloadError(
        'cleanup-failed',
        'Could not fully clean up filesystem output',
        new AggregateError(failures),
      )
    }
  }

  async #openDirectory(
    parentPath: string,
    name: string,
    depth: number,
  ): Promise<FileSystemDirectoryHandle> {
    const parent = await this.#directoryByString(parentPath)
    // FSA has no ownership journal or transactional overwrite rollback. Refusing
    // an existing file is the only safe way to make abort preserve user data.
    try {
      return await parent.getDirectoryHandle(name)
    } catch (error) {
      if (!hasErrorName(error, 'NotFoundError')) {
        throw new DownloadError('output-write', 'Could not open an output directory', error)
      }
    }
    const handle = await parent.getDirectoryHandle(name, { create: true })
    this.#created.push({ parent, name, depth, kind: 'directory' })
    this.#createdDirectories.add(parentPath === '' ? name : `${parentPath}/${name}`)
    return handle
  }

  async #openFile(
    parentPath: string,
    name: string,
    depth: number,
    size: number,
  ): Promise<FsaFileTarget> {
    const parent = await this.#directoryByString(parentPath)
    try {
      await parent.getFileHandle(name)
      throw new DownloadError(
        'output-write',
        'Refusing to overwrite an existing output file',
      )
    } catch (error) {
      if (error instanceof DownloadError) {
        throw error
      }
      if (!hasErrorName(error, 'NotFoundError')) {
        throw new DownloadError('output-write', 'Could not open an output file', error)
      }
    }
    const handle = await parent.getFileHandle(name, { create: true })
    this.#created.push({ parent, name, depth, kind: 'file' })
    try {
      const writable = await handle.createWritable({ keepExistingData: false })
      return new FsaFileTarget(handle, writable, size)
    } catch (error) {
      throw new DownloadError('output-write', 'Could not create an output file stream', error)
    }
  }

  #directoryByString(path: string): Promise<FileSystemDirectoryHandle> {
    if (path === '') {
      const root = this.#directories.get('')
      if (root === undefined) {
        throw new DownloadError('invalid-state', 'The filesystem root is unavailable')
      }
      return root
    }
    return this.directory(path as CanonicalPath)
  }

  async #removeCreatedNodes(): Promise<void> {
    const ordered = [...this.#created].sort((left, right) => {
      if (left.depth !== right.depth) {
        return right.depth - left.depth
      }
      if (left.kind === right.kind) {
        return 0
      }
      return left.kind === 'file' ? -1 : 1
    })
    const failures: unknown[] = []
    const remaining: CreatedNode[] = []
    for (const node of ordered) {
      try {
        await node.parent.removeEntry(node.name)
      } catch (error) {
        if (!hasErrorName(error, 'NotFoundError')) {
          failures.push(error)
          remaining.push(node)
        }
      }
    }
    this.#created.length = 0
    this.#created.push(...remaining)
    if (failures.length > 0) {
      throw new DownloadError(
        'cleanup-failed',
        'Could not remove every output created by the aborted transfer',
        new AggregateError(failures),
      )
    }
  }
}
