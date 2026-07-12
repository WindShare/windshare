import {
  MAX_CHUNK_COUNT,
  chunkSetHas,
  type ChunkIndex,
  type ChunkSet,
} from '../contracts'
import { DownloadError } from './errors'

const BITS_PER_BYTE = 8
const CHUNKS_PER_PAGE = 4_096
const BYTES_PER_PAGE = CHUNKS_PER_PAGE / BITS_PER_BYTE
export const MAX_CHUNK_AVAILABILITY_BYTES = MAX_CHUNK_COUNT / BITS_PER_BYTE

/**
 * A paged bitset keeps resume state sparse for high-index selections while the
 * complete protocol domain remains bounded by the accepted 8 MiB dense ceiling.
 */
export class ChunkAvailabilityMap {
  readonly #selection: ChunkSet
  readonly #pages = new Map<number, Uint8Array>()

  constructor(selection: ChunkSet) {
    this.#selection = selection
  }

  get allocatedBytes(): number {
    return this.#pages.size * BYTES_PER_PAGE
  }

  has(index: ChunkIndex): boolean {
    if (!chunkSetHas(this.#selection, index)) {
      return false
    }
    const page = this.#pages.get(Math.floor(index / CHUNKS_PER_PAGE))
    if (page === undefined) {
      return false
    }
    const withinPage = index % CHUNKS_PER_PAGE
    const value = page[Math.floor(withinPage / BITS_PER_BYTE)]
    return value !== undefined && (value & (1 << (withinPage % BITS_PER_BYTE))) !== 0
  }

  add(index: ChunkIndex): void {
    if (!chunkSetHas(this.#selection, index)) {
      throw new DownloadError('block-not-selected', 'The block is not part of this transfer plan')
    }
    const pageIndex = Math.floor(index / CHUNKS_PER_PAGE)
    let page = this.#pages.get(pageIndex)
    if (page === undefined) {
      page = new Uint8Array(BYTES_PER_PAGE)
      this.#pages.set(pageIndex, page)
    }
    const withinPage = index % CHUNKS_PER_PAGE
    const byteIndex = Math.floor(withinPage / BITS_PER_BYTE)
    const value = page[byteIndex]
    if (value === undefined) {
      throw new DownloadError('invalid-state', 'Chunk availability page is inconsistent')
    }
    page[byteIndex] = value | (1 << (withinPage % BITS_PER_BYTE))
  }

  clear(): void {
    this.#pages.clear()
  }
}
