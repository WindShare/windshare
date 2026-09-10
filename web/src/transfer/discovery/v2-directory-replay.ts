import type { DirectoryCursor, PendingFile } from '../job/contract'
import type { AsyncBoundedQueue } from '../job/scheduler'
import { isolateV2DirectoryFailure } from './v2-directory-stack'
import type { V2GenerationReplay } from './v2-generation-replay'

// Directory handles, rather than individual files, let ordinary selections finish
// discovery while content is slow. Both breadth and retained metadata remain bounded.
export const V2_MAXIMUM_PENDING_GENERATIONS = 256
export const V2_MAXIMUM_PENDING_GENERATION_METADATA_BYTES = 16n * 1024n * 1024n
const GENERATION_STRUCTURAL_METADATA_BYTES = 1024n
const TEXT_CODE_UNIT_BYTES = 2n
const STRING_STRUCTURAL_METADATA_BYTES = 64n

export interface V2DirectoryReplay {
  readonly cursor: DirectoryCursor
  readonly metadataBytes: bigint
  readonly run: (files: AsyncBoundedQueue<PendingFile>) => Promise<void>
}

export function generationReplayMetadataBytes(generation: V2GenerationReplay): bigint {
  const { cursor, committed } = generation
  const strings = [
    cursor.idText, ...cursor.path, ...cursor.ancestry,
    committed.directoryIdText, committed.generationText,
    ...generation.replayEntryIds ?? [],
  ]
  const textBytes = strings.reduce((bytes, value) =>
    bytes + BigInt(value.length) * TEXT_CODE_UNIT_BYTES + STRING_STRUCTURAL_METADATA_BYTES, 0n)
  // Lazy parent materializers can retain the ancestor chain after its queue item
  // was consumed. Conservative charging covers those handles without owning files.
  return (GENERATION_STRUCTURAL_METADATA_BYTES + textBytes) * BigInt(cursor.path.length + 1)
}

export async function runV2DirectoryReplayWorker(input: Readonly<{
  queue: AsyncBoundedQueue<V2DirectoryReplay>
  files: AsyncBoundedQueue<PendingFile>
  signal: AbortSignal
  isolateDirectory: (directoryId: string, error: unknown) => void
}>): Promise<void> {
  while (true) {
    const replay = await input.queue.pop(input.signal)
    if (replay === undefined) return
    try {
      await replay.run(input.files)
    } catch (error) {
      isolateV2DirectoryFailure(replay.cursor, error, input.isolateDirectory)
    }
  }
}
