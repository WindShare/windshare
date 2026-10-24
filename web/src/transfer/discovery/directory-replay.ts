import type { DirectoryCursor, PendingFile } from '../job/contract'
import type { AsyncBoundedQueue } from '../job/scheduler'
import { isolateDirectoryFailure } from './directory-stack'
import type { GenerationReplay } from './generation-replay'

// Directory handles, rather than individual files, let ordinary selections finish
// discovery while content is slow. Both breadth and retained metadata remain bounded.
export const MAXIMUM_PENDING_GENERATIONS = 256
export const MAXIMUM_PENDING_GENERATION_METADATA_BYTES = 16n * 1024n * 1024n
const GENERATION_STRUCTURAL_METADATA_BYTES = 1024n
const TEXT_CODE_UNIT_BYTES = 2n
const STRING_STRUCTURAL_METADATA_BYTES = 64n

export interface DirectoryReplay {
  readonly cursor: DirectoryCursor
  readonly metadataBytes: bigint
  readonly run: (files: AsyncBoundedQueue<PendingFile>) => Promise<void>
}

export function generationReplayMetadataBytes(generation: GenerationReplay): bigint {
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

export async function runDirectoryReplayWorker(input: Readonly<{
  queue: AsyncBoundedQueue<DirectoryReplay>
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
      isolateDirectoryFailure(replay.cursor, error, input.isolateDirectory)
    }
  }
}
