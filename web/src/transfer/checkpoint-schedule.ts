export const PREFIX_COPY_CHECKPOINT_PENDING_FLOOR_BYTES = 64n * 1024n * 1024n

export const NATIVE_FILE_CHECKPOINT_PENDING_BYTES = 16n * 1024n * 1024n
export const NATIVE_FILE_CHECKPOINT_PENDING_MILLISECONDS = 5_000
export const ZIP_OBJECT_CHECKPOINT_PENDING_BYTES = 4n * 1024n * 1024n
export const ZIP_OBJECT_CHECKPOINT_PENDING_MILLISECONDS = 1_000

export type AutomaticCheckpointTrigger = 'pending-bytes' | 'pending-time'

export type AutomaticCheckpointPolicy =
  | Readonly<{ kind: 'disabled' }>
  | Readonly<{ kind: 'prefix-copy'; pendingBytes: bigint }>
  | Readonly<{ kind: 'incremental'; pendingBytes: bigint; pendingMilliseconds: number }>

export interface CheckpointScheduleInput {
  readonly durableBytes: bigint
  readonly pendingBytes: bigint
  readonly remainingBytes: bigint
  readonly pendingMilliseconds: number
  readonly retryAtPendingBytes: bigint
}

export type CheckpointScheduleDecision =
  | Readonly<{ kind: 'wait-for-progress' }>
  | Readonly<{
      kind: 'checkpoint-now'
      trigger: AutomaticCheckpointTrigger
      retryAtPendingBytes: bigint
    }>
  | Readonly<{ kind: 'finish-without-further-checkpoint' }>

export function snapshotAutomaticCheckpointPolicy(policy: AutomaticCheckpointPolicy): AutomaticCheckpointPolicy {
  if (policy?.kind === 'disabled') return Object.freeze({ kind: 'disabled' })
  if (policy?.kind !== 'prefix-copy' && policy?.kind !== 'incremental') {
    throw new RangeError('checkpoint policy kind is invalid')
  }
  const pendingBytes = requireByteCount(policy.pendingBytes, 'threshold')
  if (pendingBytes === 0n) throw new RangeError('checkpoint threshold must be positive')
  if (policy.kind === 'prefix-copy') return Object.freeze({ kind: policy.kind, pendingBytes })
  if (!Number.isSafeInteger(policy.pendingMilliseconds) || policy.pendingMilliseconds <= 0) {
    throw new RangeError('checkpoint time threshold must be positive')
  }
  return Object.freeze({ kind: policy.kind, pendingBytes, pendingMilliseconds: policy.pendingMilliseconds })
}

/** The output selects its cost model; transfer progress alone cannot determine a safe schedule. */
export function evaluateCheckpointSchedule(
  policy: AutomaticCheckpointPolicy,
  input: CheckpointScheduleInput,
): CheckpointScheduleDecision {
  const durableBytes = requireByteCount(input.durableBytes, 'durable')
  const pendingBytes = requireByteCount(input.pendingBytes, 'pending')
  const remainingBytes = requireByteCount(input.remainingBytes, 'remaining')
  const retryAtPendingBytes = requireByteCount(input.retryAtPendingBytes, 'retry')
  if (!Number.isFinite(input.pendingMilliseconds) || input.pendingMilliseconds < 0) {
    throw new RangeError('checkpoint pending time must not be negative')
  }
  if (policy.kind === 'disabled' || pendingBytes === 0n || remainingBytes === 0n ||
      pendingBytes < retryAtPendingBytes) return Object.freeze({ kind: 'wait-for-progress' })

  if (policy.kind === 'prefix-copy') {
    const requiredAdvance = durableBytes > policy.pendingBytes ? durableBytes : policy.pendingBytes
    // A timer cannot make a full-prefix copy cheaper. Keep the existing sparse
    // schedule and let the output's admission authority enforce its copy budget.
    if (pendingBytes < requiredAdvance) return Object.freeze({ kind: 'wait-for-progress' })
    if (remainingBytes <= durableBytes + pendingBytes) {
      return Object.freeze({ kind: 'finish-without-further-checkpoint' })
    }
    return checkpointNow('pending-bytes', pendingBytes + requiredAdvance)
  }

  // In-place flushes do not copy the saved prefix, including near file completion.
  if (pendingBytes >= policy.pendingBytes) {
    return checkpointNow('pending-bytes', pendingBytes + policy.pendingBytes)
  }
  if (input.pendingMilliseconds >= policy.pendingMilliseconds) {
    return checkpointNow('pending-time', pendingBytes + policy.pendingBytes)
  }
  return Object.freeze({ kind: 'wait-for-progress' })
}

function checkpointNow(
  trigger: AutomaticCheckpointTrigger,
  retryAtPendingBytes: bigint,
): CheckpointScheduleDecision {
  return Object.freeze({ kind: 'checkpoint-now', trigger, retryAtPendingBytes })
}

function requireByteCount(value: bigint, label: string): bigint {
  if (typeof value !== 'bigint' || value < 0n) {
    throw new RangeError(`checkpoint schedule ${label} bytes must not be negative`)
  }
  return value
}
