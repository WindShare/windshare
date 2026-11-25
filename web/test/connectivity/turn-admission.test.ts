import { describe, expect, it, vi } from 'vitest'
import { ReceiverConnectivity } from '../../src/connectivity/receiver-policy'
import {
  browserPeerTabAdmission,
  browserTurnTabAdmission,
  PeerTabAdmission,
  type PeerTabAdmissionClock,
} from '../../src/connectivity/peer-set/tab-admission'
import { PeerAttemptBudget } from '../../src/connectivity/peer-set/budget'
import { THROUGHPUT_SAMPLE_MS } from '../../src/connectivity/peer-set/throughput'
import type { OfferChannelFactory, TURNServer } from '../../src/connectivity/peer-offer'
import type { PeerChannel } from '../../src/connectivity/peer-channel'
import { LaneSet, type BlockLane } from '../../src/content/lane-set'
import { decodeTURNDemand, encodeTURNGrants, type TURNDemand } from '../../src/protocol/turn-control'
import {
  createProtocolOperationIdentity,
  createProtocolSessionIdentity,
} from '../../src/session/identities'
import type { ReceiverSessionRuntime } from '../../src/session/runtime'

interface PendingOffer {
  readonly kind: 'direct' | 'turn'
  readonly server: TURNServer | undefined
  reject(reason: unknown): void
}

class ManualClock implements PeerTabAdmissionClock {
  #now = 0
  readonly #waits = new Set<{
    readonly at: number
    readonly signal: AbortSignal
    readonly resolve: () => void
    readonly abort: () => void
  }>()

  now(): number {
    return this.#now
  }

  sleep(ms: number, signal: AbortSignal): Promise<void> {
    if (signal.aborted) return Promise.reject(signal.reason)
    return new Promise((resolve, reject) => {
      const wait = {
        at: this.#now + ms,
        signal,
        resolve: () => {
          signal.removeEventListener('abort', wait.abort)
          resolve()
        },
        abort: () => {
          this.#waits.delete(wait)
          reject(signal.reason)
        },
      }
      signal.addEventListener('abort', wait.abort, { once: true })
      this.#waits.add(wait)
    })
  }

  async advance(ms: number): Promise<void> {
    this.#now += ms
    for (const wait of [...this.#waits]) {
      if (wait.at > this.#now) continue
      this.#waits.delete(wait)
      wait.resolve()
    }
    await flushMicrotasks()
  }
}

async function flushMicrotasks(): Promise<void> {
  for (let i = 0; i < 16; i += 1) await Promise.resolve()
}

function createFakeSession(seed: number) {
  const protocolSessionId = new Uint8Array(16).fill(seed)
  const protocolSessionIdentity = createProtocolSessionIdentity(protocolSessionId)
  let turnListener: ((body: Uint8Array<ArrayBuffer>) => void) | undefined
  const turnDemands: TURNDemand[] = []
  let nextGrantByte = seed

  const session = {
    initialLaneId: 1,
    isClosed: false,
    keys: Object.freeze({ protocolSessionId, initialLaneEpoch: 0 }),
    protocolSessionIdentity,
    availableLaneIds: () => [1],
    laneEpoch: (laneId: number) => (laneId === 1 ? 0 : undefined),
    subscribeLaneChanges: () => () => undefined,
    subscribePeerPathControls: () => () => undefined,
    sendPeerPathControl: async () => undefined,
    subscribeTURNControls: (listener: (body: Uint8Array<ArrayBuffer>) => void) => {
      turnListener = listener
      return () => {
        if (turnListener === listener) turnListener = undefined
      }
    },
    sendTURNControl: async (body: Uint8Array) => {
      turnDemands.push(decodeTURNDemand(body))
    },
    retireLane: async () => undefined,
    retireLaneIfCovered: () => undefined,
    operationCorrelation: () => Object.freeze({
      protocolSessionId: protocolSessionIdentity,
      protocolOperationId: createProtocolOperationIdentity(1n),
      lane: Object.freeze({ id: 1, epoch: 0 }),
    }),
    recordReceivedProtocolError: () => undefined,
  } as unknown as ReceiverSessionRuntime

  return {
    session,
    turnDemands,
    replyTURNGrants(serviceIds: readonly string[]) {
      const request = turnDemands.at(-1)
      if (!request || !turnListener) throw new Error('expected active TURN demand')
      turnListener(encodeTURNGrants({
        requestId: request.requestId,
        status: 'ok',
        hasFallback: false,
        grants: serviceIds.map((serviceId) => ({
          serviceId,
          grantId: new Uint8Array(16).fill(++nextGrantByte),
          urls: [`turn:${serviceId}.example`],
          username: 'u',
          password: 'p',
          validForMs: 60_000n,
        })),
      }))
    },
  }
}

function createControlledOffers() {
  const offers: PendingOffer[] = []
  const factory: OfferChannelFactory = {
    offer(_route, signal, _observer, _profile, turnServer): Promise<PeerChannel> {
      return new Promise((_resolve, reject) => {
        const entry: PendingOffer = {
          kind: turnServer === undefined ? 'direct' : 'turn',
          server: turnServer,
          reject,
        }
        offers.push(entry)
        const onAbort = () => reject(signal.reason)
        signal.addEventListener('abort', onAbort, { once: true })
        if (signal.aborted) onAbort()
      })
    },
  }
  return { factory, offers }
}

function createConnectivityFixture(options: {
  readonly seed: number
  readonly clock: ManualClock
  readonly tabAdmission: PeerTabAdmission
  readonly turnTabAdmission: PeerTabAdmission
  readonly budget?: PeerAttemptBudget
}) {
  const fake = createFakeSession(options.seed)
  const lanes = new LaneSet()
  vi.spyOn(lanes, 'schedulingDemand').mockReturnValue({
    queuedBytes: 1024,
    queuedBlocks: 1,
    networkWaitingBytes: 1024,
    capacityWaitingBytes: 0,
    inFlightBytes: 0,
    unfinishedBytes: 1024,
    usefulBytes: 0,
    outputBytes: 0,
    remainingBytes: 8 * 1024 * 1024,
    remainingKnown: true,
  })
  const controlled = createControlledOffers()
  let identitySeed = options.seed + 10
  const connectivity = new ReceiverConnectivity({
    policy: 'auto',
    session: fake.session,
    lanes,
    createBlockLane: ({ laneId }) => ({
      id: laneId,
      fetchBlock: () => Promise.reject(new Error('unused in admission test')),
    }) as BlockLane,
    offers: controlled.factory,
    randomBytes: (length) => new Uint8Array(length).fill(++identitySeed),
    nativePeerUsable: () => true,
    peerRecovery: {
      clock: options.clock,
      tabAdmission: options.tabAdmission,
      turnTabAdmission: options.turnTabAdmission,
      ...(options.budget === undefined ? {} : { budget: options.budget }),
    },
  })
  return { connectivity, fake, controlled }
}

describe('TURN tab admission isolation and dependency injection', () => {
  it('provides a lazy browser TURN admission singleton distinct from direct admission', () => {
    const first = browserTurnTabAdmission()
    expect(browserTurnTabAdmission()).toBe(first)
    expect(first).not.toBe(browserPeerTabAdmission())
  })

  it('isolates TURN from saturated direct admission and honors injected turnTabAdmission and clock', async () => {
    const clock = new ManualClock()
    const directAdmission = new PeerTabAdmission({ concurrentAttempts: 1 }, clock)
    const turnAdmission = new PeerTabAdmission(
      { concurrentAttempts: 1, startsPerInterval: 1, intervalMilliseconds: 100 },
      clock,
    )
    const { connectivity, fake, controlled } = createConnectivityFixture({
      seed: 1,
      clock,
      tabAdmission: directAdmission,
      turnTabAdmission: turnAdmission,
    })

    const activation = connectivity.begin('download')
    await flushMicrotasks()
    expect(controlled.offers.filter((o) => o.kind === 'direct')).toHaveLength(1)

    await clock.advance(THROUGHPUT_SAMPLE_MS)
    fake.replyTURNGrants(['turn-a', 'turn-b'])
    await flushMicrotasks()

    const turnOffers = () => controlled.offers.filter((o) => o.kind === 'turn')
    expect(turnOffers().map((o) => o.server?.urls[0])).toEqual(['turn:turn-a.example'])

    turnOffers()[0]!.reject(new Error('turn-a failed'))
    await flushMicrotasks()
    expect(turnOffers()).toHaveLength(1)

    await clock.advance(99)
    expect(turnOffers()).toHaveLength(1)

    await clock.advance(1)
    expect(turnOffers().map((o) => o.server?.urls[0])).toEqual([
      'turn:turn-a.example',
      'turn:turn-b.example',
    ])

    activation.close()
    await connectivity.close()
  })

  it('preserves TURN receiver fairness identity across generations via shared budget', async () => {
    const clock = new ManualClock()
    const directAdmission = new PeerTabAdmission({ concurrentAttempts: 4 }, clock)
    const turnAdmission = new PeerTabAdmission({ concurrentAttempts: 1 }, clock)
    const sharedBudget = new PeerAttemptBudget()
    const started: string[] = []

    const firstGen = createConnectivityFixture({
      seed: 10,
      clock,
      tabAdmission: directAdmission,
      turnTabAdmission: turnAdmission,
      budget: sharedBudget,
    })
    const secondGen = createConnectivityFixture({
      seed: 30,
      clock,
      tabAdmission: directAdmission,
      turnTabAdmission: turnAdmission,
      budget: sharedBudget,
    })
    const otherReceiver = createConnectivityFixture({
      seed: 50,
      clock,
      tabAdmission: directAdmission,
      turnTabAdmission: turnAdmission,
    })

    const track = (label: string, controlled: ReturnType<typeof createControlledOffers>) => {
      const original = controlled.factory.offer
      controlled.factory.offer = (route, signal, observer, profile, turnServer) => {
        if (turnServer !== undefined) started.push(label)
        return original(route, signal, observer, profile, turnServer)
      }
    }
    track('first-gen', firstGen.controlled)
    track('second-gen', secondGen.controlled)
    track('other', otherReceiver.controlled)

    const a1 = firstGen.connectivity.begin('download')
    const a2 = secondGen.connectivity.begin('download')
    const a3 = otherReceiver.connectivity.begin('download')
    await clock.advance(THROUGHPUT_SAMPLE_MS)

    firstGen.fake.replyTURNGrants(['g1'])
    await flushMicrotasks()
    expect(started).toEqual(['first-gen'])

    secondGen.fake.replyTURNGrants(['g2'])
    await flushMicrotasks()
    otherReceiver.fake.replyTURNGrants(['other'])
    await flushMicrotasks()
    expect(started).toEqual(['first-gen'])

    firstGen.controlled.offers.find((o) => o.kind === 'turn')!.reject(new Error('done'))
    await flushMicrotasks()
    expect(started).toEqual(['first-gen', 'other'])

    otherReceiver.controlled.offers.find((o) => o.kind === 'turn')!.reject(new Error('done'))
    await flushMicrotasks()
    expect(started).toEqual(['first-gen', 'other', 'second-gen'])

    a1.close()
    a2.close()
    a3.close()
    await Promise.all([
      firstGen.connectivity.close(),
      secondGen.connectivity.close(),
      otherReceiver.connectivity.close(),
    ])
  })
})
