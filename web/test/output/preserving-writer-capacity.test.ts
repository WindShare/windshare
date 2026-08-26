import { describe, expect, it } from 'vitest'
import {
  MAXIMUM_AGGREGATE_PRESERVING_WRITER_TEMPORARY_BYTES,
  createPreservingWriterCapacityAuthority,
} from '../../src/output/persistent-tree/preserving-writer-capacity'
import type {
  AutomaticCapacityHandoffResult,
  CheckpointAuthorityObservation,
  PreservingWriterCapacityRequest,
  PreservingWriterCapacityToken,
  PreservingWriterCost,
} from '../../src/output/persistent-tree/contracts'

const MEBIBYTE_BYTES = 1024n * 1024n
const ONE_WRITER_BYTES = 128n * MEBIBYTE_BYTES
const ATTEMPT_IDENTITY = Object.freeze({
  receiveOperationId: 'receive-operation-capacity',
  transferJobId: 'transfer-job-capacity',
  outputSessionId: 'output-session-capacity',
})

describe('shared preserving writer capacity', () => {
  it('returns automatic contention immediately and retries after capacity is released', () => {
    const authority = createPreservingWriterCapacityAuthority({ identity: ATTEMPT_IDENTITY })
    const tokens = Array.from({ length: 8 }, (_, index) => {
      const token = reserved(authority.tryHandoff(automaticRequest(`${index}.bin`, ONE_WRITER_BYTES)))
      token.commit()
      return token
    })

    expect(authority.snapshot()).toMatchObject({
      heldTemporaryBytes: MAXIMUM_AGGREGATE_PRESERVING_WRITER_TEMPORARY_BYTES,
      heldTokens: 8,
    })
    expect(authority.tryHandoff(automaticRequest('contended.bin', 1n))).toEqual({
      kind: 'unavailable',
      reason: 'capacity-unavailable',
    })

    tokens[0]!.release('writer-closed')
    expect(authority.tryHandoff(automaticRequest('retry.bin', ONE_WRITER_BYTES)).kind).toBe(
      'reserved',
    )
  })

  it('atomically replaces a full-capacity token without double counting old and next prefixes', () => {
    const authority = createPreservingWriterCapacityAuthority({ identity: ATTEMPT_IDENTITY })
    const tokens = Array.from({ length: 8 }, (_, index) => {
      const token = reserved(authority.tryHandoff(automaticRequest(`${index}.bin`, ONE_WRITER_BYTES)))
      token.commit()
      return token
    })
    const old = tokens[0]!

    expect(authority.tryHandoff(
      automaticRequest('0.bin', 2n * ONE_WRITER_BYTES, 2),
      old,
    )).toEqual({ kind: 'unavailable', reason: 'capacity-unavailable' })
    expect(authority.snapshot()).toMatchObject({
      heldTemporaryBytes: MAXIMUM_AGGREGATE_PRESERVING_WRITER_TEMPORARY_BYTES,
      heldTokens: 8,
    })

    const next = reserved(authority.tryHandoff(
      automaticRequest('0.bin', ONE_WRITER_BYTES, 2),
      old,
    ))
    expect(authority.snapshot()).toMatchObject({
      heldTemporaryBytes: MAXIMUM_AGGREGATE_PRESERVING_WRITER_TEMPORARY_BYTES,
      heldTokens: 8,
    })
    expect(() => authority.tryHandoff(automaticRequest('invalid.bin', 1n), old)).toThrowError(
      DOMException,
    )
    next.commit()
  })

  it('preserves paused-recovery FIFO and removes a cancelled head request', async () => {
    const authority = createPreservingWriterCapacityAuthority({ identity: ATTEMPT_IDENTITY })
    const blocker = reserved(authority.tryHandoff(automaticRequest(
      'blocker.bin',
      MAXIMUM_AGGREGATE_PRESERVING_WRITER_TEMPORARY_BYTES,
    )))
    blocker.commit()
    const cancelled = new AbortController()
    const first = authority.reservePaused(pausedRequest('first.bin', ONE_WRITER_BYTES), cancelled.signal)
    const second = authority.reservePaused(pausedRequest('second.bin', ONE_WRITER_BYTES))

    expect(authority.snapshot().queuedPausedRecoveries).toBe(2)
    cancelled.abort(new DOMException('cancel first recovery', 'AbortError'))
    await expect(first).rejects.toMatchObject({ name: 'AbortError' })
    expect(authority.snapshot().queuedPausedRecoveries).toBe(1)

    blocker.release('writer-closed')
    const secondToken = await second
    expect(secondToken.reservedTemporaryBytes).toBe(ONE_WRITER_BYTES)
    expect(authority.snapshot()).toMatchObject({
      heldTokens: 1,
      queuedPausedRecoveries: 0,
    })
  })

  it('does not let a smaller paused request overtake an earlier FIFO head', async () => {
    const authority = createPreservingWriterCapacityAuthority({ identity: ATTEMPT_IDENTITY })
    const blocker = reserved(authority.tryHandoff(automaticRequest(
      'blocker.bin',
      MAXIMUM_AGGREGATE_PRESERVING_WRITER_TEMPORARY_BYTES,
    )))
    blocker.commit()
    const order: string[] = []
    const firstPromise = authority.reservePaused(pausedRequest(
      'first.bin',
      MAXIMUM_AGGREGATE_PRESERVING_WRITER_TEMPORARY_BYTES,
    )).then(token => {
      order.push('first')
      return token
    })
    const secondPromise = authority.reservePaused(pausedRequest('second.bin', 1n)).then(token => {
      order.push('second')
      return token
    })

    blocker.release('writer-closed')
    const first = await firstPromise
    await Promise.resolve()
    expect(order).toEqual(['first'])
    expect(authority.snapshot().queuedPausedRecoveries).toBe(1)

    first.release('writer-closed')
    await secondPromise
    expect(order).toEqual(['first', 'second'])
  })

  it('waits for exclusive capacity for an oversized authorized recovery', async () => {
    const authority = createPreservingWriterCapacityAuthority({ identity: ATTEMPT_IDENTITY })
    const active = reserved(authority.tryHandoff(automaticRequest('active.bin', ONE_WRITER_BYTES)))
    active.commit()
    const oversizedBytes = MAXIMUM_AGGREGATE_PRESERVING_WRITER_TEMPORARY_BYTES + 1n
    const oversizedPromise = authority.reservePaused(pausedRequest('oversized.bin', oversizedBytes))

    expect(authority.snapshot()).toMatchObject({
      heldTokens: 1,
      queuedPausedRecoveries: 1,
      oversizedExclusive: false,
    })
    expect(authority.tryHandoff(automaticRequest('automatic.bin', 1n)).kind).toBe('unavailable')
    active.release('writer-closed')

    const oversized = await oversizedPromise
    expect(authority.snapshot()).toMatchObject({
      heldTemporaryBytes: oversizedBytes,
      heldTokens: 1,
      oversizedExclusive: true,
    })
    const laterPromise = authority.reservePaused(pausedRequest('later.bin', 1n))
    expect(authority.tryHandoff(automaticRequest('automatic.bin', 1n)).kind).toBe('unavailable')
    oversized.release('writer-closed')
    await laterPromise
  })

  it('keeps automatic handoff from overtaking an authorized paused queue', async () => {
    const authority = createPreservingWriterCapacityAuthority({ identity: ATTEMPT_IDENTITY })
    const current = reserved(authority.tryHandoff(automaticRequest(
      'current.bin',
      MAXIMUM_AGGREGATE_PRESERVING_WRITER_TEMPORARY_BYTES,
    )))
    current.commit()
    const paused = authority.reservePaused(pausedRequest('paused.bin', ONE_WRITER_BYTES))

    expect(authority.tryHandoff(automaticRequest('current.bin', ONE_WRITER_BYTES, 2), current)).toEqual({
      kind: 'unavailable',
      reason: 'capacity-unavailable',
    })
    expect(authority.snapshot()).toMatchObject({
      heldTokens: 1,
      queuedPausedRecoveries: 1,
    })
    current.release('writer-closed')
    expect((await paused).purpose).toBe('paused-file-recovery')
  })

  it('makes token commit and release idempotent while rejecting foreign or settled handoffs', () => {
    const authority = createPreservingWriterCapacityAuthority({ identity: ATTEMPT_IDENTITY })
    const foreignAuthority = createPreservingWriterCapacityAuthority({ identity: ATTEMPT_IDENTITY })
    const token = reserved(authority.tryHandoff(automaticRequest('owned.bin', ONE_WRITER_BYTES)))
    const foreign = reserved(foreignAuthority.tryHandoff(automaticRequest('foreign.bin', 1n)))

    expect(() => authority.tryHandoff(automaticRequest('tentative.bin', 1n), token)).toThrowError(
      DOMException,
    )
    token.commit()
    token.commit()
    expect(() => authority.tryHandoff(automaticRequest('foreign.bin', 1n), foreign)).toThrowError(
      TypeError,
    )
    token.release('writer-closed')
    token.release('writer-closed')
    expect(() => token.commit()).toThrowError(DOMException)
    expect(() => authority.tryHandoff(automaticRequest('released.bin', 1n), token)).toThrowError(
      DOMException,
    )
    expect(authority.snapshot()).toMatchObject({ heldTemporaryBytes: 0n, heldTokens: 0 })
  })

  it('cancels queued work and invalidates every held token at the terminal cut', async () => {
    const authority = createPreservingWriterCapacityAuthority({ identity: ATTEMPT_IDENTITY })
    const active = reserved(authority.tryHandoff(automaticRequest(
      'active.bin',
      MAXIMUM_AGGREGATE_PRESERVING_WRITER_TEMPORARY_BYTES,
    )))
    active.commit()
    const queued = authority.reservePaused(pausedRequest('queued.bin', 1n))

    authority.close()
    await expect(queued).rejects.toMatchObject({ name: 'InvalidStateError' })
    expect(authority.snapshot()).toEqual({
      accepting: false,
      heldTemporaryBytes: 0n,
      heldTokens: 0,
      queuedPausedRecoveries: 0,
      oversizedExclusive: false,
    })
    active.release('writer-closed')
    expect(() => active.commit()).toThrowError(DOMException)
    expect(authority.tryHandoff(automaticRequest('after-close.bin', 1n))).toEqual({
      kind: 'unavailable',
      reason: 'capacity-unavailable',
    })
  })

  it('emits decision context, modeled cost, remaining budget, and release reason', () => {
    const observations: CheckpointAuthorityObservation[] = []
    const authority = createPreservingWriterCapacityAuthority({
      identity: ATTEMPT_IDENTITY,
      observe: observation => observations.push(observation),
    })
    const token = reserved(authority.tryHandoff(automaticRequest('observed.bin', ONE_WRITER_BYTES)))
    token.commit()
    token.release('writer-aborted')

    expect(observations).toEqual([
      expect.objectContaining({
        authority: 'preserving-capacity',
        ...ATTEMPT_IDENTITY,
        materializationRelativePath: ['observed.bin'],
        trigger: 'pending-bytes',
        checkpointOrdinal: 1,
        remainingAutomaticWriteAmplificationBytes: 2n * 1024n * MEBIBYTE_BYTES,
        decision: 'admitted',
      }),
      expect.objectContaining({ decision: 'committed' }),
      expect.objectContaining({ decision: 'released', releaseReason: 'writer-aborted' }),
    ])
  })
})

function automaticRequest(
  name: string,
  bytes: bigint,
  checkpointOrdinal = 1,
): PreservingWriterCapacityRequest {
  return {
    materializationRelativePath: [name],
    trigger: 'pending-bytes',
    checkpointOrdinal,
    cost: cost(bytes),
    remainingAutomaticWriteAmplificationBytes: 2n * 1024n * MEBIBYTE_BYTES,
  }
}

function pausedRequest(name: string, bytes: bigint): PreservingWriterCapacityRequest {
  return {
    materializationRelativePath: [name],
    trigger: 'paused-file-recovery',
    cost: cost(bytes),
  }
}

function cost(bytes: bigint): PreservingWriterCost {
  return {
    prefixCopyBytes: bytes,
    writeAmplificationBytes: bytes,
    temporaryBytes: bytes,
  }
}

function reserved(result: AutomaticCapacityHandoffResult): PreservingWriterCapacityToken {
  expect(result.kind).toBe('reserved')
  if (result.kind !== 'reserved') throw new Error('expected preserving capacity reservation')
  return result.token
}
