import { bench, describe } from 'vitest'
import { fullChunkSet } from '../../src/contracts/selection'
import type { ChunkAvailability } from '../../src/contracts/sink'
import { CompactDemand } from '../../src/session/demand'
import { REQUEST_WINDOW_BLOCKS } from '../../src/session/model'

const KIB = 1024
const MIB = 1024 * KIB
const FIXTURE_BYTES = 64 * MIB
const BENCHMARK_TIME_MS = 2_000
const WARMUP_TIME_MS = 1_000
const CHUNK_BYTES = [KIB, 64 * KIB, MIB, 4 * MIB] as const
const NOTHING_AVAILABLE: ChunkAvailability = Object.freeze({
  has: () => false,
})

export let schedulerChecksum = 0

// A fixed transfer size makes each case schedule identical useful bytes while
// varying only the number of chunks that CompactDemand must traverse.
describe('CompactDemand request-window scheduling', () => {
  for (const chunkBytes of CHUNK_BYTES) {
    const chunkCount = FIXTURE_BYTES / chunkBytes
    bench(
      `chunk_${chunkBytes / KIB}KiB_${chunkCount}blocks`,
      () => {
        const demand = new CompactDemand(
          fullChunkSet(chunkCount),
          NOTHING_AVAILABLE,
          false,
        )
        demand.start()

        let scheduled = 0
        let requestWindows = 0
        while (!demand.exhausted) {
          const indices = demand.take(REQUEST_WINDOW_BLOCKS, () => false)
          scheduled += indices.length
          if (indices.length > 0) {
            requestWindows += 1
          }
        }
        if (scheduled !== chunkCount) {
          throw new Error(`scheduled ${scheduled} blocks, want ${chunkCount}`)
        }

        // Keep the completed traversal observable to the JavaScript engine.
        schedulerChecksum ^= scheduled + requestWindows
      },
      {
        time: BENCHMARK_TIME_MS,
        warmupTime: WARMUP_TIME_MS,
      },
    )
  }
})

