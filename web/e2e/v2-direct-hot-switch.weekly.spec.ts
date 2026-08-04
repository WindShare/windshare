import { createHash } from 'node:crypto'

import { expect, test } from '@playwright/test'

import { V2_BLOCK_BROKER_PARALLEL_READS } from '../src/content/v2-broker'
import type { HotSwitchPageEvent } from './fixtures/hot-switch-contract'
import {
  releasePageOutput,
  sealPageRelayCut,
  startPageTransfer,
} from './fixtures/hot-switch-page-transfer'
import {
  DIRECT_TEST_BLOCK_BYTES,
  DirectProductStack,
  relayReceiverUrl,
} from './fixtures/direct-product-stack'

const SCENARIO_ID = 'chromium-hot-switch'
const FILE_NAME = 'hot-switch.bin'
// The output fence holds the first bounded read window; one additional block
// creates a deterministic post-cut dispatch without manufacturing a timing race.
const TRANSFER_BYTES = (V2_BLOCK_BROKER_PARALLEL_READS + 1) * DIRECT_TEST_BLOCK_BYTES
const EVENT_TIMEOUT_MILLISECONDS = 30_000
const MAXIMUM_RETAINED_EVENTS = 1_024

test('continues on an authenticated peer lane after the relay is cut', async ({ page }, testInfo) => {
  const stack = new DirectProductStack(SCENARIO_ID)
  const events = new HotSwitchEventLog()
  await stack.start()
  try {
    const payload = deterministicBytes(TRANSFER_BYTES)
    const expectedHash = createHash('sha256').update(payload).digest('hex')
    const proxy = await stack.createRelayCutProxy()
    const path = await stack.createFile(FILE_NAME, payload)
    const share = await stack.share(path)

    await page.exposeFunction('__windshareHotSwitchEvent', (event: unknown) => events.accept(event))
    await page.goto(relayReceiverUrl(share, proxy.url))
    await startPageTransfer(page, {
      expectedHash,
      key: share.key,
      rtcConfiguration: { iceServers: [] },
      transferBytes: TRANSFER_BYTES,
    })

    await events.waitFor(
      'dispatch',
      (event) => event.kind === 'dispatch' && event.observation.route === 'relay',
      'first relay dispatch',
    )
    const peerAttempt = await events.waitFor(
      'attempt',
      (event) => event.kind === 'attempt' && event.evidence.stage === 'admitted',
      'authenticated peer admission',
    )
    const peerLane = await events.waitFor(
      'lane-admitted',
      (event) => event.kind === 'lane-admitted' && event.observation.route === 'peer',
      'peer content lane admission',
    )

    await proxy.cut()
    await sealPageRelayCut(page)
    await events.waitFor('relay-ineligible', () => true, 'relay ineligibility')
    const relayCutDispatchBoundary = events.latestDispatchSequence()
    await releasePageOutput(page)

    const peerDispatch = await events.waitFor(
      'dispatch',
      (event) => event.kind === 'dispatch' &&
        event.observation.route === 'peer' &&
        event.observation.dispatchSequence > relayCutDispatchBoundary,
      'post-cut peer dispatch',
    )
    const delivery = await events.waitFor(
      'delivery',
      (event) => event.kind === 'delivery',
      'delivery terminal',
    )
    const runtime = await events.waitFor(
      'runtime-settled',
      (event) => event.kind === 'runtime-settled',
      'runtime settlement',
    )

    expect(peerAttempt.evidence).toMatchObject({
      sessionId: expect.any(String),
      peerPathId: expect.any(String),
      attemptId: expect.any(String),
      stage: 'admitted',
    })
    if (peerAttempt.evidence.stage !== 'admitted') {
      throw new Error('Peer admission wait returned a non-admitted diagnostic')
    }
    expect(peerLane.observation).toMatchObject({
      laneId: peerAttempt.evidence.lane.laneId,
      laneEpoch: peerAttempt.evidence.lane.laneEpoch,
      route: 'peer',
    })
    expect(peerDispatch.observation).toMatchObject({
      laneId: peerLane.observation.laneId,
      laneEpoch: peerLane.observation.laneEpoch,
      route: 'peer',
    })
    expect(delivery).toMatchObject({
      outcome: 'succeeded',
      evidence: {
        expectedBytes: TRANSFER_BYTES,
        receivedBytes: TRANSFER_BYTES,
        expectedSha256: expectedHash,
        receivedSha256: expectedHash,
        terminal: 'succeeded',
      },
      jobOutcome: { status: 'Succeeded', failureCount: 0 },
    })
    expect(runtime.error).toBeUndefined()
  } catch (error) {
    await testInfo.attach('direct-hot-switch-diagnostic', {
      body: JSON.stringify({ events: events.snapshot(), processes: stack.diagnostic() }, null, 2),
      contentType: 'application/json',
    })
    throw error
  } finally {
    await releasePageOutput(page).catch(() => undefined)
    await stack.dispose()
  }
})

type MatchingEvent<T extends HotSwitchPageEvent['kind']> = Extract<HotSwitchPageEvent, { kind: T }>

interface EventWaiter {
  readonly kind: HotSwitchPageEvent['kind']
  readonly predicate: (event: HotSwitchPageEvent) => boolean
  readonly resolve: (event: HotSwitchPageEvent) => void
  readonly reject: (reason: unknown) => void
  readonly timer: ReturnType<typeof setTimeout>
}

class HotSwitchEventLog {
  readonly #events: HotSwitchPageEvent[] = []
  readonly #waiters = new Set<EventWaiter>()

  accept(value: unknown): void {
    const event = requireHotSwitchEvent(value)
    if (this.#events.length >= MAXIMUM_RETAINED_EVENTS) {
      throw new Error('Direct hot-switch event log exceeded its diagnostic bound')
    }
    this.#events.push(event)
    for (const waiter of [...this.#waiters]) {
      if (event.kind !== waiter.kind) continue
      if (!waiter.predicate(event)) continue
      clearTimeout(waiter.timer)
      this.#waiters.delete(waiter)
      waiter.resolve(event)
    }
  }

  waitFor<T extends HotSwitchPageEvent['kind']>(
    kind: T,
    predicate: (event: MatchingEvent<T>) => boolean,
    label: string,
  ): Promise<MatchingEvent<T>> {
    const existing = this.#events.find(
      (event): event is MatchingEvent<T> => event.kind === kind && predicate(event as MatchingEvent<T>),
    )
    if (existing !== undefined) return Promise.resolve(existing)
    return new Promise<MatchingEvent<T>>((resolve, reject) => {
      const waiter: EventWaiter = {
        kind,
        predicate: (event) => predicate(event as MatchingEvent<T>),
        resolve: (event) => resolve(event as MatchingEvent<T>),
        reject,
        timer: setTimeout(() => {
          this.#waiters.delete(waiter)
          reject(new Error(`Timed out waiting for ${label}`))
        }, EVENT_TIMEOUT_MILLISECONDS),
      }
      this.#waiters.add(waiter)
    })
  }

  snapshot(): readonly HotSwitchPageEvent[] {
    return Object.freeze([...this.#events])
  }

  latestDispatchSequence(): number {
    return this.#events.reduce(
      (latest, event) => event.kind === 'dispatch'
        ? Math.max(latest, event.observation.dispatchSequence)
        : latest,
      0,
    )
  }
}

function requireHotSwitchEvent(value: unknown): HotSwitchPageEvent {
  if (value === null || typeof value !== 'object' || Array.isArray(value) ||
      !Object.hasOwn(value, 'kind') || typeof (value as { kind?: unknown }).kind !== 'string') {
    throw new TypeError('Direct hot-switch bridge received an invalid event')
  }
  return value as HotSwitchPageEvent
}

function deterministicBytes(length: number): Uint8Array {
  return Uint8Array.from({ length }, (_value, index) => (index * 31 + 17) & 0xff)
}
