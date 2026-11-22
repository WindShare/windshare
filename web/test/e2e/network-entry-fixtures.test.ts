import { describe, expect, it, vi } from 'vitest'

import type { PeerChannel, PeerPathRoute } from '../../src/connectivity/peer-channel'
import { FileGeometry } from '../../src/content/geometry'
import { BlockBroker, LaneSet, type BlockDemand } from '../../src/content/broker'
import type { BlockSchedulingObservation } from '../../src/content/lane-set'
import {
  HOT_SWITCH_INITIAL_BUFFERED_BLOCKS,
  HOT_SWITCH_TRANSFER_BLOCKS,
  hotSwitchTerminalEvidence,
  type HotSwitchLaneObservation,
  type HotSwitchPageEvent,
} from '../../e2e/fixtures/hot-switch-contract'
import { EvidenceBridge, RelayCutEvidence } from '../../e2e/fixtures/hot-switch-page-evidence'
import {
  OneShotRelease,
  OutputFence,
  PagePeerRecoveryHarness,
} from '../../e2e/fixtures/hot-switch-page-transfer'
import { NetworkEventLog } from '../../e2e/fixtures/network-event-log'
import { createCapabilityRedactor } from '../../e2e/fixtures/capability-redactor'
import { normalizeFileTransferFailure } from '../../src/transfer/job/failures'

describe('direct weekly network fixtures', () => {
  it('retains terminal exception evidence when the event prefix exceeds the diagnostic bound', () => {
    const normalized = normalizeFileTransferFailure(new Error('physical socket failed', {
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
            evidence: { exception: {
              errorName: 'Error', message: 'physical socket failed', cause: { message: 'connection reset' },
            } },
          },
        },
      },
      runtime: { kind: 'runtime-settled' },
    })
  })

  it.each([
    { relay: 'cut', route: 'direct' },
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
    const fetchBlock = async (demand: BlockDemand) => {
      // A physical channel delivers its reply in a later task, after queued output microtasks.
      await new Promise<void>(resolve => setTimeout(resolve, 1))
      return { descriptor, localBlockIndex: demand.localBlockIndex, data: new Uint8Array(blockBytes) }
    }
    const dispatches: BlockSchedulingObservation[] = []
    const lanes = new LaneSet({ onBlockScheduled: observation => dispatches.push(observation) })
    lanes.add({ id: 1, fetchBlock }, 'application-relay')
    const broker = new BlockBroker(lanes)
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
      ...recoveryPeer(),
      get pathRoute() { return pathRoute },
      subscribePathRoute(listener) {
        listeners.add(listener)
        return () => { listeners.delete(listener) }
      },
    }
    const harness = new PagePeerRecoveryHarness({ publish: async () => undefined })
    const gated = harness.wrap(peer, [1])
    expect(gated).not.toBe(peer)
    expect(gated.pathRoute).toBe('direct')

    const observed: PeerPathRoute[] = []
    const unsubscribe = gated.subscribePathRoute?.((route) => observed.push(route))
    try {
      for (const nextRoute of [undefined, 'direct'] as const) {
        pathRoute = nextRoute
        for (const listener of listeners) listener(nextRoute)
        expect(gated.pathRoute).toBe(nextRoute)
      }
      expect(observed).toEqual([undefined, 'direct'])
    } finally {
      unsubscribe?.()
      await gated.close()
    }
    expect(listeners.size).toBe(0)
  })

  it('holds the actual replacement response after earlier channels close before admission', async () => {
    const log = new NetworkEventLog()
    const harness = new PagePeerRecoveryHarness({ publish: async (event) => { log.accept(event) } })
    const response = Uint8Array.of(1)
    const first = harness.wrap(recoveryPeer(response), [1])
    const firstRead = expect(first.frames.getReader().read()).rejects.toMatchObject({ name: 'AbortError' })
    const start = await log.waitFor(
      'admission-response-gated',
      (event) => event.observation.release === 'attempt-timeout',
      'initial response',
    )
    expect(start.observation.attemptIdBytes).toEqual([1])
    await first.close()
    await firstRead

    const lane = { laneId: 2, laneEpoch: 2, route: 'direct' } as const
    const active = recoveryPeer(undefined, () => harness.observeDetachment(lane))
    expect(harness.wrap(active, [2])).toBe(active)
    await harness.detachCurrentPeer()
    await harness.wrap(recoveryPeer(), [3]).close()
    await harness.wrap(recoveryPeer(), [4]).close()

    const replacement = harness.wrap(recoveryPeer(response), [5])
    const reader = replacement.frames.getReader()
    const received = reader.read()
    const held = await log.waitFor(
      'admission-response-gated',
      (event) => event.observation.release === 'page-controlled',
      'replacement response',
    )
    expect(held.observation.attemptIdBytes).toEqual([5])
    harness.releaseAdmission()
    await expect(received).resolves.toEqual({ done: false, value: response })
    reader.releaseLock()
    await replacement.close()
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

  it('reports a runtime failure to pending and later milestone waits', async () => {
    const log = new NetworkEventLog()
    const error = 'TypeError: transfer planning failed'
    log.accept({
      kind: 'dispatch',
      observation: { dispatchSequence: 7, laneId: 1, laneEpoch: 1, route: 'application-relay' },
    })
    const pending = expect(log.waitFor('lane-admitted', () => true, 'peer admission')).rejects.toThrow(error)

    log.accept({ kind: 'runtime-settled', error })

    await pending
    await expect(log.waitFor('delivery', () => true, 'delivery')).rejects.toThrow(error)
    await expect(log.waitFor('dispatch', () => true, 'relay dispatch')).resolves.toMatchObject({
      observation: { dispatchSequence: 7 },
    })
    await expect(log.waitFor('runtime-settled', () => true, 'runtime settlement')).resolves.toMatchObject({ error })
  })

  it.each(['retained', 'live'] as const)(
    'correlates %s renewal traffic with its exact lane incarnation and cut boundary',
    async (arrival) => {
      const log = new NetworkEventLog()
      const cutBoundary = 27
      const replacement = { laneId: 1, laneEpoch: 2, route: 'direct' } as const
      const dispatch = (lane: HotSwitchLaneObservation, sequence: number) => ({
        kind: 'dispatch' as const,
        observation: { ...lane, dispatchSequence: sequence },
      })
      log.accept(dispatch(replacement, cutBoundary))
      // The failing weekly run carried sequences 28 through 38 on the predecessor
      // before sequence 39 proved that the renewed allocation carried content.
      log.accept(dispatch({ laneId: 2, laneEpoch: 1, route: 'direct' }, 28))
      log.accept(dispatch({ ...replacement, laneEpoch: 1 }, 38))
      const renewed = dispatch(replacement, 39)
      if (arrival === 'retained') log.accept(renewed)

      const observed = vi.fn()
      const waiting = log.waitForLaneDispatch(replacement, cutBoundary).then((event) => {
        observed(event)
        return event
      })
      if (arrival === 'live') {
        await Promise.resolve()
        expect(observed).not.toHaveBeenCalled()
        log.accept(renewed)
      }
      await expect(waiting).resolves.toEqual(renewed)
      expect(observed).toHaveBeenCalledExactlyOnceWith(renewed)
    },
  )

  it('rejects malformed bridge values before retaining them', () => {
    const log = new NetworkEventLog()
    expect(() => log.accept({ observation: {} })).toThrow(/invalid event/u)
    expect(() => log.accept({ kind: 'not-a-product-event' })).toThrow(/invalid event/u)
    expect(log.snapshot()).toEqual([])
  })
})

function recoveryPeer(frame?: Uint8Array, onClose?: () => void): PeerChannel {
  return {
    state: 'open',
    unavailable: new AbortController().signal,
    frames: new ReadableStream<Uint8Array>({
      start: (controller) => {
        if (frame !== undefined) controller.enqueue(frame)
        controller.close()
      },
    }),
    opened: Promise.resolve(),
    done: Promise.resolve(),
    reason: undefined,
    pathRoute: 'direct',
    send: async () => undefined,
    sendTerminal: async () => undefined,
    close: async () => { onClose?.() },
  }
}
