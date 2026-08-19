import { createHash } from 'node:crypto'

import { expect, type Page, type TestInfo } from '@playwright/test'

import { V2_BLOCK_BROKER_PARALLEL_READS } from '../../src/content/v2-broker'
import { V2_TYPED_PEER_ERROR_CODES } from '../../src/connectivity/diagnostics'
import {
  classifyNativePeerConnection,
  type NativeRtcCapabilityDiagnostic,
} from '../../test/transport/webrtc/browser-capability'
import type {
  HotSwitchPageEvent,
  HotSwitchPeerAttemptEvidence,
} from './hot-switch-contract'
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

type ResolvedHotSwitchRoute = Exclude<HotSwitchRouteMode, 'native-capability'>
type PeerAttemptFailure = {
  readonly kind: 'attempt'
  readonly evidence: Extract<
    HotSwitchPeerAttemptEvidence,
    { readonly stage: 'failed' }
  >
}
type PeerLaneAdmission = Extract<HotSwitchPageEvent, { readonly kind: 'lane-admitted' }>
type NativePeerOutcomeEvent = PeerAttemptFailure | PeerLaneAdmission

type NativePeerOutcome =
  | { readonly kind: 'peer'; readonly lane: PeerLaneAdmission }
  | { readonly kind: 'relay-fallback'; readonly failure: PeerAttemptFailure }

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
  let routeMode: ResolvedHotSwitchRoute | undefined
  let capability: NativeRtcCapabilityDiagnostic | undefined
  let fallbackFailure: PeerAttemptFailure | undefined
  await stack.start()
  try {
    const routePlan = await determineRoutePlan(options, stack.baseURL)
    capability = routePlan.capability
    routeMode = routePlan.routeMode
    const initialRouteMode = routePlan.routeMode
    const payload = deterministicBytes(HOT_SWITCH_TRANSFER_BYTES)
    const expectedHash = createHash('sha256').update(payload).digest('hex')
    const proxy = await stack.createRelayCutProxy()
    const path = await stack.createFile(HOT_SWITCH_FILE_NAME, payload)
    const share = await stack.share(path, {
      blockSizeBytes: hotSwitchBlockSize(
        options.browserName,
        initialRouteMode,
        routePlan.dynamicWebKitNativeAttempt,
      ),
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
      nativePeerUsable: initialRouteMode === 'peer',
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
    const settlement = await settleHotSwitchRoute(
      options,
      proxy,
      events,
      initialRouteMode,
      routePlan.dynamicWebKitNativeAttempt,
      firstRelayDispatch.observation.dispatchSequence,
    )
    routeMode = settlement.routeMode
    fallbackFailure = settlement.fallbackFailure

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
    if (routeMode === 'relay-fallback') assertRelayFallback(events, fallbackFailure)
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

interface HotSwitchRoutePlan {
  readonly capability: NativeRtcCapabilityDiagnostic | undefined
  readonly routeMode: ResolvedHotSwitchRoute
  readonly dynamicWebKitNativeAttempt: boolean
}

interface HotSwitchRouteSettlement {
  readonly routeMode: ResolvedHotSwitchRoute
  readonly fallbackFailure: PeerAttemptFailure | undefined
}

async function determineRoutePlan(
  options: HotSwitchScenarioOptions,
  baseURL: string,
): Promise<HotSwitchRoutePlan> {
  if (options.mode !== 'native-capability') {
    return {
      capability: undefined,
      routeMode: options.mode,
      dynamicWebKitNativeAttempt: false,
    }
  }

  const capability = await classifyNativePeerConnection(options.page, baseURL)
  await options.testInfo.attach('cross-browser-rtc-capability', {
    body: JSON.stringify({ browserName: options.browserName, ...capability }),
    contentType: 'application/json',
  })
  return {
    capability,
    routeMode: nativeRouteMode(options.browserName, capability),
    dynamicWebKitNativeAttempt: options.browserName === 'webkit' &&
      capability.rtcCapability === 'available',
  }
}

async function settleHotSwitchRoute(
  options: HotSwitchScenarioOptions,
  proxy: { readonly cut: () => Promise<void> },
  events: HotSwitchEventLog,
  routeMode: ResolvedHotSwitchRoute,
  dynamicWebKitNativeAttempt: boolean,
  firstRelayDispatchSequence: number,
): Promise<HotSwitchRouteSettlement> {
  if (routeMode !== 'peer') {
    await releaseRelayOutput(options.page)
    return { routeMode: 'relay-fallback', fallbackFailure: undefined }
  }

  if (!dynamicWebKitNativeAttempt) {
    await completePeerHotSwitch(options, proxy, events, firstRelayDispatchSequence)
    return { routeMode: 'peer', fallbackFailure: undefined }
  }

  const outcome = await waitForNativePeerOutcome(events)
  if (outcome.kind === 'peer') {
    await completePeerHotSwitch(
      options,
      proxy,
      events,
      firstRelayDispatchSequence,
      outcome.lane,
    )
    return { routeMode: 'peer', fallbackFailure: undefined }
  }

  // A typed attempt failure is a product route outcome, not a test failure. The
  // relay lane remains authoritative and can drain the exact payload once the
  // output fence is released.
  await releaseRelayOutput(options.page)
  return { routeMode: 'relay-fallback', fallbackFailure: outcome.failure }
}

async function releaseRelayOutput(page: Page): Promise<void> {
  // Releasing after the first relay dispatch lets the live relay drain without
  // introducing a second timing gate or changing the transfer contract.
  await releasePageOutput(page)
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
  routeMode: ResolvedHotSwitchRoute,
  dynamicWebKitNativeAttempt: boolean,
): number {
  // A native-capability WebKit run can discover product-level peer failure only
  // after the sender is already serving blocks. Start with the relay-safe frame
  // geometry so that either terminal route can finish without rebuilding the
  // sender or changing the exact payload contract.
  return browserName === 'webkit' &&
    (routeMode === 'relay-fallback' || dynamicWebKitNativeAttempt)
    ? DIRECT_WEBKIT_RELAY_BLOCK_BYTES
    : DIRECT_TEST_BLOCK_BYTES
}

async function waitForNativePeerOutcome(events: HotSwitchEventLog): Promise<NativePeerOutcome> {
  const event = await events.waitForAny(
    isNativePeerOutcomeEvent,
    'peer lane admission or typed native attempt failure',
  )
  if (event.kind === 'lane-admitted') return { kind: 'peer', lane: event }
  if (event.evidence.failureScope !== 'attempt' || event.evidence.failedAtStage === 'admitted') {
    throw new Error(
      `Native peer attempt failed outside the pre-admission fallback boundary ` +
      `(scope=${event.evidence.failureScope}, stage=${event.evidence.failedAtStage})`,
    )
  }
  return { kind: 'relay-fallback', failure: event }
}

function isNativePeerOutcomeEvent(event: HotSwitchPageEvent): event is NativePeerOutcomeEvent {
  if (event.kind === 'lane-admitted') return event.observation.route === 'peer'
  if (event.kind !== 'attempt' || event.evidence.stage !== 'failed') return false
  return V2_TYPED_PEER_ERROR_CODES.includes(event.evidence.typedErrorCode)
}

async function completePeerHotSwitch(
  options: HotSwitchScenarioOptions,
  proxy: { readonly cut: () => Promise<void> },
  events: HotSwitchEventLog,
  firstRelayDispatchSequence: number,
  peerLaneAdmission?: PeerLaneAdmission,
): Promise<void> {
  const peerAttempt = await events.waitFor(
    'attempt',
    (event) => event.kind === 'attempt' && event.evidence.stage === 'admitted',
    'authenticated peer admission',
  )
  const peerLane = peerLaneAdmission ?? await events.waitFor(
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
    protocolSessionIdBytes: expect.any(Array),
    peerPathIdBytes: expect.any(Array),
    attemptIdBytes: expect.any(Array),
    stage: 'admitted',
  })
  if (peerAttempt.evidence.stage !== 'admitted') {
    throw new Error('Peer admission wait returned a non-admitted diagnostic')
  }
  const admittedLane = requireEvidenceLane(peerAttempt.evidence)
  expect(peerLane.observation).toMatchObject({
    laneId: admittedLane.laneId,
    laneEpoch: admittedLane.laneEpoch,
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

function assertRelayFallback(
  events: HotSwitchEventLog,
  expectedFailure: PeerAttemptFailure | undefined,
): void {
  const snapshot = events.snapshot()
  const attempts = snapshot.filter((event) => event.kind === 'attempt')
  if (expectedFailure === undefined) {
    expect(attempts).toHaveLength(0)
  } else {
    expect(attempts.some((event) =>
      event.kind === 'attempt' &&
      event.evidence.stage === 'failed' &&
      sameIdentityBytes(
        event.evidence.attemptIdBytes,
        expectedFailure.evidence.attemptIdBytes,
      ),
    )).toBe(true)
    expect(attempts.some((event) =>
      event.kind === 'attempt' && event.evidence.stage === 'admitted',
    )).toBe(false)
  }
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

function requireEvidenceLane(evidence: { readonly lane?: {
  readonly laneId: number
  readonly laneEpoch: number
} }): { readonly laneId: number; readonly laneEpoch: number } {
  if (evidence.lane === undefined) throw new Error('Peer evidence did not retain its lane')
  return evidence.lane
}

function sameIdentityBytes(left: readonly number[], right: readonly number[]): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index])
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
  readonly kind: HotSwitchPageEvent['kind'] | undefined
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
      if ((waiter.kind !== undefined && event.kind !== waiter.kind) || !waiter.predicate(event)) continue
      clearTimeout(waiter.timer)
      this.#waiters.delete(waiter)
      waiter.resolve(event)
    }
  }

  waitForAny<T extends HotSwitchPageEvent>(
    predicate: (event: HotSwitchPageEvent) => event is T,
    label: string,
  ): Promise<T> {
    const existing = this.#events.find(predicate)
    if (existing !== undefined) return Promise.resolve(existing)
    return new Promise<T>((resolve, reject) => {
      const waiter: EventWaiter = {
        kind: undefined,
        predicate,
        resolve: (event) => resolve(event as T),
        reject,
        timer: setTimeout(() => {
          this.#waiters.delete(waiter)
          reject(new Error(`Timed out waiting for ${label}`))
        }, EVENT_TIMEOUT_MILLISECONDS),
      }
      this.#waiters.add(waiter)
    })
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
