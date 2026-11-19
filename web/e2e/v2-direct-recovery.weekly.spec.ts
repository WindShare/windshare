import { createHash } from 'node:crypto'
import { writeFile } from 'node:fs/promises'

import { expect, test, type TestInfo } from '@playwright/test'

import { createPeerRecoveryPolicy } from '../src/connectivity/peer-set/path'
import {
  HOT_SWITCH_INITIAL_BUFFERED_BLOCKS,
  type HotSwitchPageEvent,
} from './fixtures/hot-switch-contract'
import {
  advancePageOutput,
  detachPagePeer,
  releasePageAdmissionResponse,
  releasePageOutput,
  startPageTransfer,
} from './fixtures/hot-switch-page-transfer'
import {
  capabilityUrl,
  DIRECT_TEST_BLOCK_BYTES,
  DirectProductStack,
  type DirectStackTrace,
} from './fixtures/direct-product-stack'
import { NetworkEventLog } from './fixtures/network-event-log'
import {
  createCapabilityRedactor,
  withCapabilityRedaction,
  type CapabilityRedactor,
} from './fixtures/capability-redactor'

const SCENARIO_ID = 'chromium-direct-recovery'
const FILE_NAME = 'direct-recovery.bin'
const TRANSFER_BLOCKS_AFTER_INITIAL_WINDOW = 4
const TRANSFER_BYTES =
  (HOT_SWITCH_INITIAL_BUFFERED_BLOCKS + TRANSFER_BLOCKS_AFTER_INITIAL_WINDOW) *
  DIRECT_TEST_BLOCK_BYTES
const RECOVERY_POLICY = createPeerRecoveryPolicy({
  negotiationBudgetMilliseconds: 10_000,
  admissionBudgetMilliseconds: 4_000,
  waveMaxAttempts: 3,
  waveElapsedBudgetMilliseconds: 30_000,
  sessionMaxAttempts: 5,
  sessionActiveElapsedBudgetMilliseconds: 60_000,
  retryInitialBackoffMilliseconds: 100,
  retryBackoffMultiplier: 2,
  retryBackoffMaximumMilliseconds: 500,
  retryJitterMinimumFactor: 1,
  retryJitterMaximumFactor: 1,
})

test('recovers authenticated Chromium peer traffic without interrupting relay', async ({ page }, testInfo) => {
  const events = new NetworkEventLog()
  const stackTraces: DirectStackTrace[] = []
  const stack = new DirectProductStack(SCENARIO_ID, (trace) => stackTraces.push(trace))
  let failure: unknown
  let redactor: CapabilityRedactor | undefined

  try {
    await stack.start()
    const payload = deterministicBytes(TRANSFER_BYTES)
    const expectedHash = createHash('sha256').update(payload).digest('hex')
    const path = await stack.createFile(FILE_NAME, payload)
    const share = await stack.share(path)
    const navigationUrl = capabilityUrl(share)
    redactor = createCapabilityRedactor({
      completeUrl: navigationUrl,
      fragment: new URL(navigationUrl).hash,
      separateKey: share.key,
    })

    await page.exposeFunction('__windshareHotSwitchEvent', (event: unknown) => events.accept(event))
    await withCapabilityRedaction(() => page.goto(navigationUrl), {
      completeUrl: navigationUrl,
      fragment: new URL(navigationUrl).hash,
      separateKey: share.key,
    })
    await withCapabilityRedaction(() => startPageTransfer(page, {
      expectedHash,
      key: share.key,
      nativePeerUsable: true,
      peerRecovery: { policy: RECOVERY_POLICY },
      rtcConfiguration: { iceServers: [] },
      transferBytes: TRANSFER_BYTES,
    }), {
      completeUrl: navigationUrl,
      fragment: new URL(navigationUrl).hash,
      separateKey: share.key,
    })

    const activationWave = await events.waitFor(
      'recovery',
      (event) => event.evidence.stage === 'wave-started' && event.evidence.trigger === 'activation',
      'activation recovery wave',
    )
    requireRecoveryStage(activationWave, 'wave-started')
    const firstRelayDispatch = await events.waitFor(
      'dispatch',
      (event) => event.observation.route === 'application-relay',
      'relay dispatch while peer admission is pending',
    )
    const timeoutGate = await events.waitFor(
      'admission-response-gated',
      (event) => event.observation.release === 'attempt-timeout',
      'first admission response gate',
    )
    const timedOutAttempt = await events.waitFor(
      'attempt',
      (event) => event.evidence.stage === 'failed' &&
        sameIdentityBytes(event.evidence.attemptIdBytes, timeoutGate.observation.attemptIdBytes),
      'gated admission timeout',
    )
    requireAttemptStage(timedOutAttempt, 'failed')
    expect(timedOutAttempt.evidence).toMatchObject({
      protocolSessionIdBytes: activationWave.evidence.protocolSessionIdBytes,
      peerPathIdBytes: activationWave.evidence.peerPathIdBytes,
      waveOrdinal: activationWave.evidence.waveOrdinal,
      failureScope: 'attempt-transient',
      typedErrorCode: 'peer-timeout',
      failure: {
        kind: 'local-transient',
        phase: 'admission',
        reason: 'admission-timeout',
      },
    } satisfies Partial<typeof timedOutAttempt.evidence>)
    expect(eventPosition(events, activationWave)).toBeLessThan(
      eventPosition(events, firstRelayDispatch),
    )
    expect(eventPosition(events, firstRelayDispatch)).toBeLessThan(
      eventPosition(events, timedOutAttempt),
    )

    const retryDecision = await events.waitFor(
      'recovery',
      (event) => event.evidence.stage === 'retry-decided' &&
        event.evidence.attemptIdBytes !== undefined &&
        sameIdentityBytes(event.evidence.attemptIdBytes, timeoutGate.observation.attemptIdBytes),
      'retry decision for the gated admission',
    )
    const replacement = await events.waitFor(
      'recovery',
      (event) => event.evidence.stage === 'attempt-replaced' &&
        sameIdentityBytes(
          event.evidence.previousAttemptIdBytes,
          timeoutGate.observation.attemptIdBytes,
        ),
      'fresh replacement attempt',
    )
    requireRecoveryStage(retryDecision, 'retry-decided')
    requireRecoveryStage(replacement, 'attempt-replaced')
    expect(retryDecision.evidence).toMatchObject({
      decision: 'retry-attempt',
      reason: 'local-transient',
    })
    expect(replacement.evidence.attemptIdBytes).toEqual(expect.any(Array))
    expect(replacement.evidence.attemptIdBytes).not.toEqual(timeoutGate.observation.attemptIdBytes)

    const recoveredAttempt = await events.waitFor(
      'attempt',
      (event) => event.evidence.stage === 'admitted' &&
        event.evidence.waveOrdinal === activationWave.evidence.waveOrdinal &&
        event.evidence.sessionAttemptOrdinal > timedOutAttempt.evidence.sessionAttemptOrdinal &&
        sameIdentityBytes(event.evidence.peerPathIdBytes, timedOutAttempt.evidence.peerPathIdBytes),
      'authenticated replacement admission',
    )
    requireAttemptStage(recoveredAttempt, 'admitted')
    expect(recoveredAttempt.evidence.protocolSessionIdBytes).toEqual(timedOutAttempt.evidence.protocolSessionIdBytes)
    expect(recoveredAttempt.evidence.attemptIdBytes).not.toEqual(timeoutGate.observation.attemptIdBytes)
    const recoveredAttemptLane = requireEvidenceLane(recoveredAttempt.evidence)
    const recoveredLane = await events.waitFor(
      'lane-admitted',
      (event) => event.observation.route === 'direct' &&
        event.observation.laneId === recoveredAttemptLane.laneId &&
        event.observation.laneEpoch === recoveredAttemptLane.laneEpoch,
      'recovered peer content lane',
    )

    const recoveredTrafficBoundary = events.latestDispatchSequence()
    // One page turn supplies concurrent demand so the allocator can sample the
    // recovered peer before the local relay completes the first new request.
    await advancePageOutput(page, 2)
    const recoveredPeerDispatch = await events.waitForLaneDispatch(
      recoveredLane.observation,
      recoveredTrafficBoundary,
    )
    expect(recoveredPeerDispatch.observation).toMatchObject({
      laneId: recoveredLane.observation.laneId,
      laneEpoch: recoveredLane.observation.laneEpoch,
      route: 'direct',
    })

    await detachPagePeer(page)
    const detachedLane = await events.waitFor(
      'lane-detached',
      (event) => event.observation.route === 'direct' &&
        event.observation.laneId === recoveredLane.observation.laneId &&
        event.observation.laneEpoch === recoveredLane.observation.laneEpoch,
      'page-controlled peer detachment',
    )
    const detached = await events.waitFor(
      'recovery',
      (event) => event.evidence.stage === 'peer-detached' &&
        event.evidence.lane?.laneId === recoveredLane.observation.laneId &&
        event.evidence.lane.laneEpoch === recoveredLane.observation.laneEpoch,
      'exact recovery detachment',
    )
    const detachmentWave = await events.waitFor(
      'recovery',
      (event) => event.evidence.stage === 'wave-started' &&
        event.evidence.trigger === 'detachment',
      'detachment recovery wave',
    )
    requireRecoveryStage(detached, 'peer-detached')
    requireRecoveryStage(detachmentWave, 'wave-started')
    expect(detached.evidence.lane).toEqual({
      laneId: recoveredLane.observation.laneId,
      laneEpoch: recoveredLane.observation.laneEpoch,
    })
    expect(detachmentWave.evidence.waveOrdinal).toBeGreaterThan(activationWave.evidence.waveOrdinal)
    expect(eventPosition(events, detachedLane)).toBeLessThan(eventPosition(events, detached))

    const detachmentGate = await events.waitFor(
      'admission-response-gated',
      (event) => event.observation.release === 'page-controlled',
      'detachment admission response gate',
    )
    const requestedLogicalLane = await events.waitFor(
      'attempt',
      (event) => event.evidence.stage === 'grant-requested' &&
        event.evidence.waveOrdinal === detachmentWave.evidence.waveOrdinal &&
        sameIdentityBytes(
          event.evidence.attemptIdBytes,
          detachmentGate.observation.attemptIdBytes,
        ),
      'detachment lane-grant request',
    )
    requireAttemptStage(requestedLogicalLane, 'grant-requested')
    expect(requestedLogicalLane.evidence).toMatchObject({
      protocolSessionIdBytes: recoveredAttempt.evidence.protocolSessionIdBytes,
      peerPathIdBytes: recoveredAttempt.evidence.peerPathIdBytes,
      requestedLaneId: recoveredLane.observation.laneId,
    })
    expect(requestedLogicalLane.evidence.attemptIdBytes).not.toEqual(recoveredAttempt.evidence.attemptIdBytes)
    expect(requestedLogicalLane.evidence.sessionAttemptOrdinal).toBeGreaterThan(
      recoveredAttempt.evidence.sessionAttemptOrdinal,
    )

    const detachmentRelayBoundary = events.latestDispatchSequence()
    await advancePageOutput(page)
    const relayDuringDetachment = await events.waitFor(
      'dispatch',
      (event) => event.observation.route === 'application-relay' &&
        event.observation.dispatchSequence > detachmentRelayBoundary,
      'relay dispatch during detachment recovery',
    )
    expect(eventPosition(events, detachmentGate)).toBeLessThan(
      eventPosition(events, relayDuringDetachment),
    )

    await releasePageAdmissionResponse(page)
    const readoptedAttempt = await events.waitFor(
      'attempt',
      (event) => event.evidence.stage === 'admitted' &&
        sameIdentityBytes(
          event.evidence.attemptIdBytes,
          detachmentGate.observation.attemptIdBytes,
        ),
      'peer readoption after detachment',
    )
    requireAttemptStage(readoptedAttempt, 'admitted')
    const readoptedLane = await events.waitFor(
      'lane-admitted',
      (event) => event.observation.route === 'direct' &&
        event.observation.laneId === recoveredLane.observation.laneId &&
        event.observation.laneEpoch > recoveredLane.observation.laneEpoch,
      'reattached logical peer lane',
    )
    expect(requireEvidenceLane(readoptedAttempt.evidence)).toEqual({
      laneId: readoptedLane.observation.laneId,
      laneEpoch: readoptedLane.observation.laneEpoch,
    })

    await releasePageOutput(page)
    const delivery = await events.waitFor('delivery', () => true, 'recovery delivery terminal')
    const runtime = await events.waitFor(
      'runtime-settled',
      () => true,
      'recovery runtime settlement',
    )
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
    expect(events.snapshot().some((event) =>
      event.kind === 'lane-detached' && event.observation.route === 'application-relay'
    )).toBe(false)
    expect(events.snapshot().some((event) => event.kind === 'relay-ineligible')).toBe(false)
    expect(stackTraces.length).toBeGreaterThan(0)
    expect(stackTraces.every((trace) => trace.scenarioId === SCENARIO_ID)).toBe(true)
  } catch (error) {
    await attachDiagnostic(testInfo, events, stack, stackTraces, redactor).catch(() => undefined)
    failure = redactedFailure(error, redactor)
  } finally {
    await releasePageOutput(page).catch(() => undefined)
    try {
      await stack.dispose()
    } catch (cleanupError) {
      failure = failure === undefined
        ? cleanupError
        : new AggregateError([failure, cleanupError], 'Recovery scenario and cleanup failed')
    }
    redactor?.clear()
  }

  if (failure !== undefined) throw failure
})

function eventPosition(
  events: NetworkEventLog,
  event: HotSwitchPageEvent,
): number {
  const position = events.snapshot().indexOf(event)
  if (position < 0) throw new Error('Observed recovery event is absent from its owning log')
  return position
}

type AttemptEvent = Extract<HotSwitchPageEvent, { readonly kind: 'attempt' }>
type RecoveryEvent = Extract<HotSwitchPageEvent, { readonly kind: 'recovery' }>
type EvidenceAtStage<
  Evidence extends { readonly stage: string },
  Stage extends string,
> = Evidence extends unknown
  ? Stage extends Evidence['stage'] ? Evidence & { readonly stage: Stage } : never
  : never

function requireAttemptStage<T extends AttemptEvent['evidence']['stage']>(
  event: AttemptEvent,
  stage: T,
): asserts event is AttemptEvent & {
  readonly evidence: EvidenceAtStage<AttemptEvent['evidence'], T>
} {
  if (event.evidence.stage !== stage) {
    throw new Error(`Expected attempt stage ${stage}, received ${event.evidence.stage}`)
  }
}

function requireRecoveryStage<T extends RecoveryEvent['evidence']['stage']>(
  event: RecoveryEvent,
  stage: T,
): asserts event is RecoveryEvent & {
  readonly evidence: EvidenceAtStage<RecoveryEvent['evidence'], T>
} {
  if (event.evidence.stage !== stage) {
    throw new Error(`Expected recovery stage ${stage}, received ${event.evidence.stage}`)
  }
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

async function attachDiagnostic(
  testInfo: TestInfo,
  events: NetworkEventLog,
  stack: DirectProductStack,
  stackTraces: readonly DirectStackTrace[],
  redactor: CapabilityRedactor | undefined,
): Promise<void> {
  const diagnostic = {
    component: 'browser-direct-recovery',
    scenarioId: SCENARIO_ID,
    events: events.snapshot(),
    stackTraces,
    processes: stack.diagnostic(),
  } as const
  // The event log already owns its bound. Formatting the whole array would
  // discard the late recovery attempts that explain an intermittent failure.
  const body = JSON.stringify(diagnostic, null, 2)
  const path = testInfo.outputPath('direct-recovery-diagnostic.json')
  await writeFile(path, redactor?.redactText(body) ?? body)
  await testInfo.attach('direct-recovery-diagnostic', {
    path,
    contentType: 'application/json',
  })
}

function redactedFailure(error: unknown, redactor: CapabilityRedactor | undefined): Error {
  const message = error instanceof Error ? error.message : String(error)
  return new Error(redactor?.redactText(message) ?? message, {
    cause: redactor?.value(error),
  })
}

function deterministicBytes(length: number): Uint8Array {
  return Uint8Array.from({ length }, (_value, index) => (index * 47 + 23) & 0xff)
}
