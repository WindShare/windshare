import type { RecoveryCostSnapshot } from './recovery-cost'
import type { BrowserStagingStorageFacts } from './staging-storage'

export const UNKNOWN_SPEED_STAGING_THRESHOLD_BYTES = 256n * 1024n ** 2n
export const SMALL_FILE_DIRECT_MAXIMUM_BYTES = 16n * 1024n ** 2n
export const LONG_RECEIVE_THRESHOLD_MILLISECONDS = 30_000n
export const UNOBSERVED_LOCAL_COPY_BYTES_PER_SECOND = 64n * 1024n ** 2n
const MILLISECONDS_PER_SECOND = 1_000n
const RECOVERY_VALIDATION_AND_COPY_PASSES = 2n
const EXPECTED_LOST_RECEIVE_DIVISOR = 2n

export type FileReceivingPlacement = 'direct' | 'staged'
export type FileReceivingPlacementReason =
  | 'retained-placement' | 'explicit-direct' | 'opfs-unavailable' | 'storage-pressure'
  | 'small-file' | 'unknown-speed-large-file' | 'unknown-speed-small-file'
  | 'long-receive-benefits-recovery' | 'local-recovery-cost-exceeds-receive-benefit'
  | 'short-receive'

export interface FileReceivingPlacementDecision {
  readonly placement: FileReceivingPlacement
  readonly reason: FileReceivingPlacementReason
  readonly estimatedReceiveMilliseconds: bigint | null
  readonly estimatedLocalRecoveryMilliseconds: bigint | null
}

/** Called at authenticated revision open; directory totals cannot decide any member's placement. */
export function decideFileReceivingPlacement(input: Readonly<{
  exactSize: bigint
  preference: 'automatic' | 'direct'
  storage: BrowserStagingStorageFacts
  costs?: RecoveryCostSnapshot
  retainedPlacement?: FileReceivingPlacement
}>): FileReceivingPlacementDecision {
  if (typeof input.exactSize !== 'bigint' || input.exactSize < 0n ||
      input.exactSize > 0xffff_ffff_ffff_ffffn) throw new RangeError('Exact file size is not a u64')
  const receiveRate = positiveRate(input.costs?.receivedBytesPerSecond)
  const copyRate = positiveRate(input.costs?.copiedBytesPerSecond) ?? UNOBSERVED_LOCAL_COPY_BYTES_PER_SECOND
  const receive = receiveRate === null ? null : duration(input.exactSize, receiveRate)
  const flush = input.costs?.flushMilliseconds
  const localRecovery = duration(input.exactSize, copyRate) * RECOVERY_VALIDATION_AND_COPY_PASSES +
    BigInt(flush !== undefined && flush !== null && Number.isFinite(flush) && flush > 0 ? Math.ceil(flush) : 0)
  const decision = (placement: FileReceivingPlacement, reason: FileReceivingPlacementReason) =>
    Object.freeze({ placement, reason, estimatedReceiveMilliseconds: receive,
      estimatedLocalRecoveryMilliseconds: localRecovery })
  if (input.retainedPlacement !== undefined) return decision(input.retainedPlacement, 'retained-placement')
  if (input.preference === 'direct') return decision('direct', 'explicit-direct')
  if (input.storage.opfs === 'unavailable') return decision('direct', 'opfs-unavailable')
  if (input.storage.pressure === 'drain-first') return decision('direct', 'storage-pressure')
  if (input.exactSize <= SMALL_FILE_DIRECT_MAXIMUM_BYTES) return decision('direct', 'small-file')
  if (receive === null) return input.exactSize >= UNKNOWN_SPEED_STAGING_THRESHOLD_BYTES
    ? decision('staged', 'unknown-speed-large-file') : decision('direct', 'unknown-speed-small-file')
  if (receive < LONG_RECEIVE_THRESHOLD_MILLISECONDS) return decision('direct', 'short-receive')
  return receive / EXPECTED_LOST_RECEIVE_DIVISOR > localRecovery
    ? decision('staged', 'long-receive-benefits-recovery')
    : decision('direct', 'local-recovery-cost-exceeds-receive-benefit')
}

function positiveRate(value: bigint | null | undefined): bigint | null {
  return value !== undefined && value !== null && value > 0n ? value : null
}

function duration(bytes: bigint, bytesPerSecond: bigint): bigint {
  return (bytes * MILLISECONDS_PER_SECOND + bytesPerSecond - 1n) / bytesPerSecond
}
