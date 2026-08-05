import { createHash } from 'node:crypto'

import { expect, type Page, type TestInfo } from '@playwright/test'

import { V2_BLOCK_BROKER_PARALLEL_READS } from '../../src/content/v2-broker'
import {
  classifyNativePeerConnection,
  type NativeRtcCapabilityDiagnostic,
} from '../../test/transport/webrtc/browser-capability'
import type { HotSwitchPageEvent } from './hot-switch-contract'
import {
  releasePageOutput,
  sealPageRelayCut,
  startPageTransfer,
} from './hot-switch-page-transfer'
import {
  DIRECT_TEST_BLOCK_BYTES,
  DirectProductStack,
  DIRECT_WEBKIT_RELAY_BLOCK_BYTES,
  type DirectStackTrace,
  relayReceiverUrl,
} from './direct-product-stack'
import {
  createCapabilityRedactor,
  withCapabilityRedaction,
  type CapabilityRedactor,
} from './capability-redactor'

/** One extra block makes the post-cut dispatch observable without a timing race. */
export const HOT_SWITCH_TRANSFER_BYTES =
  (V2_BLOCK_BROKER_PARALLEL_READS + 1) * DIRECT_TEST_BLOCK_BYTES
export const HOT_SWITCH_FILE_NAME = 'hot-switch.bin'

const EVENT_TIMEOUT_MILLISECONDS = 30_000
const MAXIMUM_RETAINED_EVENTS = 1_024

export type HotSwitchRouteMode = 'native-capability' | 'peer' | 'relay-fallback'

export interface HotSwitchScenarioOptions {
  readonly browserName: string
  readonly mode: HotSwitchRouteMode
  readonly page: Page
  readonly testInfo: TestInfo
}

/**
 * Browser names are part of the operation identity. A stable, validated token
 * prevents a browser project from publishing Chromium-shaped diagnostics.
 */
export function hotSwitchScenarioId(browserName: string): string {
  if (browserName !== 'chromium' && browserName !== 'firefox' && browserName !== 'webkit') {
    throw new TypeError(`Unsupported browser engine for hot-switch: ${browserName}`)
  }
  return `${browserName}-hot-switch`
}

/** Run the real sender/relay/receiver path with the requested route fence. */
export async function runHotSwitchScenario(options: HotSwitchScenarioOptions): Promise<void> {
  const scenarioId = hotSwitchScenarioId(options.browserName)
  const stackTraces: DirectStackTrace[] = []
  const stack = new DirectProductStack(scenarioId, (trace) => {
    stackTraces.push(trace)
    console.info(JSON.stringify(trace))
  })
  const events = new HotSwitchEventLog()
  let redactor: CapabilityRedactor | undefined
  let capability: NativeRtcCapabilityDiagnostic | undefined
  let routeMode: Exclude<HotSwitchRouteMode, 'native-capability'> | undefined
  await stack.start()
  try {
    if (options.mode === 'native-capability') {
      capability = await classifyNativePeerConnection(options.page, stack.baseURL)
      await options.testInfo.attach('cross-browser-rtc-capability', {
        body: JSON.stringify({ browserName: options.browserName, ...capability }),
        contentType: 'application/json',
      })
      routeMode = nativeRouteMode(options.browserName, capability)
    } else {
      routeMode = options.mode
    }
    if (routeMode === undefined) {
      throw new Error('Hot-switch capability branch did not resolve a route mode')
    }
    const payload = deterministicBytes(HOT_SWITCH_TRANSFER_BYTES)
    const expectedHash = createHash('sha256').update(payload).digest('hex')
    const proxy = await stack.createRelayCutProxy()
    const path = await stack.createFile(HOT_SWITCH_FILE_NAME, payload)
    const share = await stack.share(path, {
      blockSizeBytes: hotSwitchBlockSize(options.browserName, routeMode),
    })
    const navigationUrl = relayReceiverUrl(share, proxy.url)
    redactor = createCapabilityRedactor({
      completeUrl: navigationUrl,
      fragment: new URL(navigationUrl).hash,
      separateKey: share.key,
    })

    await options.page.exposeFunction('__windshareHotSwitchEvent', (event: unknown) => {
      events.accept(event)
    })
    await withCapabilityRedaction(() => options.page.goto(navigationUrl), {
      completeUrl: navigationUrl,
      fragment: new URL(navigationUrl).hash,
      separateKey: share.key,
    })
    await withCapabilityRedaction(() => startPageTransfer(options.page, {
      expectedHash,
      key: share.key,
      nativePeerUsable: routeMode === 'peer',
      rtcConfiguration: { iceServers: [] },
      transferBytes: HOT_SWITCH_TRANSFER_BYTES,
    }), {
      completeUrl: navigationUrl,
      fragment: new URL(navigationUrl).hash,
      separateKey: share.key,
    })

    const firstRelayDispatch = await events.waitFor(
      'dispatch',
      (event) => event.kind === 'dispatch' && event.observation.route === 'relay',
      'first relay dispatch',
    )
    if (routeMode === 'peer') {
      await completePeerHotSwitch(
        options,
        proxy,
        events,
        firstRelayDispatch.observation.dispatchSequence,
      )
    } else {
      // A relay-only capability branch never cuts the live relay. Releasing the
      // output fence at the first dispatch lets the real transfer reach terminal
      // state while preserving relay ownership for every subsequent block.
      await releasePageOutput(options.page)
    }

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

    assertDelivery(delivery, expectedHash)
    assertStackIdentity(stackTraces, scenarioId)
    expect(runtime.error).toBeUndefined()
    if (routeMode === 'relay-fallback') assertRelayFallback(events)
  } catch (error) {
    const diagnostic = {
      browserName: options.browserName,
      capability,
      requestedMode: options.mode,
      routeMode,
      scenarioId,
      stackTraces,
      events: events.snapshot(),
      processes: stack.diagnostic(),
    }
    await options.testInfo.attach('direct-hot-switch-diagnostic', {
      body: redactor?.text(diagnostic) ?? JSON.stringify(diagnostic, null, 2),
      contentType: 'application/json',
    }).catch(() => undefined)
    const message = error instanceof Error ? error.message : String(error)
    throw new Error(redactor?.redactText(message) ?? message, {
      // eslint-disable-next-line preserve-caught-error -- detached redacted cause is the only permitted boundary value
      cause: redactor?.value(error),
    })
  } finally {
    await releasePageOutput(options.page).catch(() => undefined)
    try {
      await stack.dispose()
    } finally {
      redactor?.clear()
    }
  }
}

function nativeRouteMode(
  browserName: string,
  capability: NativeRtcCapabilityDiagnostic,
): Exclude<HotSwitchRouteMode, 'native-capability'> {
  if (browserName !== 'webkit') {
    // Firefox is a product hot-switch lane, not a relay-only compatibility
    // probe. A broken native API therefore remains visible as a product failure.
    expect(capability.rtcCapability).toBe('available')
    return 'peer'
  }
  return capability.rtcCapability === 'available' ? 'peer' : 'relay-fallback'
}

function hotSwitchBlockSize(
  browserName: string,
  routeMode: Exclude<HotSwitchRouteMode, 'native-capability'>,
): number {
  return browserName === 'webkit' && routeMode === 'relay-fallback'
    ? DIRECT_WEBKIT_RELAY_BLOCK_BYTES
    : DIRECT_TEST_BLOCK_BYTES
}

async function completePeerHotSwitch(
  options: HotSwitchScenarioOptions,
  proxy: { readonly cut: () => Promise<void> },
  events: HotSwitchEventLog,
  firstRelayDispatchSequence: number,
): Promise<void> {
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

  // The physical proxy cut is the authority boundary. Capture all dispatches
  // already observed before it; the relay-ineligible acknowledgement arrives
  // after the cut and must not move the boundary forward.
  const preCutDispatchBoundary = Math.max(
    firstRelayDispatchSequence,
    events.latestDispatchSequence(),
  )
  await proxy.cut()
  await sealPageRelayCut(options.page)
  await events.waitFor('relay-ineligible', () => true, 'relay ineligibility')
  expect(events.snapshot().some((event) =>
    event.kind === 'dispatch' &&
    event.observation.route === 'relay' &&
    event.observation.dispatchSequence > preCutDispatchBoundary,
  )).toBe(false)
  await releasePageOutput(options.page)

  const peerDispatch = await events.waitFor(
    'dispatch',
    (event) => event.kind === 'dispatch' &&
      event.observation.route === 'peer' &&
      event.observation.dispatchSequence > preCutDispatchBoundary,
    'post-cut peer dispatch',
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
}

function assertDelivery(
  delivery: Extract<HotSwitchPageEvent, { kind: 'delivery' }>,
  expectedHash: string,
): void {
  expect(delivery).toMatchObject({
    outcome: 'succeeded',
    evidence: {
      expectedBytes: HOT_SWITCH_TRANSFER_BYTES,
      receivedBytes: HOT_SWITCH_TRANSFER_BYTES,
      expectedSha256: expectedHash,
      receivedSha256: expectedHash,
      terminal: 'succeeded',
    },
    jobOutcome: { status: 'Succeeded', failureCount: 0 },
  })
}

function assertRelayFallback(events: HotSwitchEventLog): void {
  const snapshot = events.snapshot()
  expect(snapshot.some((event) => event.kind === 'attempt')).toBe(false)
  expect(snapshot.some((event) => event.kind === 'relay-ineligible')).toBe(false)
  expect(snapshot.some((event) =>
    (event.kind === 'lane-admitted' || event.kind === 'lane-detached') &&
    event.observation.route === 'peer',
  )).toBe(false)
  expect(snapshot.some((event) =>
    event.kind === 'dispatch' && event.observation.route === 'peer',
  )).toBe(false)
  expect(snapshot.some((event) =>
    event.kind === 'dispatch' && event.observation.route === 'relay',
  )).toBe(true)
}

function assertStackIdentity(
  traces: readonly DirectStackTrace[],
  scenarioId: string,
): void {
  expect(traces.length).toBeGreaterThan(0)
  expect(traces.every((trace) => trace.scenarioId === scenarioId)).toBe(true)
  expect(traces.every((trace) => trace.operationId.startsWith(`${scenarioId}-`))).toBe(true)
}

function deterministicBytes(length: number): Uint8Array {
  return Uint8Array.from({ length }, (_value, index) => (index * 31 + 17) & 0xff)
}

type MatchingEvent<T extends HotSwitchPageEvent['kind']> = Extract<HotSwitchPageEvent, { kind: T }>

interface EventWaiter {
  readonly kind: HotSwitchPageEvent['kind']
  readonly predicate: (event: HotSwitchPageEvent) => boolean
  readonly resolve: (event: HotSwitchPageEvent) => void
  readonly reject: (reason: unknown) => void
  readonly timer: ReturnType<typeof setTimeout>
}

export class HotSwitchEventLog {
  readonly #events: HotSwitchPageEvent[] = []
  readonly #waiters = new Set<EventWaiter>()

  accept(value: unknown): void {
    const event = requireHotSwitchEvent(value)
    if (this.#events.length >= MAXIMUM_RETAINED_EVENTS) {
      throw new Error('Direct hot-switch event log exceeded its diagnostic bound')
    }
    this.#events.push(event)
    for (const waiter of [...this.#waiters]) {
      if (event.kind !== waiter.kind || !waiter.predicate(event)) continue
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
