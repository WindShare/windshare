import { describe, expect, it } from 'vitest'

import { encodeV2Body, encodeV2Message, V2_MESSAGE_KIND } from '../../src/session/v2-message'
import { V2_MAXIMUM_PEER_CANDIDATES } from '../../src/session/v2-operation-continuation'
import {
  V2_MAXIMUM_ACTIVE_OPERATIONS,
  V2_SESSION_CONTROL_BACKLOG,
  V2OperationRouter,
} from '../../src/session/v2-operation-router'

const EMPTY_REQUEST_BODY = encodeV2Body([])
const EXACT_CANDIDATE_REPLAY_STRESS_COUNT = 300

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

describe('v2 operation replay ownership', () => {
  it('drops concurrent exact peer-answer replays before delivery without advancing multiplicity', async () => {
    const router = new V2OperationRouter(() => undefined)
    const operationId = identity(22)
    const peerPathId = identity(23)
    const attemptId = identity(24)
    const operation = router.create(
      operationId,
      V2_MESSAGE_KIND.peerOffer,
      peerOfferBody(peerPathId, attemptId),
    )
    const answer = peerAnswer(operationId, peerPathId, attemptId, 'v=0\r\ns=answer-a\r\n')
    const exactCopies = Array.from(
      { length: 32 },
      () => peerAnswer(operationId, peerPathId, attemptId, 'v=0\r\ns=answer-a\r\n'),
    )

    await expect(Promise.all(exactCopies.map((copy) => router.route(copy)))).resolves.toHaveLength(32)
    await expect(operation.next()).resolves.toEqual(answer)

    const waiting = operation.next()
    const candidate = peerCandidate(operationId, peerPathId, attemptId, 1)
    await router.route(candidate)
    await expect(waiting).resolves.toEqual(candidate)

    const conflict = peerAnswer(operationId, peerPathId, attemptId, 'v=0\r\ns=answer-b\r\n')
    const interleaved = await Promise.allSettled([
      router.route(answer),
      router.route(conflict),
      router.route(answer),
    ])
    expect(interleaved.map((result) => result.status)).toEqual(['fulfilled', 'rejected', 'fulfilled'])
    expect(interleaved[1]).toMatchObject({
      status: 'rejected',
      reason: { scope: 'session' },
    })
    operation.close()
  })

  it('drops exact candidate replays before they consume the session backlog', async () => {
    const router = new V2OperationRouter(() => undefined)
    const operationId = identity(60)
    const peerPathId = identity(61)
    const attemptId = identity(62)
    const operation = router.create(
      operationId,
      V2_MESSAGE_KIND.peerOffer,
      peerOfferBody(peerPathId, attemptId),
    )
    const candidate = peerCandidate(operationId, peerPathId, attemptId, 1)

    const routed = await Promise.all(Array.from(
      { length: EXACT_CANDIDATE_REPLAY_STRESS_COUNT },
      () => router.route(candidate),
    ))
    expect(routed).toHaveLength(EXACT_CANDIDATE_REPLAY_STRESS_COUNT)
    await expect(operation.next()).resolves.toEqual(candidate)

    const answer = peerAnswer(operationId, peerPathId, attemptId, 'v=0\r\ns=after-replay\r\n')
    await router.route(answer)
    await expect(operation.next()).resolves.toEqual(answer)
    operation.close()
  })

  it('admits one overflow representative after the bounded unique candidate authority', async () => {
    const router = new V2OperationRouter(() => undefined)
    const operationId = identity(63)
    const peerPathId = identity(64)
    const attemptId = identity(65)
    const operation = router.create(
      operationId,
      V2_MESSAGE_KIND.peerOffer,
      peerOfferBody(peerPathId, attemptId),
    )
    const candidates = Array.from(
      { length: V2_MAXIMUM_PEER_CANDIDATES + 1 },
      (_, index) => peerCandidate(operationId, peerPathId, attemptId, index + 1),
    )
    for (const candidate of candidates) await router.route(candidate)

    const overflow = candidates.at(-1)!
    await Promise.all(Array.from(
      { length: EXACT_CANDIDATE_REPLAY_STRESS_COUNT },
      () => router.route(overflow),
    ))
    await router.route(peerCandidate(operationId, peerPathId, attemptId, 66))

    const delivered = []
    for (let index = 0; index < candidates.length; index += 1) {
      delivered.push(await operation.next())
    }
    expect(delivered).toEqual(candidates)

    const answer = peerAnswer(operationId, peerPathId, attemptId, 'v=0\r\ns=after-overflow\r\n')
    await router.route(answer)
    await expect(operation.next()).resolves.toEqual(answer)
    operation.close()
  })

  it('rolls back a candidate reservation when global queue admission rejects it', async () => {
    const router = new V2OperationRouter(() => undefined)
    const blockerId = identity(67)
    const blocker = router.create(blockerId, V2_MESSAGE_KIND.listChildren, EMPTY_REQUEST_BODY)
    const progress = scanProgress(blockerId, identity(68))
    for (let index = 0; index < V2_SESSION_CONTROL_BACKLOG; index += 1) {
      await router.route(progress)
    }

    const operationId = identity(69)
    const peerPathId = identity(70)
    const attemptId = identity(71)
    const operation = router.create(
      operationId,
      V2_MESSAGE_KIND.peerOffer,
      peerOfferBody(peerPathId, attemptId),
    )
    const candidate = peerCandidate(operationId, peerPathId, attemptId, 1)
    await expect(router.route(candidate)).rejects.toMatchObject({ scope: 'session' })

    await expect(blocker.next()).resolves.toEqual(progress)
    await expect(router.route(candidate)).resolves.toBeUndefined()
    await expect(operation.next()).resolves.toEqual(candidate)
    blocker.close()
    operation.close()
  })

  it('retains a peer-answer fingerprint across remote finalization', async () => {
    const router = new V2OperationRouter(() => undefined)
    const operationId = identity(25)
    const peerPathId = identity(26)
    const attemptId = identity(27)
    const operation = router.create(
      operationId,
      V2_MESSAGE_KIND.peerOffer,
      peerOfferBody(peerPathId, attemptId),
    )
    const answer = peerAnswer(operationId, peerPathId, attemptId, 'v=0\r\ns=retained\r\n')
    await router.route(answer)
    await expect(operation.next()).resolves.toEqual(answer)
    const final = encodeV2Message(
      V2_MESSAGE_KIND.operationError,
      operationId,
      encodeV2Body(new Map<number, unknown>([
        [0, 1], [1, 5], [2, 0x5001], [3, false], [4, null], [5, 'negotiation failed'],
      ])),
    )
    const raced = await Promise.allSettled([
      router.route(final),
      router.route(answer),
      router.route(peerAnswer(operationId, peerPathId, attemptId, 'v=0\r\ns=conflict\r\n')),
    ])
    expect(raced.map((result) => result.status)).toEqual(['fulfilled', 'fulfilled', 'rejected'])
    expect(raced[2]).toMatchObject({ status: 'rejected', reason: { scope: 'session' } })
    await expect(operation.next()).resolves.toEqual(final)

    await expect(router.route(answer)).resolves.toBeUndefined()
  })

  it('retains final fingerprints and rejects reuse or conflicting late traffic', async () => {
    let now = 1_000
    const router = new V2OperationRouter(() => undefined, () => now)
    const operationId = identity(1)
    const operation = router.create(
      operationId,
      V2_MESSAGE_KIND.openRevisions,
      EMPTY_REQUEST_BODY,
    )
    const final = encodeV2Message(
      V2_MESSAGE_KIND.openResults,
      operationId,
      encodeV2Body(new Map<number, unknown>([[0, 1], [1, []]])),
    )

    await router.route(final)
    await expect(operation.next()).resolves.toEqual(final)
    expect(() => router.create(
      operationId,
      V2_MESSAGE_KIND.openRevisions,
      EMPTY_REQUEST_BODY,
    )).toThrow(
      /Operation ID was reused/,
    )
    await expect(router.route(final)).resolves.toBeUndefined()

    const conflict = encodeV2Message(
      V2_MESSAGE_KIND.openResults,
      operationId,
      encodeV2Body(new Map<number, unknown>([[0, 1], [1, [1]]])),
    )
    await expect(router.route(conflict)).rejects.toMatchObject({ scope: 'session' })
    now += 30_001
    expect(() => router.create(
      operationId,
      V2_MESSAGE_KIND.openRevisions,
      EMPTY_REQUEST_BODY,
    )).not.toThrow()
  })
})

describe('v2 operation tombstone and admission ownership', () => {
  it('rejects authenticated traffic for an operation outside the active or tombstone sets', async () => {
    const router = new V2OperationRouter(() => undefined)
    const message = encodeV2Message(
      V2_MESSAGE_KIND.operationError,
      identity(9),
      encodeV2Body(new Map<number, unknown>([
        [0, 1], [1, 4], [2, 0x4006], [3, false], [4, null], [5, 'cancelled'],
      ])),
    )
    await expect(router.route(message)).rejects.toMatchObject({ scope: 'session' })
  })

  it('drops request-compatible traffic after local cancellation but rejects a wrong kind', async () => {
    const router = new V2OperationRouter(() => undefined)
    const operationId = identity(10)
    const operation = router.create(operationId, V2_MESSAGE_KIND.listChildren, EMPTY_REQUEST_BODY)
    operation.close()

    const lateProgress = encodeV2Message(
      V2_MESSAGE_KIND.scanProgress,
      operationId,
      encodeV2Body(new Map<number, unknown>([
        [0, 1], [1, identity(11)], [2, 1],
      ])),
    )
    const unrelatedLateControl = encodeV2Message(
      V2_MESSAGE_KIND.peerAnswer,
      operationId,
      encodeV2Body([]),
    )
    const wrongScopedLateError = operationError(operationId, 'block', 'wrong scope')

    await expect(router.route(lateProgress)).resolves.toBeUndefined()
    await expect(router.route(unrelatedLateControl)).rejects.toMatchObject({ scope: 'session' })
    await expect(router.route(wrongScopedLateError)).rejects.toMatchObject({ scope: 'session' })
  })

  it('retains peer binding and continuation fingerprints after local cancellation', async () => {
    const router = new V2OperationRouter(() => undefined)
    const operationId = identity(72)
    const peerPathId = identity(73)
    const attemptId = identity(74)
    const operation = router.create(
      operationId,
      V2_MESSAGE_KIND.peerOffer,
      peerOfferBody(peerPathId, attemptId),
    )
    operation.close()

    const candidate = peerCandidate(operationId, peerPathId, attemptId, 1)
    const answer = peerAnswer(operationId, peerPathId, attemptId, 'v=0\r\ns=late-answer\r\n')
    await expect(router.route(candidate)).resolves.toBeUndefined()
    await expect(router.route(answer)).resolves.toBeUndefined()
    await expect(router.route(answer)).resolves.toBeUndefined()

    const hostile = await Promise.allSettled([
      router.route(peerAnswer(operationId, peerPathId, attemptId, 'v=0\r\ns=conflict\r\n')),
      router.route(peerCandidate(operationId, identity(75), attemptId, 2)),
      router.route(encodeV2Message(
        V2_MESSAGE_KIND.catalogResult,
        operationId,
        encodeV2Body(new Map<number, unknown>([[0, 1], [1, identity(76)]])),
      )),
      router.route(operationError(operationId, 'directory', 'wrong scope')),
    ])
    expect(hostile.map((result) => result.status)).toEqual([
      'rejected',
      'rejected',
      'rejected',
      'rejected',
    ])
    for (const result of hostile) {
      expect(result).toMatchObject({ status: 'rejected', reason: { scope: 'session' } })
    }
  })

  it('shares peer continuation authority with the tombstone across a remote-final race', async () => {
    const router = new V2OperationRouter(() => undefined)
    const operationId = identity(77)
    const peerPathId = identity(78)
    const attemptId = identity(79)
    const operation = router.create(
      operationId,
      V2_MESSAGE_KIND.peerOffer,
      peerOfferBody(peerPathId, attemptId),
    )
    const final = operationError(operationId, 'peer', 'negotiation failed')
    const candidate = peerCandidate(operationId, peerPathId, attemptId, 1)

    const capturedRace = await Promise.allSettled([
      router.route(final),
      router.route(candidate),
      router.route(candidate),
    ])
    expect(capturedRace.map((result) => result.status)).toEqual([
      'fulfilled',
      'fulfilled',
      'fulfilled',
    ])
    await expect(operation.next()).resolves.toEqual(final)
    await expect(router.route(candidate)).resolves.toBeUndefined()
    await expect(router.route(final)).resolves.toBeUndefined()

    const answer = peerAnswer(operationId, peerPathId, attemptId, 'v=0\r\ns=late-answer\r\n')
    await expect(router.route(answer)).resolves.toBeUndefined()
    await expect(router.route(answer)).resolves.toBeUndefined()
    await expect(router.route(
      peerAnswer(operationId, peerPathId, attemptId, 'v=0\r\ns=conflict\r\n'),
    )).rejects.toMatchObject({ scope: 'session' })
    await expect(router.route(
      peerCandidate(operationId, peerPathId, identity(80), 2),
    )).rejects.toMatchObject({ scope: 'session' })
    await expect(router.route(
      operationError(operationId, 'peer', 'different final'),
    )).rejects.toMatchObject({ scope: 'session' })
  })

  it('rejects nonterminal traffic introduced after a remote final', async () => {
    const router = new V2OperationRouter(() => undefined)
    const operationId = identity(13)
    const operation = router.create(operationId, V2_MESSAGE_KIND.listChildren, EMPTY_REQUEST_BODY)
    const final = encodeV2Message(
      V2_MESSAGE_KIND.catalogResult,
      operationId,
      encodeV2Body(new Map<number, unknown>([[0, 1], [1, identity(14)]])),
    )
    await router.route(final)
    await expect(operation.next()).resolves.toEqual(final)

    const lateProgress = encodeV2Message(
      V2_MESSAGE_KIND.scanProgress,
      operationId,
      encodeV2Body(new Map<number, unknown>([
        [0, 1], [1, identity(15)], [2, 1],
      ])),
    )
    await expect(router.route(lateProgress)).rejects.toMatchObject({ scope: 'session' })
  })

  it('enforces and releases the protocol-session control backlog', async () => {
    const router = new V2OperationRouter(() => undefined)
    const operationId = identity(16)
    const operation = router.create(operationId, V2_MESSAGE_KIND.listChildren, EMPTY_REQUEST_BODY)
    const progress = encodeV2Message(
      V2_MESSAGE_KIND.scanProgress,
      operationId,
      encodeV2Body(new Map<number, unknown>([
        [0, 1], [1, identity(17)], [2, 1],
      ])),
    )
    for (let index = 0; index < V2_SESSION_CONTROL_BACKLOG; index += 1) {
      await router.route(progress)
    }
    await expect(router.route(progress)).rejects.toMatchObject({ scope: 'session' })

    await expect(operation.next()).resolves.toEqual(progress)
    await expect(router.route(progress)).resolves.toBeUndefined()
  })

  it('drops authenticated frames after the first session terminal', async () => {
    const router = new V2OperationRouter(() => undefined)
    await router.route(encodeV2Message(
      V2_MESSAGE_KIND.sessionTerminal,
      undefined,
      encodeV2Body(new Map<number, unknown>([[0, 1], [1, 0x1001], [2, 'done']])),
    ))
    const late = encodeV2Message(
      V2_MESSAGE_KIND.openResults,
      identity(18),
      encodeV2Body(new Map<number, unknown>([[0, 1], [1, []]])),
    )
    await expect(router.route(late)).resolves.toBeUndefined()
  })

  it('discards remotely-finalized buffers when the session terminates', async () => {
    const router = new V2OperationRouter(() => undefined)
    const operationId = identity(19)
    const operation = router.create(operationId, V2_MESSAGE_KIND.listChildren, EMPTY_REQUEST_BODY)
    await router.route(encodeV2Message(
      V2_MESSAGE_KIND.scanProgress,
      operationId,
      encodeV2Body(new Map<number, unknown>([
        [0, 1], [1, identity(20)], [2, 1],
      ])),
    ))
    await router.route(encodeV2Message(
      V2_MESSAGE_KIND.catalogResult,
      operationId,
      encodeV2Body(new Map<number, unknown>([[0, 1], [1, identity(21)]])),
    ))

    const terminal = encodeV2Message(
      V2_MESSAGE_KIND.sessionTerminal,
      undefined,
      encodeV2Body(new Map<number, unknown>([[0, 1], [1, 0x1001], [2, 'done']])),
    )
    await router.route(terminal)
    await expect(operation.next()).rejects.toMatchObject({ scope: 'session' })
  })

  it('bounds active operations independently of the tombstone budget', () => {
    const router = new V2OperationRouter(() => undefined)
    for (let index = 0; index < V2_MAXIMUM_ACTIVE_OPERATIONS; index += 1) {
      const operationId = new Uint8Array(16)
      operationId[0] = (index % 255) + 1
      operationId[1] = Math.floor(index / 255)
      router.create(operationId, V2_MESSAGE_KIND.openRevisions, EMPTY_REQUEST_BODY)
    }

    const overflowId = new Uint8Array(16)
    overflowId[0] = 1
    overflowId[1] = 2
    expect(() => router.create(
      overflowId,
      V2_MESSAGE_KIND.openRevisions,
      EMPTY_REQUEST_BODY,
    )).toThrow(
      /Active operation budget is exhausted/,
    )
  })
})

function peerAnswer(
  operationId: Uint8Array<ArrayBuffer>,
  peerPathId: Uint8Array<ArrayBuffer>,
  attemptId: Uint8Array<ArrayBuffer>,
  sdp: string,
) {
  return encodeV2Message(
    V2_MESSAGE_KIND.peerAnswer,
    operationId,
    encodeV2Body([1, peerPathId, attemptId, sdp]),
  )
}

function peerOfferBody(
  peerPathId: Uint8Array<ArrayBuffer>,
  attemptId: Uint8Array<ArrayBuffer>,
): Uint8Array<ArrayBuffer> {
  return encodeV2Body([1, peerPathId, attemptId, 'v=0\r\ns=offer\r\n'])
}

function peerCandidate(
  operationId: Uint8Array<ArrayBuffer>,
  peerPathId: Uint8Array<ArrayBuffer>,
  attemptId: Uint8Array<ArrayBuffer>,
  seed: number,
) {
  return encodeV2Message(
    V2_MESSAGE_KIND.peerCandidate,
    operationId,
    encodeV2Body([
      1,
      peerPathId,
      attemptId,
      `candidate:${seed} 1 udp 1 192.0.2.${seed} ${5_000 + seed} typ host`,
      'data',
      0,
      'windshare',
    ]),
  )
}

function scanProgress(
  operationId: Uint8Array<ArrayBuffer>,
  attemptId: Uint8Array<ArrayBuffer>,
) {
  return encodeV2Message(
    V2_MESSAGE_KIND.scanProgress,
    operationId,
    encodeV2Body(new Map<number, unknown>([
      [0, 1], [1, attemptId], [2, 1],
    ])),
  )
}

function operationError(
  operationId: Uint8Array<ArrayBuffer>,
  scope: 'block' | 'directory' | 'peer',
  message: string,
) {
  const [scopeId, code] = operationErrorRegistry(scope)
  return encodeV2Message(
    V2_MESSAGE_KIND.operationError,
    operationId,
    encodeV2Body(new Map<number, unknown>([
      [0, 1], [1, scopeId], [2, code], [3, false], [4, null], [5, message],
    ])),
  )
}

function operationErrorRegistry(
  scope: 'block' | 'directory' | 'peer',
): readonly [number, number] {
  switch (scope) {
    case 'directory':
      return [2, 0x2001]
    case 'block':
      return [4, 0x4001]
    case 'peer':
      return [5, 0x5001]
  }
}
