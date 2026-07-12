import {
  createChunkIndex,
  createChunkSet,
  type ByteLength,
  type CanonicalPath,
  type ChunkIndex,
  type DirectoryManifestEntry,
  type FileManifestEntry,
  type ManifestEntry,
  type PlanId,
  type TransferPlan,
  type UnixMilliseconds,
} from '../../src/contracts'
import type {
  BlockFileRange,
  BlockLayout,
  CanonicalPathValidator,
  DownloadSinkContext,
} from '../../src/download'

const FIXTURE_MTIME = Date.UTC(2025, 0, 2, 3, 4, 6) as UnixMilliseconds

export function canonical(path: string): CanonicalPath {
  return path as CanonicalPath
}

export function file(
  path: string,
  size: number,
  mtime: UnixMilliseconds = FIXTURE_MTIME,
): FileManifestEntry {
  return Object.freeze({
    kind: 'file',
    path: canonical(path),
    size: size as ByteLength,
    mtime,
  })
}

export function directory(
  path: string,
  mtime: UnixMilliseconds = FIXTURE_MTIME,
): DirectoryManifestEntry {
  return Object.freeze({ kind: 'directory', path: canonical(path), mtime })
}

export function range(
  path: string,
  offset: number,
  length: number,
): BlockFileRange {
  return Object.freeze({
    path: canonical(path),
    offset: offset as ByteLength,
    length: length as ByteLength,
  })
}

export const validateFixturePath: CanonicalPathValidator = (path) => {
  if (
    path === '' ||
    path.startsWith('/') ||
    path.endsWith('/') ||
    path.split('/').some((segment) => segment === '' || segment === '.' || segment === '..')
  ) {
    throw new Error('fixture path policy rejected the path')
  }
  return canonical(path)
}

export function fixtureContext(
  entries: readonly ManifestEntry[],
  ranges: ReadonlyMap<number, readonly BlockFileRange[]>,
  validatePath: CanonicalPathValidator = validateFixturePath,
  selectedBytes = entries.reduce(
    (total, entry) => total + (entry.kind === 'file' ? entry.size : 0),
    0,
  ),
): DownloadSinkContext {
  const indices = [...ranges.keys()].sort((left, right) => left - right)
  const plan: TransferPlan = Object.freeze({
    planId: new Uint8Array(32) as PlanId,
    selectedEntries: Object.freeze([...entries]),
    selectedBytes: selectedBytes as ByteLength,
    chunks: createChunkSet(indices.map((index) => ({ first: index, end: index + 1 }))),
  })
  const layout: BlockLayout = {
    chunkRanges(index: ChunkIndex): readonly BlockFileRange[] {
      return ranges.get(index) ?? Object.freeze([])
    },
  }
  return { plan, layout, validatePath }
}

export function chunk(index: number): ChunkIndex {
  return createChunkIndex(index)
}

export function concat(chunks: readonly Uint8Array[]): Uint8Array {
  const length = chunks.reduce((total, value) => total + value.byteLength, 0)
  const output = new Uint8Array(length)
  let offset = 0
  for (const value of chunks) {
    output.set(value, offset)
    offset += value.byteLength
  }
  return output
}

export interface RecordingOutput {
  stream: WritableStream<Uint8Array>
  readonly chunks: Uint8Array[]
  closed: boolean
  aborted: unknown
}

export function recordingOutput(): RecordingOutput {
  const output: RecordingOutput = {
    chunks: [],
    closed: false,
    aborted: undefined,
    stream: undefined as unknown as WritableStream<Uint8Array>,
  }
  output.stream = new WritableStream<Uint8Array>({
    write(value) {
      output.chunks.push(value.slice())
    },
    close() {
      output.closed = true
    },
    abort(reason) {
      output.aborted = reason
    },
  })
  return output
}
