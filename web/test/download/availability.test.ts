import { describe, expect, it } from 'vitest'

import {
  MAX_CHUNK_COUNT,
  createChunkIndex,
  fullChunkSet,
} from '../../src/contracts'
import {
  ChunkAvailabilityMap,
  MAX_CHUNK_AVAILABILITY_BYTES,
} from '../../src/download/availability'

const CHUNKS_PER_PAGE = 4_096
const BYTES_PER_PAGE = CHUNKS_PER_PAGE / 8

describe('chunk availability state', () => {
  it('keeps a high sparse selection to one page', () => {
    const availability = new ChunkAvailabilityMap(fullChunkSet(MAX_CHUNK_COUNT))
    const index = createChunkIndex(MAX_CHUNK_COUNT - 1)

    availability.add(index)

    expect(availability.has(index)).toBe(true)
    expect(availability.allocatedBytes).toBe(BYTES_PER_PAGE)
  })

  it('stays inside the accepted dense-state ceiling across the full domain', () => {
    const availability = new ChunkAvailabilityMap(fullChunkSet(MAX_CHUNK_COUNT))
    for (let index = 0; index < MAX_CHUNK_COUNT; index += CHUNKS_PER_PAGE) {
      availability.add(createChunkIndex(index))
    }

    expect(availability.allocatedBytes).toBe(MAX_CHUNK_AVAILABILITY_BYTES)
    expect(MAX_CHUNK_AVAILABILITY_BYTES).toBe(8 * 1_024 * 1_024)
    availability.clear()
    expect(availability.allocatedBytes).toBe(0)
  })
})
