import { describe, expect, it } from 'vitest'

import {
  parseAttemptEvidence,
  type AttemptEvidence,
} from '../../scripts/browser-evidence/attempt-evidence.ts'
import {
  AttemptCollector,
  parseLogicalAttempts,
  reducePeerAttemptOutcome,
} from '../../scripts/browser-evidence/attempt-collector.ts'
import {
  admittedEvents,
  browserFailureEvents,
  collect,
  identity,
  TEST_IDENTITY,
} from './fixtures.ts'

describe('browser attempt evidence contract', () => {
  it('keeps browser and sender terminals independent and round-trippable', () => {
    const attempts = collect(admittedEvents())
    expect(attempts).toHaveLength(1)
    expect(attempts[0]?.outcome).toBe('admitted')
    expect(attempts[0]?.events.filter(({ evidence }) => evidence.stage === 'admitted')).toHaveLength(2)
    expect(reducePeerAttemptOutcome(attempts)).toBe('admitted')
    expect(parseLogicalAttempts(structuredClone(attempts))).toEqual(attempts)
  })

  it('accepts a browser-only early failure and makes failure absorbing across attempts', () => {
    const first = collect(admittedEvents({ attemptId: identity(10) }))
    const second = collect(browserFailureEvents({ attemptId: identity(11) }))
    expect(second[0]?.outcome).toBe('failed')
    expect(reducePeerAttemptOutcome([...first, ...second])).toBe('failed')
    expect(reducePeerAttemptOutcome([])).toBe('not-started')
    expect(() => reducePeerAttemptOutcome([...second, ...second])).toThrow(/more than once/u)
  })

  it('rejects missing, duplicate, post-terminal, gapped, and regressing side evidence', () => {
    const missing = new AttemptCollector()
    missing.ingest(admittedEvents()[0])
    expect(() => missing.finalize()).toThrow(/no terminal/u)

    const duplicate = new AttemptCollector()
    for (const event of browserFailureEvents()) duplicate.ingest(event)
    expect(() => duplicate.ingest({
      ...browserFailureEvents()[1],
      sideSequence: 3,
    })).toThrow(/duplicate terminal/u)

    const postTerminal = new AttemptCollector()
    for (const event of browserFailureEvents()) postTerminal.ingest(event)
    expect(() => postTerminal.ingest({
      ...admittedEvents()[2],
      sideSequence: 3,
    })).toThrow(/post-terminal/u)

    const gap = new AttemptCollector()
    gap.ingest(admittedEvents()[0])
    expect(() => gap.ingest({ ...admittedEvents()[2], sideSequence: 3 })).toThrow(/not contiguous/u)
    expect(() => gap.ingest(admittedEvents()[2])).toThrow(/previously rejected/u)
    expect(() => gap.finalize()).toThrow(/cannot finalize/u)

    const elapsed = new AttemptCollector()
    elapsed.ingest({ ...admittedEvents()[0], attemptElapsedMs: 2 })
    expect(() => elapsed.ingest({ ...admittedEvents()[2], attemptElapsedMs: 1 })).toThrow(/regressed/u)
  })

  it('requires exact stage payloads and lossless authenticated peer error mapping', () => {
    const started = admittedEvents()[0]
    expect(() => parseAttemptEvidence({ ...started, failureScope: 'attempt' })).toThrow(/unknown field/u)
    expect(() => parseAttemptEvidence({ ...started, sessionId: 'not-base64url' })).toThrow(/base64url/u)

    const failed = {
      ...browserFailureEvents()[1],
      sideSequence: 4,
      failedAtStage: 'answer-received',
      failureMessage: 'Peer negotiation failed',
      candidateCounts: { localEmitted: 1, remoteAccepted: 0 },
      authenticatedSenderOperationFailure: {
        scope: 'peer',
        code: 0x5001,
        message: 'Peer negotiation failed',
      },
    }
    expect(parseAttemptEvidence(failed)).toMatchObject({
      typedErrorCode: 'peer-negotiation',
      authenticatedSenderOperationFailure: { code: 0x5001 },
    })
    expect(() => parseAttemptEvidence({
      ...failed,
      typedErrorCode: 'peer-admission',
    })).toThrow(/does not match/u)
    expect(() => parseAttemptEvidence({
      ...failed,
      failureScope: 'session',
    })).toThrow(/attempt-scoped/u)
    Object.defineProperty(Object.prototype, '12345', {
      value: 'unexpected',
      configurable: true,
    })
    try {
      expect(() => parseAttemptEvidence({
        ...failed,
        typedErrorCode: 'unexpected',
        authenticatedSenderOperationFailure: {
          scope: 'peer',
          code: 12_345,
          message: 'Peer negotiation failed',
        },
      })).toThrow(/does not match/u)
    } finally {
      Reflect.deleteProperty(Object.prototype, '12345')
    }
    expect(() => parseAttemptEvidence({
      ...failed,
      sideSequence: 2,
      failedAtStage: 'offer-created',
      candidateCounts: undefined,
    })).toThrow(/must not be undefined|after offer dispatch/u)
  })

  it('rejects cross-side lane mismatch but keeps missing pair proof distinct from admission', () => {
    const missingPair = collect(admittedEvents({}, { selectedPairs: false }))
    expect(missingPair[0]?.outcome).toBe('admitted')

    const events = admittedEvents()
    const senderLane = events.find(({ side, stage }) => side === 'sender' && stage === 'lane-admission-started')
    if (senderLane === undefined || !('lane' in senderLane)) throw new Error('fixture lost sender lane')
    const changed = events.map((event) => event === senderLane
      ? { ...event, lane: { laneId: 8, laneEpoch: 9 } }
      : event)
    expect(() => collect(changed as never)).toThrow(/lane identities differ/u)
  })

  it.each([
    ['the same lane ID and epoch', { laneId: 7, laneEpoch: 9 }, /lane ID 7 is reused/u],
    ['a lane ID with a new epoch', { laneId: 7, laneEpoch: 10 }, /lane ID 7 is reused/u],
    ['a new lane ID with an old epoch', { laneId: 8, laneEpoch: 9 }, /lane epoch 9 is reused/u],
  ])('rejects %s across logical attempts in one ProtocolSession', (_label, lane, expected) => {
    expect(() => collect([
      ...admittedEvents({ attemptId: identity(10) }),
      ...admittedEvents({ attemptId: identity(11) }, { lane }),
    ])).toThrow(expected)
  })

  it('accepts fresh lane allocations in one session and identical allocations in another session', () => {
    expect(collect([
      ...admittedEvents({ attemptId: identity(10) }),
      ...admittedEvents(
        { attemptId: identity(11) },
        { lane: { laneId: 8, laneEpoch: 10 } },
      ),
    ])).toHaveLength(2)

    expect(collect([
      ...admittedEvents({ attemptId: identity(10) }),
      ...admittedEvents({ sessionId: identity(12), attemptId: identity(10) }),
    ])).toHaveLength(2)
  })

  for (const side of ['browser', 'sender'] as const) {
    it(`reserves first valid ${side} lane evidence before cross-side corroboration`, () => {
      const collector = new AttemptCollector()
      for (const event of laneBearingPrefix(
        admittedEvents({ attemptId: identity(10) }),
        side,
      )) {
        collector.ingest(event)
      }

      const reused = laneBearingPrefix(
        admittedEvents({ attemptId: identity(11) }),
        side,
      )
      for (const event of reused.slice(0, -1)) collector.ingest(event)
      const reusedLaneEvent = reused.at(-1)
      if (reusedLaneEvent === undefined) throw new Error('fixture lost lane-bearing event')
      expect(() => collector.ingest(reusedLaneEvent)).toThrow(/lane ID 7 is reused/u)
    })
  }

  for (const [order, firstSide, secondSide] of [
    ['browser-first', 'browser', 'sender'],
    ['sender-first', 'sender', 'browser'],
  ] as const) {
    it(`allows ${order} cross-side corroboration inside one logical attempt`, () => {
      const events = admittedEvents()
      const attempts = collect([
        ...events.filter(({ side }) => side === firstSide),
        ...events.filter(({ side }) => side === secondSide),
      ])
      expect(attempts).toHaveLength(1)
      expect(attempts[0]?.outcome).toBe('admitted')
    })
  }

  it('does not commit a lane claim from a rejected event transition', () => {
    const collector = new AttemptCollector()
    const prefix = laneBearingPrefix(admittedEvents(), 'sender')
    const laneEvent = prefix.at(-1)
    if (laneEvent === undefined || laneEvent.side !== 'sender') {
      throw new Error('fixture lost sender lane-bearing event')
    }
    for (const event of prefix.slice(0, -1)) collector.ingest(event)

    expect(() => collector.ingest({
      ...laneEvent,
      localGeneration: '1',
    })).toThrow(/local generation changed/u)
    const preserved = collector.finalizePreservingCompleted()
    expect(preserved.attempts).toEqual([])
    expect(preserved.integrityViolations).toEqual([
      expect.stringMatching(/no browser authority stream/u),
    ])
  })

  it('preserves completed attempts without committing a rejected lane reuse', () => {
    const collector = new AttemptCollector()
    for (const event of admittedEvents({ attemptId: identity(10) })) {
      collector.ingest(event)
    }

    let rejection: unknown
    for (const event of admittedEvents({ attemptId: identity(11) })) {
      try {
        collector.ingest(event)
      } catch (cause) {
        rejection = cause
        break
      }
    }
    expect(rejection).toBeInstanceOf(Error)
    expect((rejection as Error).message).toMatch(/lane ID 7 is reused/u)

    const preserved = collector.finalizePreservingCompleted()
    expect(preserved.attempts.map(({ attemptId }) => attemptId)).toEqual([identity(10)])
    expect(preserved.integrityViolations).toEqual([
      expect.stringMatching(/has no terminal/u),
    ])
  })

  it('rejects an admitted browser stream without corresponding sender admission', () => {
    const browserOnly = admittedEvents().filter(({ side }) => side === 'browser')
    expect(() => collect(browserOnly)).toThrow(/lacks both admitted side streams/u)
  })

  it('requires causally reachable cross-side progress and sender-authenticated participation', () => {
    const impossible = [
      ...browserFailureEvents(),
      ...admittedEvents().filter(({ side }) => side === 'sender'),
    ]
    expect(() => collect(impossible)).toThrow(/before browser offer dispatch/u)

    const browser = admittedEvents().filter(({ side }) => side === 'browser').slice(0, 3)
    const authenticatedFailure = {
      ...browser[2],
      sideSequence: 4,
      attemptElapsedMs: 3,
      stage: 'failed',
      failedAtStage: 'answer-received',
      failureScope: 'attempt',
      typedErrorCode: 'peer-negotiation',
      failureMessage: 'Peer negotiation failed',
      authenticatedSenderOperationFailure: {
        scope: 'peer',
        code: 0x5001,
        message: 'Peer negotiation failed',
      },
    }
    expect(() => collect([...browser, authenticatedFailure] as never)).toThrow(/no sender stream/u)

    const senderEvents = admittedEvents().filter(({ side }) => side === 'sender')
    const senderStarted = senderEvents[0]
    const senderOffer = senderEvents[1]
    if (senderStarted === undefined || senderOffer === undefined) throw new Error('fixture lost sender offer events')
    const senderFailedBeforeOffer = {
      ...senderOffer,
      stage: 'failed',
      failedAtStage: 'offer-received',
      failureScope: 'attempt',
      typedErrorCode: 'peer-negotiation',
      failureMessage: 'Offer was not received',
    }
    expect(() => collect([
      ...browser,
      authenticatedFailure,
      senderStarted,
      senderFailedBeforeOffer,
    ] as never)).toThrow(/lacks sender offer reception/u)

    const senderFailedAfterOffer = {
      ...senderOffer,
      sideSequence: 3,
      attemptElapsedMs: 2,
      stage: 'failed',
      failedAtStage: 'answer-created',
      failureScope: 'attempt',
      typedErrorCode: 'peer-negotiation',
      failureMessage: 'Peer negotiation failed',
    }
    expect(collect([
      ...browser,
      authenticatedFailure,
      senderStarted,
      senderOffer,
      senderFailedAfterOffer,
    ] as never)[0]?.outcome).toBe('failed')

    const admittedSenderContradiction = admittedEvents().map((event) =>
      event.side === 'browser' && event.stage === 'admitted'
        ? {
            ...event,
            stage: 'failed',
            failedAtStage: 'admitted',
            failureScope: 'attempt',
            typedErrorCode: 'peer-admission',
            failureMessage: 'Peer admission failed',
            authenticatedSenderOperationFailure: {
              scope: 'peer',
              code: 0x5004,
              message: 'Peer admission failed',
            },
          }
        : event)
    expect(() => collect(admittedSenderContradiction as never)).toThrow(/failed sender terminal/u)
  })

  it('rejects non-JSON own properties and explicit undefined optionals', () => {
    const withSymbol = { ...browserFailureEvents()[0], [Symbol('hidden')]: true }
    expect(() => parseAttemptEvidence(withSymbol)).toThrow(/symbol/u)
    const withAccessor = { ...browserFailureEvents()[0] }
    Object.defineProperty(withAccessor, 'hidden', { enumerable: true, get: () => true })
    expect(() => parseAttemptEvidence(withAccessor)).toThrow(/data field/u)
    expect(() => parseAttemptEvidence({
      ...browserFailureEvents()[0],
      selectedPair: undefined,
    })).toThrow(/unknown field|undefined/u)
  })

  it('rejects zero identities even when their encoded length is correct', () => {
    expect(() => parseAttemptEvidence({
      ...browserFailureEvents()[0],
      ...TEST_IDENTITY,
      attemptId: 'AAAAAAAAAAAAAAAAAAAAAA',
    })).toThrow(/nonzero/u)
  })
})

function laneBearingPrefix(
  events: readonly AttemptEvidence[],
  side: AttemptEvidence['side'],
): readonly AttemptEvidence[] {
  const sideEvents = events.filter((event) => event.side === side)
  const laneIndex = sideEvents.findIndex((event) => Object.hasOwn(event, 'lane'))
  if (laneIndex < 0) throw new Error(`fixture lost ${side} lane-bearing event`)
  return sideEvents.slice(0, laneIndex + 1)
}
