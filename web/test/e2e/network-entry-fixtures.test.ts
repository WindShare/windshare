import { describe, expect, it, vi } from 'vitest'

import type { PeerChannel, PeerPathRoute } from '../../src/connectivity/peer-channel'
import { FileGeometry } from '../../src/content/geometry'
import { V2BlockBroker, V2LaneSet, type V2BlockDemand } from '../../src/content/v2-broker'
import type { V2BlockSchedulingObservation } from '../../src/content/v2-lane-set'
import {
  HOT_SWITCH_INITIAL_BUFFERED_BLOCKS,
  HOT_SWITCH_TRANSFER_BLOCKS,
  hotSwitchTerminalEvidence,
  type HotSwitchPageEvent,
} from '../../e2e/fixtures/hot-switch-contract'
import { EvidenceBridge, RelayCutEvidence } from '../../e2e/fixtures/hot-switch-page-evidence'
import {
  OneShotRelease,
  OutputFence,
  PagePeerRecoveryHarness,
} from '../../e2e/fixtures/hot-switch-page-transfer'
import { parseLocalTurnReadyRecord } from '../../e2e/fixtures/local-turn-server'
import { NetworkEventLog } from '../../e2e/fixtures/network-event-log'
import { createCapabilityRedactor } from '../../e2e/fixtures/capability-redactor'
import { normalizeV2FileTransferFailure } from '../../src/transfer/job/failures'

describe('direct weekly network fixtures', () => {
  it('retains terminal exception evidence when the event prefix exceeds the diagnostic bound', () => {
    const normalized = normalizeV2FileTransferFailure(new Error('physical socket failed', {
      cause: new Error('connection reset'),
    }))
    if (normalized.kind !== 'fault') throw new Error('Expected a classified fixture failure')
    const diagnosticPrefixEvents = 96
    const events: HotSwitchPageEvent[] = Array.from({ length: diagnosticPrefixEvents }, (_unused, index) => ({
      kind: 'dispatch',
      observation: { dispatchSequence: index + 1, laneId: 1, laneEpoch: 0, route: 'application-relay' },
    }))
    events.push({
      kind: 'delivery', outcome: 'failed',
      evidence: { expectedBytes: 2, receivedBytes: 1, expectedSha256: 'expected', receivedSha256: null, terminal: 'failed' },
      failureClassification: normalized.diagnostic.classification,
    }, { kind: 'runtime-settled' })
    const diagnostic = createCapabilityRedactor({}).value({ events, ...hotSwitchTerminalEvidence(events) })
    expect(diagnostic).toMatchObject({
      delivery: {
        kind: 'delivery',
        failureClassification: {
          fact: {
            kind: 'unclassified',
            payload: { unclassified: { exception: {
              errorName: 'Error', message: 'physical socket failed', cause: expect.stringContaining('connection reset'),
            } } },
          },
        },
      },
      runtime: { kind: 'runtime-settled' },
    })
  })

  it.each([
    { relay: 'cut', route: 'direct' },
    { relay: 'cut', route: 'turn' },
    { relay: 'healthy', route: 'direct' },
  ] as const)('keeps post-fence $route demand with a $relay relay', async ({ relay, route }) => {
    vi.useFakeTimers()
    const blockBytes = 16
    const blockCount = relay === 'cut'
      ? HOT_SWITCH_TRANSFER_BLOCKS
      : HOT_SWITCH_INITIAL_BUFFERED_BLOCKS + 3
    const exactSize = BigInt(blockBytes * blockCount)
    const descriptor = {
      shareInstance: new Uint8Array(16), shareInstanceId: 'share',
      fileId: new Uint8Array(16), fileIdText: 'file',
      fileRevision: new Uint8Array(16), fileRevisionText: 'revision',
      exactSize, geometry: new FileGeometry(exactSize, BigInt(blockBytes)),
    }
    const fetchBlock = async (demand: V2BlockDemand) => {
      // A physical channel delivers its reply in a later task, after queued output microtasks.
      await new Promise<void>(resolve => setTimeout(resolve, 1))
      return { descriptor, localBlockIndex: demand.localBlockIndex, data: new Uint8Array(blockBytes) }
    }
    const dispatches: V2BlockSchedulingObservation[] = []
    const lanes = new V2LaneSet({ onBlockScheduled: observation => dispatches.push(observation) })
    lanes.add({ id: 1, fetchBlock }, 'application-relay')
    const broker = new V2BlockBroker(lanes)
    const fence = new OutputFence(relay === 'healthy')
    let writtenBytes = 0
    const transfer = (async () => {
      for await (const slice of broker.readRouteAuthorizedRange(
        descriptor, new Uint8Array(16), { start: 0n, end: exactSize },
        { routes: {
          active: true, allows: () => true, assertActive: () => undefined,
          subscribe: () => () => undefined,
        } },
      )) {
        await fence.waitForWrite()
        writtenBytes += slice.data.byteLength
      }
    })()
    try {
      // Fake time drains the asynchronous pipeline without a real network or sleep.
      await vi.runAllTimersAsync()
      expect(writtenBytes).toBe(0)
      expect(dispatches).toHaveLength(HOT_SWITCH_INITIAL_BUFFERED_BLOCKS)
      expect(dispatches.every(dispatch => dispatch.route === 'application-relay')).toBe(true)
      lanes.add({ id: 2, fetchBlock }, route)
      if (relay === 'cut') {
        lanes.remove(1)
      } else {
        fence.advance()
        fence.advance()
        await vi.runAllTimersAsync()
        expect(writtenBytes).toBe(2 * blockBytes)
        expect(dispatches.at(-1)).toMatchObject({ route, purpose: 'probe' })
        expect(lanes.size).toBe(2)
      }
      fence.release()
      await vi.runAllTimersAsync()
      await transfer
      expect(dispatches.slice(HOT_SWITCH_INITIAL_BUFFERED_BLOCKS)).toContainEqual(expect.objectContaining({
        route,
        localBlockIndex: BigInt(HOT_SWITCH_INITIAL_BUFFERED_BLOCKS + (relay === 'healthy' ? 1 : 0)),
      }))
      expect(writtenBytes).toBe(Number(exactSize))
    } finally {
      fence.release()
      await vi.runAllTimersAsync()
      await Promise.allSettled([transfer])
      broker.close()
      lanes.close()
      vi.useRealTimers()
    }
  })

  it.each(['seal-first', 'detach-first'] as const)(
    'freezes the page dispatch boundary with delayed bridge delivery and %s',
    async (order) => {
      const delivery = new OneShotRelease()
      const log = new NetworkEventLog()
      const bridge = new EvidenceBridge(async (event) => {
        await delivery.wait()
        log.accept(event)
      }, 4)
      const evidence = new RelayCutEvidence(bridge)
      const relay = { laneId: 1, laneEpoch: 1, route: 'application-relay' } as const
      const peer = { laneId: 2, laneEpoch: 1, route: 'direct' } as const
      evidence.admit(relay)
      evidence.dispatch({ ...relay, dispatchSequence: 1 })
      let sealed: Promise<void>
      if (order === 'seal-first') {
        sealed = evidence.seal()
        // The page may schedule buffered work before learning the socket has closed.
        evidence.dispatch({ ...relay, dispatchSequence: 2 })
        evidence.detach(relay)
      } else {
        evidence.detach(relay)
        evidence.dispatch({ ...peer, dispatchSequence: 2 })
        sealed = evidence.seal()
      }

      // Neither these later observations nor delayed bridge delivery may advance
      // the boundary and hide an erroneous post-detachment relay dispatch.
      evidence.dispatch({ ...relay, dispatchSequence: 3 })
      evidence.dispatch({ ...peer, dispatchSequence: 4 })
      await Promise.resolve()
      expect(log.latestDispatchSequence()).toBe(0)
      delivery.release()
      await sealed
      await evidence.seal()
      expect(await bridge.terminalFailure()).toBeUndefined()
      const events = log.snapshot()
      expect(events.filter(event => event.kind === 'relay-ineligible')).toEqual([
        { kind: 'relay-ineligible', dispatchSequenceBoundary: 2 },
      ])
      const cutIndex = events.findIndex(event => event.kind === 'relay-ineligible')
      expect(events.slice(cutIndex + 1)).toEqual([
        { kind: 'dispatch', observation: { ...relay, dispatchSequence: 3 } },
        { kind: 'dispatch', observation: { ...peer, dispatchSequence: 4 } },
      ])
      expect(log.latestDispatchSequence()).toBe(4)
    },
  )

  it('preserves live path evidence while recovery admission is gated', async () => {
    const listeners = new Set<(route: PeerPathRoute) => void>()
    let pathRoute: PeerPathRoute = 'direct'
    const peer: PeerChannel = {
      state: 'open',
      frames: new ReadableStream<Uint8Array>({ start: (controller) => controller.close() }),
      opened: Promise.resolve(),
      done: Promise.resolve(),
      reason: undefined,
      get pathRoute() { return pathRoute },
      subscribePathRoute(listener) {
        listeners.add(listener)
        return () => { listeners.delete(listener) }
      },
      send: async () => undefined,
      sendTerminal: async () => undefined,
      close: async () => undefined,
    }
    const harness = new PagePeerRecoveryHarness({ publish: async () => undefined }, true)
    const gated = harness.wrap(peer)
    expect(gated).not.toBe(peer)
    expect(gated.pathRoute).toBe('direct')

    const observed: PeerPathRoute[] = []
    const unsubscribe = gated.subscribePathRoute?.((route) => observed.push(route))
    try {
      for (const nextRoute of ['turn', undefined] as const) {
        pathRoute = nextRoute
        for (const listener of listeners) listener(nextRoute)
        expect(gated.pathRoute).toBe(nextRoute)
      }
      expect(observed).toEqual(['turn', undefined])
    } finally {
      unsubscribe?.()
      await gated.close()
    }
    expect(listeners.size).toBe(0)
  })

  it('accepts only the owned TURN readiness identity and loopback endpoint', () => {
    expect(parseLocalTurnReadyRecord(JSON.stringify({
      component: 'browser-local-turn-server',
      scenarioId: 'chromium-turn-route',
      operationId: 'chromium-turn-route-server',
      milestone: 'listener-ready',
      url: 'turn:127.0.0.1:34781?transport=udp',
      relayAddress: '192.0.2.10',
      username: 'windshare-browser',
      credential: 'windshare-local-turn',
    }))).toMatchObject({
      operationId: 'chromium-turn-route-server',
      url: 'turn:127.0.0.1:34781?transport=udp',
    })
    expect(() => parseLocalTurnReadyRecord(JSON.stringify({
      component: 'browser-local-turn-server',
      scenarioId: 'chromium-turn-route',
      operationId: 'chromium-turn-route-server',
      milestone: 'listener-ready',
      url: 'turn:192.0.2.1:34781?transport=udp',
      relayAddress: '192.0.2.10',
      username: 'windshare-browser',
      credential: 'windshare-local-turn',
    }))).toThrow(/owned UDP loopback/u)
    expect(() => parseLocalTurnReadyRecord(JSON.stringify({
      component: 'browser-local-turn-server',
      scenarioId: 'chromium-turn-route',
      operationId: 'chromium-turn-route-server',
      milestone: 'listener-ready',
      url: 'turn:127.0.0.1:34781?transport=udp',
      relayAddress: '127.0.0.1',
      username: 'windshare-browser',
      credential: 'windshare-local-turn',
    }))).toThrow(/usable owned IPv4 route/u)
  })

  it('correlates asynchronous route milestones without losing earlier events', async () => {
    const log = new NetworkEventLog()
    log.accept({
      kind: 'dispatch',
      observation: { dispatchSequence: 7, laneId: 1, laneEpoch: 1, route: 'application-relay' },
    })
    await expect(log.waitFor(
      'dispatch',
      (event) => event.observation.dispatchSequence === 7,
      'existing relay dispatch',
    )).resolves.toMatchObject({ kind: 'dispatch' })

    const admitted = log.waitFor(
      'lane-admitted',
      (event) => event.observation.route === 'direct',
      'peer admission',
    )
    log.accept({
      kind: 'lane-admitted',
      observation: { laneId: 2, laneEpoch: 3, route: 'direct' },
    })
    await expect(admitted).resolves.toMatchObject({
      observation: { laneId: 2, laneEpoch: 3, route: 'direct' },
    })
    expect(log.latestDispatchSequence()).toBe(7)
    expect(log.snapshot()).toHaveLength(2)
  })

  it('rejects malformed bridge values before retaining them', () => {
    const log = new NetworkEventLog()
    expect(() => log.accept({ observation: {} })).toThrow(/invalid event/u)
    expect(() => log.accept({ kind: 'not-a-product-event' })).toThrow(/invalid event/u)
    expect(log.snapshot()).toEqual([])
  })
})
