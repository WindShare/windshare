import {
  MAX_MTIME_MILLISECONDS,
  MAX_STREAM_BYTES,
  MIN_MTIME_MILLISECONDS,
  chunkSetHas,
  type ByteLength,
  type CanonicalPath,
  type ChunkIndex,
  type ChunkSet,
  type DirectoryManifestEntry,
  type FileManifestEntry,
  type ManifestEntry,
} from '../contracts'
import { DownloadError } from './errors'
import type { BlockFileRange, DownloadSinkContext } from './model'

export interface SelectedBlockSlice {
  readonly path: CanonicalPath
  readonly offset: ByteLength
  readonly data: Uint8Array
}

function checkedInteger(
  value: number,
  minimum: number,
  maximum: number,
  message: string,
): number {
  if (!Number.isSafeInteger(value) || value < minimum || value > maximum) {
    throw new DownloadError('invalid-plan', message)
  }
  return value
}

function validateEntry(
  entry: unknown,
  context: DownloadSinkContext,
): asserts entry is ManifestEntry {
  if (typeof entry !== 'object' || entry === null) {
    throw new DownloadError('invalid-plan', 'The transfer plan contains an invalid entry')
  }
  const candidate = entry as {
    readonly kind?: unknown
    readonly path?: unknown
    readonly mtime?: unknown
    readonly size?: unknown
  }
  if (typeof candidate.path !== 'string') {
    throw new DownloadError('invalid-plan', 'A selected entry has an invalid path')
  }
  const validatedPath = context.validatePath(candidate.path)
  if (validatedPath !== candidate.path) {
    throw new DownloadError(
      'invalid-plan',
      'The injected path policy must validate canonical paths without rewriting them',
    )
  }
  checkedInteger(
    candidate.mtime as number,
    MIN_MTIME_MILLISECONDS,
    MAX_MTIME_MILLISECONDS,
    'A selected entry has an invalid modification time',
  )
  if (candidate.kind === 'file') {
    checkedInteger(
      candidate.size as number,
      0,
      MAX_STREAM_BYTES,
      'A selected file has an invalid byte length',
    )
    return
  }
  if (candidate.kind !== 'directory') {
    throw new DownloadError('invalid-plan', 'The transfer plan contains an unknown entry kind')
  }
}

function validateRange(
  range: unknown,
  plaintextBytes: number,
): asserts range is BlockFileRange {
  if (typeof range !== 'object' || range === null) {
    throw new DownloadError('invalid-layout', 'The block layout returned an invalid file range')
  }
  const candidate = range as {
    readonly path?: unknown
    readonly offset?: unknown
    readonly length?: unknown
  }
  if (
    typeof candidate.path !== 'string' ||
    !Number.isSafeInteger(candidate.offset) ||
    (candidate.offset as number) < 0 ||
    !Number.isSafeInteger(candidate.length) ||
    (candidate.length as number) <= 0 ||
    (candidate.length as number) > plaintextBytes
  ) {
    throw new DownloadError('invalid-layout', 'The block layout returned an invalid file range')
  }
}

/**
 * Converts complete authenticated blocks into selected file-local slices. The
 * cursor advances across unselected siblings too, so a shared boundary block
 * can never shift selected bytes or materialize a skipped file.
 */
export class BlockProjector {
  readonly entries: readonly ManifestEntry[]
  readonly files: readonly FileManifestEntry[]
  readonly directories: readonly DirectoryManifestEntry[]
  readonly chunks: ChunkSet

  readonly #context: DownloadSinkContext
  readonly #selectedFiles = new Map<string, FileManifestEntry>()

  constructor(context: DownloadSinkContext) {
    this.#context = context
    const entries = [...context.plan.selectedEntries]
    const files: FileManifestEntry[] = []
    const directories: DirectoryManifestEntry[] = []
    const seen = new Set<string>()
    let selectedBytes = 0

    for (const entry of entries) {
      validateEntry(entry, context)
      if (seen.has(entry.path)) {
        throw new DownloadError('invalid-plan', 'The transfer plan contains a duplicate path')
      }
      seen.add(entry.path)
      if (entry.kind === 'file') {
        selectedBytes += entry.size
        if (!Number.isSafeInteger(selectedBytes) || selectedBytes > MAX_STREAM_BYTES) {
          throw new DownloadError('invalid-plan', 'Selected file bytes exceed the protocol limit')
        }
        files.push(entry)
        this.#selectedFiles.set(entry.path, entry)
      } else if (entry.kind === 'directory') {
        directories.push(entry)
      } else {
        throw new DownloadError('invalid-plan', 'The transfer plan contains an unknown entry kind')
      }
    }
    if (selectedBytes !== context.plan.selectedBytes) {
      throw new DownloadError(
        'invalid-plan',
        'Selected byte count does not match the selected manifest entries',
      )
    }
    this.entries = Object.freeze(entries)
    this.files = Object.freeze(files)
    this.directories = Object.freeze(directories)
    this.chunks = context.plan.chunks
  }

  project(index: ChunkIndex, plaintext: Uint8Array): readonly SelectedBlockSlice[] {
    if (!chunkSetHas(this.#context.plan.chunks, index)) {
      throw new DownloadError('block-not-selected', 'The block is not part of this transfer plan')
    }
    const ranges = this.#context.layout.chunkRanges(index)
    if (!Array.isArray(ranges)) {
      throw new DownloadError('invalid-layout', 'The block layout did not return a range array')
    }
    const selected: SelectedBlockSlice[] = []
    let cursor = 0

    for (const range of ranges) {
      validateRange(range, plaintext.byteLength)
      const next = cursor + range.length
      if (!Number.isSafeInteger(next) || next > plaintext.byteLength) {
        throw new DownloadError(
          'invalid-layout',
          'File ranges exceed the authenticated block length',
        )
      }
      const file = this.#selectedFiles.get(range.path)
      if (file !== undefined) {
        const rangeEnd = range.offset + range.length
        if (!Number.isSafeInteger(rangeEnd) || rangeEnd > file.size) {
          throw new DownloadError(
            'invalid-layout',
            'A selected file range exceeds its authenticated file size',
          )
        }
        selected.push(
          Object.freeze({
            path: file.path,
            offset: range.offset,
            data: plaintext.slice(cursor, next),
          }),
        )
      }
      cursor = next
    }

    if (cursor !== plaintext.byteLength) {
      throw new DownloadError(
        'invalid-layout',
        'File ranges do not cover the complete authenticated block',
      )
    }
    if (selected.length === 0) {
      throw new DownloadError(
        'invalid-layout',
        'A selected block does not contain bytes for any selected file',
      )
    }
    return Object.freeze(selected)
  }
}
