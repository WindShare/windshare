import { describe, expect, it } from 'vitest'
import {
  PREFIX_COPY_CHECKPOINT_PENDING_FLOOR_BYTES,
  evaluateCheckpointSchedule,
  snapshotAutomaticCheckpointPolicy,
  type AutomaticCheckpointPolicy,
  type CheckpointScheduleInput,
} from '../../src/transfer/checkpoint-schedule'

const MIB = 1024n * 1024n
const FLOOR = PREFIX_COPY_CHECKPOINT_PENDING_FLOOR_BYTES
const PREFIX_COPY = { kind: 'prefix-copy', pendingBytes: FLOOR } as const
const INCREMENTAL = { kind: 'incremental', pendingBytes: 16n * MIB, pendingMilliseconds: 5_000 } as const

function progress(overrides: Partial<CheckpointScheduleInput> = {}): CheckpointScheduleInput {
  return {
    durableBytes: 0n, pendingBytes: 0n, remainingBytes: 100n * 1024n * MIB,
    pendingMilliseconds: 0, retryAtPendingBytes: 0n, ...overrides,
  }
}

describe('output-selected checkpoint scheduling', () => {
  it.each([0n, FLOOR - 1n, 128n * MIB])('preserves the copy-cost floor for durable bytes %s', durableBytes => {
    expect(evaluateCheckpointSchedule(PREFIX_COPY, progress({
      durableBytes, pendingBytes: FLOOR - 1n, pendingMilliseconds: 60_000,
    }))).toEqual({ kind: 'wait-for-progress' })
  })

  it('skips a prefix copy that the remaining download cannot repay', () => {
    expect(evaluateCheckpointSchedule(PREFIX_COPY, progress({
      durableBytes: FLOOR, pendingBytes: FLOOR, remainingBytes: 2n * FLOOR,
    }))).toEqual({ kind: 'finish-without-further-checkpoint' })
    expect(evaluateCheckpointSchedule(PREFIX_COPY, progress({
      durableBytes: FLOOR, pendingBytes: FLOOR, remainingBytes: 2n * FLOOR + 1n,
    }))).toMatchObject({ kind: 'checkpoint-now', trigger: 'pending-bytes' })
  })

  it('preserves geometrically sparse copy attempts', () => {
    let durableBytes = 0n
    let pendingBytes = 0n
    let remainingBytes = 2n * 1024n * MIB
    const cuts: bigint[] = []
    while (remainingBytes > 0n) {
      pendingBytes += MIB
      remainingBytes -= MIB
      const decision = evaluateCheckpointSchedule(PREFIX_COPY, progress({ durableBytes, pendingBytes, remainingBytes }))
      if (decision.kind === 'wait-for-progress') continue
      cuts.push(durableBytes + pendingBytes)
      if (decision.kind === 'finish-without-further-checkpoint') break
      durableBytes += pendingBytes
      pendingBytes = 0n
    }
    expect(cuts).toEqual([64n, 128n, 256n, 512n, 1024n].map(value => value * MIB))
  })

  it.each([0n, 50n * 1024n * MIB])('keeps incremental intervals fixed after %s durable bytes, including near completion', durableBytes => {
    expect(evaluateCheckpointSchedule(INCREMENTAL, progress({
      durableBytes, pendingBytes: INCREMENTAL.pendingBytes, remainingBytes: 1n,
    }))).toEqual({
      kind: 'checkpoint-now', trigger: 'pending-bytes', retryAtPendingBytes: 2n * INCREMENTAL.pendingBytes,
    })
  })

  it('allows incremental time checkpoints below the byte threshold', () => {
    expect(evaluateCheckpointSchedule(INCREMENTAL, progress({
      durableBytes: 50n * 1024n * MIB, pendingBytes: 1n, pendingMilliseconds: 4_999,
    }))).toEqual({ kind: 'wait-for-progress' })
    expect(evaluateCheckpointSchedule(INCREMENTAL, progress({
      durableBytes: 50n * 1024n * MIB, pendingBytes: 1n, pendingMilliseconds: 5_000,
    }))).toMatchObject({ kind: 'checkpoint-now', trigger: 'pending-time' })
  })

  it.each([PREFIX_COPY, INCREMENTAL])('does not hammer a deferred $kind checkpoint on every subsequent write', policy => {
    const initial = progress({ pendingBytes: FLOOR })
    const attempt = evaluateCheckpointSchedule(policy, initial)
    expect(attempt.kind).toBe('checkpoint-now')
    if (attempt.kind !== 'checkpoint-now') throw new Error('expected checkpoint')
    expect(evaluateCheckpointSchedule(policy, {
      ...initial, pendingBytes: FLOOR + 1n, pendingMilliseconds: 60_000,
      retryAtPendingBytes: attempt.retryAtPendingBytes,
    })).toEqual({ kind: 'wait-for-progress' })
    expect(evaluateCheckpointSchedule(policy, {
      ...initial, pendingBytes: attempt.retryAtPendingBytes, retryAtPendingBytes: attempt.retryAtPendingBytes,
    })).toMatchObject({ kind: 'checkpoint-now' })
  })

  it.each([PREFIX_COPY, INCREMENTAL])('leaves empty progress and the final write to final commit for $kind', policy => {
    expect(evaluateCheckpointSchedule(policy, progress({ pendingMilliseconds: 60_000 })))
      .toEqual({ kind: 'wait-for-progress' })
    expect(evaluateCheckpointSchedule(policy, progress({ pendingBytes: FLOOR, remainingBytes: 0n })))
      .toEqual({ kind: 'wait-for-progress' })
    expect(evaluateCheckpointSchedule({ kind: 'disabled' }, progress({ pendingBytes: FLOOR })))
      .toEqual({ kind: 'wait-for-progress' })
  })

  it.each([
    { durableBytes: -1n }, { pendingBytes: -1n }, { remainingBytes: -1n },
    { retryAtPendingBytes: -1n }, { pendingMilliseconds: -1 }, { pendingMilliseconds: Number.NaN },
    { durableBytes: 0 },
  ])('rejects invalid progress %#', invalid => {
    expect(() => evaluateCheckpointSchedule(INCREMENTAL, progress(invalid as Partial<CheckpointScheduleInput>)))
      .toThrow(RangeError)
  })

  it.each([
    { kind: 'bounded' }, { kind: 'incremental', pendingBytes: 0n, pendingMilliseconds: 1 },
    { kind: 'prefix-copy', pendingBytes: -1n }, { kind: 'incremental', pendingBytes: 1n, pendingMilliseconds: 0 },
  ])('rejects ambiguous or invalid policies %#', policy => {
    expect(() => snapshotAutomaticCheckpointPolicy(policy as AutomaticCheckpointPolicy)).toThrow(RangeError)
  })
})
