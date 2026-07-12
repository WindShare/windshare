import {
  MAX_CHUNK_COUNT,
  createChunkIndex,
  createChunkSet,
  fullChunkSet,
  type ByteLength,
  type ChunkCount,
  type ChunkIndex,
  type ChunkSet,
  type ManifestEntry,
  type Selection,
  type ValidatedManifestV1,
} from '../contracts'
import { ManifestError } from './errors'
import { deriveGeometry, type PackedGeometry } from './geometry'
import {
  compareCanonicalPaths,
  quotePathForDiagnostic,
  validateCanonicalPath,
} from './path-policy'

export interface PackedFileRange {
  readonly path: ManifestEntry['path']
  readonly offset: ByteLength
  readonly length: ByteLength
}

export interface ResolvedSelection {
  readonly selectedEntries: readonly ManifestEntry[]
  readonly chunks: ChunkSet
}

interface EntrySpan {
  readonly first: number
  readonly end: number
}

export class PackedLayout {
  readonly geometry: PackedGeometry
  readonly entries: readonly ManifestEntry[]

  readonly #starts: readonly number[]
  readonly #files: readonly number[]
  readonly #pathOrder: readonly number[]

  constructor(manifest: ValidatedManifestV1) {
    this.entries = Object.freeze([...manifest.entries])
    this.geometry = deriveGeometry(this.entries, manifest.chunkSize)

    const starts = new Array<number>(this.entries.length)
    const files: number[] = []
    const pathOrder = this.entries.map((_, index) => index)
    let cursor = 0
    for (let index = 0; index < this.entries.length; index += 1) {
      const entry = this.entries[index]
      starts[index] = cursor
      if (entry?.kind === 'file' && entry.size > 0) {
        files.push(index)
        cursor += entry.size
      }
    }
    pathOrder.sort((left, right) => {
      const leftEntry = this.entries[left]
      const rightEntry = this.entries[right]
      if (leftEntry === undefined || rightEntry === undefined) {
        return 0
      }
      return compareCanonicalPaths(leftEntry.path, rightEntry.path)
    })
    this.#starts = Object.freeze(starts)
    this.#files = Object.freeze(files)
    this.#pathOrder = Object.freeze(pathOrder)
  }

  get chunkCount(): ChunkCount {
    return this.geometry.chunkCount
  }

  resolve(selection: Selection): ResolvedSelection {
    if (selection.kind === 'all') {
      return this.#allSelection()
    }
    if (selection.paths.length === 0) {
      return this.#emptySelection()
    }

    const picked = new Array<boolean>(this.entries.length).fill(false)
    const seenSelectors = new Set<string>()
    for (const selector of selection.paths) {
      validateCanonicalPath(selector)
      if (!seenSelectors.has(selector)) {
        seenSelectors.add(selector)
        this.#markSelector(picked, selector)
      }
    }
    return this.#resolvedPickedEntries(picked)
  }

  #allSelection(): ResolvedSelection {
    return Object.freeze({
      selectedEntries: Object.freeze([...this.entries]),
      chunks: fullChunkSet(this.geometry.chunkCount),
    })
  }

  #emptySelection(): ResolvedSelection {
    return Object.freeze({
      selectedEntries: Object.freeze([]),
      chunks: createChunkSet([]),
    })
  }

  #markSelector(picked: boolean[], selector: string): void {
    const exact = this.#findEntry(selector)
    if (exact !== undefined) {
      picked[exact] = true
      if (this.entries[exact]?.kind === 'file') {
        return
      }
    }
    const matchedDescendant = this.#markDescendants(picked, `${selector}/`)
    if (exact === undefined && !matchedDescendant) {
      throw new ManifestError(
        'unknown-selector',
        `Selector ${quotePathForDiagnostic(selector)} is not present in the manifest`,
      )
    }
  }

  #markDescendants(picked: boolean[], prefix: string): boolean {
    let matched = false
    for (
      let cursor = this.#lowerBoundPath(prefix);
      cursor < this.#pathOrder.length;
      cursor += 1
    ) {
      const entryIndex = this.#pathOrder[cursor]
      const entry = entryIndex === undefined ? undefined : this.entries[entryIndex]
      if (entryIndex === undefined || entry === undefined || !entry.path.startsWith(prefix)) {
        break
      }
      matched = true
      picked[entryIndex] = true
    }
    return matched
  }

  #resolvedPickedEntries(picked: readonly boolean[]): ResolvedSelection {
    const selectedEntries: ManifestEntry[] = []
    const ranges: EntrySpan[] = []
    for (let index = 0; index < picked.length; index += 1) {
      if (!picked[index]) {
        continue
      }
      const entry = this.entries[index]
      if (entry === undefined) {
        continue
      }
      selectedEntries.push(entry)
      const span = this.#entryChunkSpan(index)
      if (span !== undefined) {
        ranges.push(span)
      }
    }
    return Object.freeze({
      selectedEntries: Object.freeze(selectedEntries),
      chunks: createChunkSet(ranges),
    })
  }

  chunkRanges(index: number | ChunkIndex): readonly PackedFileRange[] {
    if (
      !Number.isSafeInteger(index) ||
      index < 0 ||
      index >= this.geometry.chunkCount ||
      index >= MAX_CHUNK_COUNT
    ) {
      throw new ManifestError('schema-mismatch', 'Chunk index is outside this layout')
    }
    const chunkStart = index * this.geometry.chunkSize
    const chunkEnd = Math.min(
      chunkStart + this.geometry.chunkSize,
      this.geometry.streamBytes,
    )
    const firstFile = this.#firstOverlappingFile(chunkStart)
    const ranges: PackedFileRange[] = []
    for (let cursor = firstFile; cursor < this.#files.length; cursor += 1) {
      const entryIndex = this.#files[cursor]
      const entry = entryIndex === undefined ? undefined : this.entries[entryIndex]
      const fileStart = entryIndex === undefined ? undefined : this.#starts[entryIndex]
      if (entry?.kind !== 'file' || fileStart === undefined || fileStart >= chunkEnd) {
        break
      }
      const overlapStart = Math.max(chunkStart, fileStart)
      const overlapEnd = Math.min(chunkEnd, fileStart + entry.size)
      ranges.push(
        Object.freeze({
          path: entry.path,
          offset: (overlapStart - fileStart) as ByteLength,
          length: (overlapEnd - overlapStart) as ByteLength,
        }),
      )
    }
    return Object.freeze(ranges)
  }

  #firstOverlappingFile(chunkStart: number): number {
    let low = 0
    let high = this.#files.length
    while (low < high) {
      const middle = low + Math.floor((high - low) / 2)
      const entryIndex = this.#files[middle]
      const entry = entryIndex === undefined ? undefined : this.entries[entryIndex]
      const start = entryIndex === undefined ? undefined : this.#starts[entryIndex]
      const overlaps = entry?.kind === 'file' && start !== undefined && start + entry.size > chunkStart
      if (overlaps) {
        high = middle
      } else {
        low = middle + 1
      }
    }
    return low
  }

  #lowerBoundPath(path: string): number {
    let low = 0
    let high = this.#pathOrder.length
    while (low < high) {
      const middle = low + Math.floor((high - low) / 2)
      const entryIndex = this.#pathOrder[middle]
      const entry = entryIndex === undefined ? undefined : this.entries[entryIndex]
      if (entry === undefined || compareCanonicalPaths(entry.path, path) >= 0) {
        high = middle
      } else {
        low = middle + 1
      }
    }
    return low
  }

  #findEntry(path: string): number | undefined {
    const position = this.#lowerBoundPath(path)
    const entryIndex = this.#pathOrder[position]
    return entryIndex !== undefined && this.entries[entryIndex]?.path === path
      ? entryIndex
      : undefined
  }

  #entryChunkSpan(index: number): EntrySpan | undefined {
    const entry = this.entries[index]
    const start = this.#starts[index]
    if (entry?.kind !== 'file' || entry.size === 0 || start === undefined) {
      return undefined
    }
    return {
      first: createChunkIndex(Math.floor(start / this.geometry.chunkSize)),
      end: Math.floor((start + entry.size - 1) / this.geometry.chunkSize) + 1,
    }
  }
}

export function createPackedLayout(manifest: ValidatedManifestV1): PackedLayout {
  return new PackedLayout(manifest)
}
