import { describe, expect, it } from 'vitest'
import {
  AUTOMATIC_CHECKPOINT_PENDING_FLOOR_BYTES,
  checkpointSchedule,
  evaluateCheckpointSchedule,
  type CheckpointScheduleInput,
} from '../../src/transfer/checkpoint-schedule'

const MIB = 1024n * 1024n
const FLOOR = 64n * MIB

describe('checkpoint schedule', () => {
  it.each([
    {
      label: 'no pending bytes',
      input: { durablePrefixBytes: 0n, pendingBytes: 0n, remainingBytes: FLOOR },
      nextPendingBytes: FLOOR,
    },
    {
      label: 'pending progress below the floor',
      input: { durablePrefixBytes: 0n, pendingBytes: FLOOR - 1n, remainingBytes: FLOOR },
      nextPendingBytes: FLOOR,
    },
    {
      label: 'durable prefix below the floor',
      input: { durablePrefixBytes: 1n, pendingBytes: FLOOR - 1n, remainingBytes: FLOOR },
      nextPendingBytes: FLOOR,
    },
    {
      label: 'durable prefix above the floor',
      input: { durablePrefixBytes: 128n * MIB, pendingBytes: FLOOR, remainingBytes: FLOOR },
      nextPendingBytes: 128n * MIB,
    },
  ])('returns the exact next threshold for $label', ({ input, nextPendingBytes }) => {
    expect(evaluateCheckpointSchedule(input)).toEqual({
      kind: 'wait-for-progress',
      nextPendingBytes,
    })
  })

  it('does not let a time-requested evaluation bypass the byte floor', () => {
    const decision = checkpointSchedule.evaluate({
      durablePrefixBytes: 0n,
      pendingBytes: FLOOR - 1n,
      remainingBytes: FLOOR + 1n,
    })

    expect(decision).toEqual({ kind: 'wait-for-progress', nextPendingBytes: FLOOR })
  })

  it.each([
    { durablePrefixBytes: 0n, pendingBytes: FLOOR, remainingBytes: FLOOR + 1n },
    { durablePrefixBytes: FLOOR, pendingBytes: FLOOR, remainingBytes: 128n * MIB + 1n },
    { durablePrefixBytes: 128n * MIB, pendingBytes: 128n * MIB, remainingBytes: 256n * MIB + 1n },
  ])('cuts at the exact sparse threshold %#', (input) => {
    expect(evaluateCheckpointSchedule(input)).toEqual({ kind: 'checkpoint-now' })
  })

  it('finishes when remaining work cannot repay the resulting preserving-open prefix', () => {
    expect(evaluateCheckpointSchedule({
      durablePrefixBytes: FLOOR,
      pendingBytes: FLOOR,
      remainingBytes: 128n * MIB,
    })).toEqual({ kind: 'finish-without-further-checkpoint' })
    expect(evaluateCheckpointSchedule({
      durablePrefixBytes: FLOOR,
      pendingBytes: FLOOR,
      remainingBytes: 128n * MIB + 1n,
    })).toEqual({ kind: 'checkpoint-now' })
  })

  it.each([
    ['negative durable prefix', { durablePrefixBytes: -1n, pendingBytes: 0n, remainingBytes: 0n }],
    ['negative pending bytes', { durablePrefixBytes: 0n, pendingBytes: -1n, remainingBytes: 0n }],
    ['negative remaining bytes', { durablePrefixBytes: 0n, pendingBytes: 0n, remainingBytes: -1n }],
    ['non-bigint durable prefix', { durablePrefixBytes: 0, pendingBytes: 0n, remainingBytes: 0n }],
  ] as const)('rejects %s', (_label, input) => {
    expect(() => evaluateCheckpointSchedule(input as unknown as CheckpointScheduleInput)).toThrow(RangeError)
  })

  it('evaluates a large transfer only at geometrically sparse thresholds', () => {
    const totalBytes = 2n * 1024n * MIB
    const writeBytes = MIB
    let durablePrefixBytes = 0n
    let pendingBytes = 0n
    let remainingBytes = totalBytes
    let nextPendingBytes = AUTOMATIC_CHECKPOINT_PENDING_FLOOR_BYTES
    const decisions: bigint[] = []

    while (remainingBytes > 0n) {
      pendingBytes += writeBytes
      remainingBytes -= writeBytes
      if (pendingBytes < nextPendingBytes) continue

      const decision = evaluateCheckpointSchedule({
        durablePrefixBytes,
        pendingBytes,
        remainingBytes,
      })
      decisions.push(durablePrefixBytes + pendingBytes)
      if (decision.kind === 'finish-without-further-checkpoint') break
      expect(decision.kind).toBe('checkpoint-now')
      durablePrefixBytes += pendingBytes
      pendingBytes = 0n
      const waiting = evaluateCheckpointSchedule({
        durablePrefixBytes,
        pendingBytes,
        remainingBytes,
      })
      expect(waiting.kind).toBe('wait-for-progress')
      if (waiting.kind === 'wait-for-progress') nextPendingBytes = waiting.nextPendingBytes
    }

    expect(decisions).toEqual([64n, 128n, 256n, 512n, 1024n].map(value => value * MIB))
  })
})
