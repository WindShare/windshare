export const AUTOMATIC_CHECKPOINT_PENDING_FLOOR_BYTES = 64n * 1024n * 1024n

export interface CheckpointScheduleInput {
  readonly durablePrefixBytes: bigint
  readonly pendingBytes: bigint
  readonly remainingBytes: bigint
}

export type CheckpointScheduleDecision =
  | Readonly<{
      readonly kind: 'wait-for-progress'
      readonly nextPendingBytes: bigint
    }>
  | Readonly<{ readonly kind: 'checkpoint-now' }>
  | Readonly<{ readonly kind: 'finish-without-further-checkpoint' }>

export interface CheckpointSchedule {
  evaluate(input: CheckpointScheduleInput): CheckpointScheduleDecision
}

export const checkpointSchedule = createCheckpointSchedule()

export function createCheckpointSchedule(
  pendingFloorBytes: bigint = AUTOMATIC_CHECKPOINT_PENDING_FLOOR_BYTES,
): CheckpointSchedule {
  const floor = requirePositiveByteCount(pendingFloorBytes, 'pending floor')
  return Object.freeze({
    evaluate: (input: CheckpointScheduleInput) => evaluateCheckpointScheduleAtFloor(input, floor),
  })
}

export function evaluateCheckpointSchedule(
  input: CheckpointScheduleInput,
): CheckpointScheduleDecision {
  return evaluateCheckpointScheduleAtFloor(input, AUTOMATIC_CHECKPOINT_PENDING_FLOOR_BYTES)
}

function evaluateCheckpointScheduleAtFloor(
  input: CheckpointScheduleInput,
  pendingFloorBytes: bigint,
): CheckpointScheduleDecision {
  const durablePrefixBytes = requireByteCount(input?.durablePrefixBytes, 'durable prefix')
  const pendingBytes = requireByteCount(input?.pendingBytes, 'pending')
  const remainingBytes = requireByteCount(input?.remainingBytes, 'remaining')
  const nextPendingBytes = requiredPendingAdvance(durablePrefixBytes, pendingFloorBytes)

  // Time may request an early evaluation, but returning the byte threshold keeps
  // repeated timer observations from turning into preserving-open attempts.
  if (pendingBytes < nextPendingBytes) {
    return Object.freeze({ kind: 'wait-for-progress', nextPendingBytes })
  }

  const resultingPrefixBytes = durablePrefixBytes + pendingBytes
  if (remainingBytes <= resultingPrefixBytes) {
    return Object.freeze({ kind: 'finish-without-further-checkpoint' })
  }
  return Object.freeze({ kind: 'checkpoint-now' })
}

export function nextCheckpointRetryPendingBytes(
  durablePrefixBytes: bigint,
  evaluatedPendingBytes: bigint,
  pendingFloorBytes: bigint = AUTOMATIC_CHECKPOINT_PENDING_FLOOR_BYTES,
): bigint {
  return requireByteCount(evaluatedPendingBytes, 'evaluated pending') +
    requiredPendingAdvance(
      requireByteCount(durablePrefixBytes, 'durable prefix'),
      requirePositiveByteCount(pendingFloorBytes, 'pending floor'),
    )
}

function requiredPendingAdvance(durablePrefixBytes: bigint, pendingFloorBytes: bigint): bigint {
  return durablePrefixBytes > pendingFloorBytes
    ? durablePrefixBytes
    : pendingFloorBytes
}

function requireByteCount(value: bigint, label: string): bigint {
  if (typeof value !== 'bigint' || value < 0n) {
    throw new RangeError(`checkpoint schedule ${label} bytes must not be negative`)
  }
  return value
}

function requirePositiveByteCount(value: bigint, label: string): bigint {
  if (typeof value !== 'bigint' || value <= 0n) {
    throw new RangeError(`checkpoint schedule ${label} bytes must be positive`)
  }
  return value
}
