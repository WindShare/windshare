import type { PerformanceCheckpointCost } from '../diagnostics'
import {
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  newFileCheckpointV2,
  type FileCheckpointPhase,
  type FileCheckpointV2,
} from '../persistence/checkpoint'
import type { PersistentByteRange, PreservingWriterCost } from './contracts'

export function nextCheckpoint(
  previous: FileCheckpointV2,
  ranges: readonly PersistentByteRange[],
  phase: FileCheckpointPhase,
  forceCheckpointGeneration: boolean,
): FileCheckpointV2 {
  const rangesChanged = !sameRanges(ranges, previous.verifiedRanges)
  return newFileCheckpointV2({
    ...previous,
    stateGeneration: previous.stateGeneration + 1n,
    checkpointGeneration: previous.checkpointGeneration +
      (rangesChanged || forceCheckpointGeneration ? 1n : 0n),
    verifiedRanges: ranges,
    phase,
    commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
  })
}

export function writtenBoundary(ranges: readonly PersistentByteRange[]): bigint {
  return ranges.at(-1)?.end ?? 0n
}

export function rangeBytes(ranges: readonly PersistentByteRange[]): bigint {
  return ranges.reduce((total, range) => total + (range.end - range.start), 0n)
}

export function durableByteAdvance(previous: bigint, current: bigint): bigint {
  return current > previous ? current - previous : 0n
}

export function checkpointPerformanceCost(cost: PreservingWriterCost): PerformanceCheckpointCost {
  if (cost.temporaryBytes > 0n) return 'space_preflight'
  if (cost.prefixCopyBytes > 0n || cost.writeAmplificationBytes > 0n) {
    return 'prefix_copy'
  }
  return 'constant'
}

export function sameRanges(
  left: readonly PersistentByteRange[],
  right: readonly PersistentByteRange[],
): boolean {
  return left.length === right.length && left.every((range, index) =>
    range.start === right[index]?.start && range.end === right[index]?.end)
}

export function zeroPreservingWriterCost(): PreservingWriterCost {
  return Object.freeze({
    prefixCopyBytes: 0n,
    writeAmplificationBytes: 0n,
    temporaryBytes: 0n,
  })
}

export function throwIfAborted(signal: AbortSignal | undefined): void {
  signal?.throwIfAborted()
}
