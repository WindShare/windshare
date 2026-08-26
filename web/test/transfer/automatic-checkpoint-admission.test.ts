import { describe, expect, it } from 'vitest'
import {
  MAXIMUM_AUTOMATIC_PREFIX_COPY_BYTES,
  MAXIMUM_AUTOMATIC_WRITE_AMPLIFICATION_BYTES,
  createAutomaticCheckpointAdmissionAuthority,
} from '../../src/output/persistent-tree/automatic-checkpoint-admission'
import type {
  AutomaticCheckpointAdmissionDecision,
  AutomaticCheckpointBudgetHold,
  CheckpointAuthorityObservation,
  PreservingWriterCost,
} from '../../src/output/persistent-tree/contracts'

const MEBIBYTE_BYTES = 1024n * 1024n
const FIRST_CHECKPOINT_BYTES = 64n * MEBIBYTE_BYTES
const SECOND_CHECKPOINT_BYTES = 128n * MEBIBYTE_BYTES

const ATTEMPT_IDENTITY = Object.freeze({
  receiveOperationId: 'receive-operation-1',
  transferJobId: 'transfer-job-1',
  outputSessionId: 'output-session-1',
})

describe('attempt-scoped automatic checkpoint admission', () => {
  it('admits eight writers through both useful cuts without starving a first checkpoint', () => {
    const authority = createAutomaticCheckpointAdmissionAuthority({ identity: ATTEMPT_IDENTITY })
    const files = Array.from({ length: 8 }, (_, index) =>
      authority.enrollFile(['large', `${index}.bin`]))

    for (const file of files) admitted(file.request('pending-bytes', cost(FIRST_CHECKPOINT_BYTES))).commit()
    for (const file of files) admitted(file.request('pending-time', cost(SECOND_CHECKPOINT_BYTES))).commit()

    expect(authority.snapshot()).toEqual({
      accepting: true,
      enrolledFiles: 8,
      committedWriteAmplificationBytes: 1536n * MEBIBYTE_BYTES,
      remainingWriteAmplificationBytes: 512n * MEBIBYTE_BYTES,
      tentativeHolds: 0,
      cumulativelyExhausted: false,
    })
  })

  it('does not serialize eligible cuts behind dormant enrollments while budget is ample', () => {
    const authority = createAutomaticCheckpointAdmissionAuthority({ identity: ATTEMPT_IDENTITY })
    const dormant = authority.enrollFile(['dormant.bin'])
    const ready = authority.enrollFile(['ready.bin'])

    const readyHold = admitted(ready.request('pending-bytes', cost(FIRST_CHECKPOINT_BYTES)))
    expect(authority.snapshot().tentativeHolds).toBe(1)
    readyHold.release('unused')
    dormant.retire()
  })

  it('reserves scarce budget by checkpoint ordinal and enrollment FIFO without waiting', () => {
    const authority = createAutomaticCheckpointAdmissionAuthority({ identity: ATTEMPT_IDENTITY })
    const first = authority.enrollFile(['first.bin'])
    const later = authority.enrollFile(['later.bin'])
    admitted(later.request('pending-bytes', cost(FIRST_CHECKPOINT_BYTES))).commit()
    commitBudget(authority, 1_856n * MEBIBYTE_BYTES)
    const blocker = authority.enrollFile(['tentative-blocker.bin'])
    const blockerHold = admitted(blocker.request('pending-bytes', cost(SECOND_CHECKPOINT_BYTES)))
    expect(first.request('pending-bytes', cost(FIRST_CHECKPOINT_BYTES))).toMatchObject({
      kind: 'deferred',
      reason: 'checkpoint-priority',
    })
    blockerHold.release('unused')

    expect(later.request('pending-bytes', cost(SECOND_CHECKPOINT_BYTES))).toMatchObject({
      kind: 'deferred',
      reason: 'checkpoint-priority',
    })
    admitted(first.request('pending-bytes', cost(FIRST_CHECKPOINT_BYTES))).commit()
    expect(later.request('pending-bytes', cost(SECOND_CHECKPOINT_BYTES))).toMatchObject({
      kind: 'finished',
      reason: 'cumulative-write-amplification-budget',
    })
  })

  it('uses enrollment FIFO only to break a scarce-budget tie between pending cuts', () => {
    const authority = createAutomaticCheckpointAdmissionAuthority({ identity: ATTEMPT_IDENTITY })
    commitBudget(authority, 1_984n * MEBIBYTE_BYTES)
    const first = authority.enrollFile(['first-pending.bin'])
    const second = authority.enrollFile(['second-pending.bin'])
    const blocker = authority.enrollFile(['tie-blocker.bin'])
    const blockerHold = admitted(blocker.request('pending-bytes', cost(FIRST_CHECKPOINT_BYTES)))
    expect(first.request('pending-bytes', cost(FIRST_CHECKPOINT_BYTES)).kind).toBe('deferred')
    blockerHold.release('unused')

    expect(second.request('pending-bytes', cost(FIRST_CHECKPOINT_BYTES))).toMatchObject({
      kind: 'deferred',
      reason: 'checkpoint-priority',
    })
    admitted(first.request('pending-bytes', cost(FIRST_CHECKPOINT_BYTES))).release('unused')
  })

  it('makes immutable prefix cost exhaustion sticky without consuming accounting', () => {
    const authority = createAutomaticCheckpointAdmissionAuthority({ identity: ATTEMPT_IDENTITY })
    const file = authority.enrollFile(['too-large.bin'])
    const oversized = MAXIMUM_AUTOMATIC_PREFIX_COPY_BYTES + 1n

    expect(file.request('pending-bytes', cost(oversized))).toMatchObject({
      kind: 'finished',
      reason: 'prefix-copy-budget',
    })
    expect(file.request('pending-time', cost(1n))).toMatchObject({
      kind: 'finished',
      reason: 'prefix-copy-budget',
    })
    expect(authority.snapshot().committedWriteAmplificationBytes).toBe(0n)
  })

  it('validates tentative costs and admits the exact per-open boundary', () => {
    const authority = createAutomaticCheckpointAdmissionAuthority({ identity: ATTEMPT_IDENTITY })
    const file = authority.enrollFile(['boundary.bin'])

    expect(() => file.request('pending-bytes', cost(-1n))).toThrowError(TypeError)
    const hold = admitted(file.request(
      'pending-bytes',
      cost(MAXIMUM_AUTOMATIC_PREFIX_COPY_BYTES),
    ))
    expect(authority.snapshot()).toMatchObject({
      tentativeHolds: 1,
      remainingWriteAmplificationBytes:
        MAXIMUM_AUTOMATIC_WRITE_AMPLIFICATION_BYTES - MAXIMUM_AUTOMATIC_PREFIX_COPY_BYTES,
    })
    expect(file.request('pending-time', cost(MAXIMUM_AUTOMATIC_PREFIX_COPY_BYTES + 1n))).toMatchObject({
      kind: 'deferred',
      reason: 'checkpoint-priority',
    })
    hold.release('unused')
    expect(authority.snapshot().remainingWriteAmplificationBytes).toBe(
      MAXIMUM_AUTOMATIC_WRITE_AMPLIFICATION_BYTES,
    )
    admitted(file.request('pending-time', cost(1n))).release('unused')
  })

  it('keeps an unaffordable later cut local until the committed budget is actually exhausted', () => {
    const authority = createAutomaticCheckpointAdmissionAuthority({ identity: ATTEMPT_IDENTITY })
    commitBudget(authority, 1_984n * MEBIBYTE_BYTES)
    const laterCut = authority.enrollFile(['later-cut.bin'])

    expect(laterCut.request('pending-bytes', cost(SECOND_CHECKPOINT_BYTES))).toMatchObject({
      kind: 'finished',
      reason: 'cumulative-write-amplification-budget',
    })
    expect(authority.snapshot()).toMatchObject({
      remainingWriteAmplificationBytes: FIRST_CHECKPOINT_BYTES,
      cumulativelyExhausted: false,
    })

    const firstCut = authority.enrollFile(['first-cut.bin'])
    admitted(firstCut.request('pending-time', cost(FIRST_CHECKPOINT_BYTES))).commit()
    expect(authority.snapshot().cumulativelyExhausted).toBe(true)
    const afterExhaustion = authority.enrollFile(['after-exhaustion.bin'])
    expect(afterExhaustion.request('pending-time', cost(FIRST_CHECKPOINT_BYTES))).toMatchObject({
      kind: 'finished',
      reason: 'cumulative-write-amplification-budget',
    })
  })

  it('treats tentative budget contention as retryable until the hold commits', () => {
    const authority = createAutomaticCheckpointAdmissionAuthority({ identity: ATTEMPT_IDENTITY })
    commitBudget(authority, 1_920n * MEBIBYTE_BYTES)
    const tentative = authority.enrollFile(['tentative.bin'])
    const waiting = authority.enrollFile(['waiting.bin'])
    const tentativeHold = admitted(tentative.request('pending-bytes', cost(SECOND_CHECKPOINT_BYTES)))

    expect(waiting.request('pending-bytes', cost(FIRST_CHECKPOINT_BYTES))).toMatchObject({
      kind: 'deferred',
      reason: 'checkpoint-priority',
    })
    expect(authority.snapshot().cumulativelyExhausted).toBe(false)
    tentativeHold.release('unused')
    expect(waiting.request('pending-bytes', cost(FIRST_CHECKPOINT_BYTES)).kind).toBe('admitted')
  })

  it('makes cumulative exhaustion sticky only after no useful first cut can fit', () => {
    const authority = createAutomaticCheckpointAdmissionAuthority({ identity: ATTEMPT_IDENTITY })
    commitBudget(
      authority,
      MAXIMUM_AUTOMATIC_WRITE_AMPLIFICATION_BYTES - FIRST_CHECKPOINT_BYTES + 1n,
    )

    expect(authority.snapshot()).toMatchObject({
      remainingWriteAmplificationBytes: FIRST_CHECKPOINT_BYTES - 1n,
      cumulativelyExhausted: true,
    })
    const file = authority.enrollFile(['after-effective-exhaustion.bin'])
    expect(file.request('pending-bytes', cost(1n))).toMatchObject({
      kind: 'finished',
      reason: 'cumulative-write-amplification-budget',
    })
  })

  it('rolls back an unused tentative hold and commits successful accounting exactly once', () => {
    const authority = createAutomaticCheckpointAdmissionAuthority({ identity: ATTEMPT_IDENTITY })
    const file = authority.enrollFile(['retry.bin'])
    const first = admitted(file.request('pending-bytes', cost(FIRST_CHECKPOINT_BYTES)))

    expect(authority.snapshot().tentativeHolds).toBe(1)
    first.release('replacement-open-failed')
    first.release('replacement-open-failed')
    expect(authority.snapshot()).toMatchObject({
      tentativeHolds: 0,
      committedWriteAmplificationBytes: 0n,
    })
    expect(() => first.commit()).toThrowError(DOMException)

    const retry = admitted(file.request('pending-time', cost(FIRST_CHECKPOINT_BYTES)))
    retry.commit()
    retry.commit()
    retry.release('writer-closed')
    expect(authority.snapshot()).toMatchObject({
      tentativeHolds: 0,
      committedWriteAmplificationBytes: FIRST_CHECKPOINT_BYTES,
    })
  })

  it('keeps totals on one attempt and starts fresh only with a new authority', () => {
    const firstAttempt = createAutomaticCheckpointAdmissionAuthority({ identity: ATTEMPT_IDENTITY })
    const file = firstAttempt.enrollFile(['reopen.bin'])
    admitted(file.request('pending-bytes', cost(FIRST_CHECKPOINT_BYTES))).commit()

    // Writer and lane replacement retain the same file ticket and attempt authority.
    admitted(file.request('pending-time', cost(SECOND_CHECKPOINT_BYTES))).commit()
    expect(firstAttempt.snapshot().committedWriteAmplificationBytes).toBe(
      FIRST_CHECKPOINT_BYTES + SECOND_CHECKPOINT_BYTES,
    )

    const continuedAttempt = createAutomaticCheckpointAdmissionAuthority({
      identity: { ...ATTEMPT_IDENTITY, transferJobId: 'transfer-job-2' },
    })
    expect(continuedAttempt.snapshot().committedWriteAmplificationBytes).toBe(0n)
  })

  it('emits immutable structured decisions and deterministic release reasons', () => {
    const observations: CheckpointAuthorityObservation[] = []
    const authority = createAutomaticCheckpointAdmissionAuthority({
      identity: ATTEMPT_IDENTITY,
      observe: observation => observations.push(observation),
    })
    const file = authority.enrollFile(['observed.bin'])
    const hold = admitted(file.request('pending-time', cost(FIRST_CHECKPOINT_BYTES)))
    hold.release('unused')

    expect(observations).toEqual([
      expect.objectContaining({
        authority: 'automatic-admission',
        ...ATTEMPT_IDENTITY,
        materializationRelativePath: ['observed.bin'],
        trigger: 'pending-time',
        checkpointOrdinal: 1,
        decision: 'admitted',
      }),
      expect.objectContaining({
        decision: 'released',
        releaseReason: 'unused',
      }),
    ])
    expect(Object.isFrozen(observations[0])).toBe(true)
    expect(Object.isFrozen(observations[0]!.cost)).toBe(true)
  })

  it('releases tentative holds synchronously at the terminal cut', () => {
    const authority = createAutomaticCheckpointAdmissionAuthority({ identity: ATTEMPT_IDENTITY })
    const file = authority.enrollFile(['terminal.bin'])
    const hold = admitted(file.request('pending-bytes', cost(FIRST_CHECKPOINT_BYTES)))

    authority.close()
    expect(authority.snapshot()).toMatchObject({
      accepting: false,
      enrolledFiles: 0,
      tentativeHolds: 0,
    })
    expect(() => hold.commit()).toThrowError(DOMException)
    expect(() => file.request('pending-bytes', cost(1n))).toThrowError(DOMException)
  })

  it('removes retired file metadata from the attempt immediately', () => {
    const authority = createAutomaticCheckpointAdmissionAuthority({ identity: ATTEMPT_IDENTITY })
    const retired = authority.enrollFile(['retired.bin'])
    expect(authority.snapshot().enrolledFiles).toBe(1)

    retired.retire('file-committed')
    expect(authority.snapshot()).toMatchObject({
      enrolledFiles: 0,
      tentativeHolds: 0,
    })
    expect(() => retired.request('pending-bytes', cost(FIRST_CHECKPOINT_BYTES)))
      .toThrowError(DOMException)
  })
})

function cost(bytes: bigint): PreservingWriterCost {
  return {
    prefixCopyBytes: bytes,
    writeAmplificationBytes: bytes,
    temporaryBytes: bytes,
  }
}

function admitted(decision: AutomaticCheckpointAdmissionDecision): AutomaticCheckpointBudgetHold {
  expect(decision.kind).toBe('admitted')
  if (decision.kind !== 'admitted') throw new Error('expected automatic checkpoint admission')
  return decision.hold
}

function commitBudget(
  authority: ReturnType<typeof createAutomaticCheckpointAdmissionAuthority>,
  bytes: bigint,
): void {
  let remaining = bytes
  let index = 0
  while (remaining > 0n) {
    const increment = remaining > SECOND_CHECKPOINT_BYTES ? SECOND_CHECKPOINT_BYTES : remaining
    const filler = authority.enrollFile(['budget', `${index}.bin`])
    admitted(filler.request('pending-bytes', cost(increment))).commit()
    filler.retire()
    remaining -= increment
    index += 1
  }
}
