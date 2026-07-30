import type {
  AttemptEvidence,
  BrowserSelectedPairEvidence,
  LaneIdentity,
  PionSelectedPairEvidence,
} from '../../scripts/browser-evidence/attempt-evidence.ts'
import { AttemptCollector, type LogicalAttempt } from '../../scripts/browser-evidence/attempt-collector.ts'

export function identity(seed: number): string {
  return Buffer.alloc(16, seed).toString('base64url')
}

export const TEST_IDENTITY = Object.freeze({
  sessionId: identity(1),
  peerPathId: identity(2),
  attemptId: identity(3),
})

export function browserPair(): BrowserSelectedPairEvidence {
  return {
    candidatePairId: 'browser-pair-1',
    local: {
      candidateId: 'browser-local-1',
      candidateType: 'host',
      protocol: 'udp',
      address: '192.0.2.10',
      port: 40_000,
    },
    remote: {
      candidateId: 'browser-remote-1',
      candidateType: 'prflx',
      protocol: 'udp',
      address: '192.0.2.10',
      port: 40_001,
    },
  }
}

export function pionPair(): PionSelectedPairEvidence {
  return {
    candidatePairId: 'pion-pair-1',
    local: {
      candidateType: 'host',
      protocol: 'udp',
      address: '192.0.2.10',
      port: 40_001,
      addressFamily: 'ipv4',
    },
    remote: {
      candidateType: 'prflx',
      protocol: 'udp',
      address: '192.0.2.10',
      port: 40_000,
      addressFamily: 'ipv4',
    },
  }
}

export function admittedEvents(
  identityOverride: Partial<typeof TEST_IDENTITY> = {},
  options: {
    readonly selectedPairs?: boolean
    readonly lane?: LaneIdentity
  } = {},
): AttemptEvidence[] {
  const attemptIdentity = { ...TEST_IDENTITY, ...identityOverride }
  const selectedPairs = options.selectedPairs ?? true
  const counts = (value: number) => ({ localEmitted: value, remoteAccepted: value })
  const lane = options.lane ?? { laneId: 7, laneEpoch: 9 }
  const browser: AttemptEvidence[] = [
    event(attemptIdentity, 'browser', 1, 0, 'started'),
    event(attemptIdentity, 'browser', 2, 1, 'offer-created', { candidateCounts: counts(0) }),
    event(attemptIdentity, 'browser', 3, 2, 'offer-sent', { candidateCounts: counts(1) }),
    event(attemptIdentity, 'browser', 4, 3, 'answer-received', { candidateCounts: counts(1) }),
    event(attemptIdentity, 'browser', 5, 4, 'datachannel-open', { candidateCounts: counts(2) }),
    event(attemptIdentity, 'browser', 6, 5, 'lane-granted', { candidateCounts: counts(2), lane }),
    event(attemptIdentity, 'browser', 7, 6, 'lane-attached', { candidateCounts: counts(2), lane }),
    event(attemptIdentity, 'browser', 8, 7, 'admitted', {
      candidateCounts: counts(2),
      lane,
      selectedPair: selectedPairs ? browserPair() : null,
    }),
  ]
  const senderBase = { localGeneration: '18446744073709551615' }
  const sender: AttemptEvidence[] = [
    event(attemptIdentity, 'sender', 1, 0, 'started', senderBase),
    event(attemptIdentity, 'sender', 2, 1, 'offer-received', senderBase),
    event(attemptIdentity, 'sender', 3, 2, 'answer-created', {
      ...senderBase,
      candidateCounts: counts(0),
    }),
    event(attemptIdentity, 'sender', 4, 3, 'answer-sent', {
      ...senderBase,
      candidateCounts: counts(1),
    }),
    event(attemptIdentity, 'sender', 5, 4, 'datachannel-open', {
      ...senderBase,
      candidateCounts: counts(2),
    }),
    event(attemptIdentity, 'sender', 6, 5, 'lane-admission-started', {
      ...senderBase,
      candidateCounts: counts(2),
      lane,
    }),
    event(attemptIdentity, 'sender', 7, 6, 'admitted', {
      ...senderBase,
      candidateCounts: counts(2),
      lane,
      selectedPair: selectedPairs ? pionPair() : null,
    }),
  ]
  const interleaved: AttemptEvidence[] = []
  const maximum = Math.max(browser.length, sender.length)
  for (let index = 0; index < maximum; index += 1) {
    const browserEvent = browser[index]
    const senderEvent = sender[index]
    if (browserEvent !== undefined) interleaved.push(browserEvent)
    if (senderEvent !== undefined) interleaved.push(senderEvent)
  }
  return interleaved
}

export function browserFailureEvents(
  identityOverride: Partial<typeof TEST_IDENTITY> = {},
): AttemptEvidence[] {
  const attemptIdentity = { ...TEST_IDENTITY, ...identityOverride }
  return [
    event(attemptIdentity, 'browser', 1, 0, 'started'),
    event(attemptIdentity, 'browser', 2, 1, 'failed', {
      failedAtStage: 'offer-created',
      failureScope: 'attempt',
      typedErrorCode: 'peer-negotiation',
      failureMessage: 'PeerConnection construction failed',
    }),
  ]
}

export function collect(events: readonly AttemptEvidence[]): readonly LogicalAttempt[] {
  const collector = new AttemptCollector()
  for (const value of events) collector.ingest(value)
  return collector.finalize()
}

function event(
  attemptIdentity: typeof TEST_IDENTITY,
  side: 'browser' | 'sender',
  sideSequence: number,
  attemptElapsedMs: number,
  stage: AttemptEvidence['stage'],
  payload: Record<string, unknown> = {},
): AttemptEvidence {
  return {
    schemaVersion: 1,
    ...attemptIdentity,
    side,
    sideSequence,
    attemptElapsedMs,
    stage,
    ...payload,
  } as AttemptEvidence
}
