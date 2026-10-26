import { createHash } from 'node:crypto'

import { expect, test } from '@playwright/test'

import { HOT_SWITCH_TRANSFER_BLOCKS, sameLaneIdentity } from './fixtures/hot-switch-contract'
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
import { LocalTurnServer } from './fixtures/local-turn-server'
import { turnFixture } from '../../relay-rust/crates/relay-cloudflare/tests/turn-fixture.mjs'
import { NetworkEventLog } from './fixtures/network-event-log'
import {
  createCapabilityRedactor,
  withCapabilityRedaction,
} from './fixtures/capability-redactor'

const SCENARIO_ID = 'chromium-turn-route'
const FILE_NAME = 'turn-route.bin'
const TRANSFER_BYTES = HOT_SWITCH_TRANSFER_BLOCKS * DIRECT_TEST_BLOCK_BYTES

test('discovers deployment TURN and renews native/browser paths after application relay loss', async ({ page }, testInfo) => {
  const stack = new DirectProductStack(SCENARIO_ID)
  const turn = new LocalTurnServer()
  const events = new NetworkEventLog()
  const browserDiagnostics: string[] = []
  page.on('pageerror', error => browserDiagnostics.push(error.message))
  page.on('requestfailed', request => browserDiagnostics.push(new URL(request.url()).pathname + ': ' + request.failure()?.errorText))
  page.on('websocket', socket => {
    socket.on('socketerror', error => browserDiagnostics.push('WebSocket: ' + error))
  })
  let managed: Awaited<ReturnType<typeof turnFixture>> | undefined
  let primaryFailure: unknown
  let readSenderTrace: (() => Promise<readonly unknown[]>) | undefined
  let redactor: ReturnType<typeof createCapabilityRedactor> | undefined
  try {
    const starts = await Promise.allSettled([stack.start(), turn.start()])
    const startFailures = starts.flatMap((result) => result.status === 'rejected' ? [result.reason] : [])
    if (startFailures.length === 1) throw startFailures[0]
    if (startFailures.length > 1) throw new AggregateError(startFailures, 'TURN route startup failed')

    const payload = deterministicBytes(TRANSFER_BYTES)
    const expectedHash = createHash('sha256').update(payload).digest('hex')
    managed = await turnFixture({ lifetimeMs: 60_000, provider: async (request: Request) =>
      request.url.endsWith('/revoke') ? new Response(null, { status: 204 }) :
        Response.json({ iceServers: [turn.issueCredentials()] }, { status: 201 }) })
    expect((await managed.provision()).status).toBe('ready')
    const credential = await managed.admin('credentials', { name: 'browser sender' })
    // Begin with no cached capability: discovery must add it without blocking the share.
    const configuration = credential.configuration
    const proxy = await stack.createRelayCutProxy(managed.origin.replace(/^http:/u, 'ws:'))
    const path = await stack.createFile(FILE_NAME, payload)
    const share = await stack.share(path, { relayUrl: managed.origin, configuration, senderTrace: true })
    readSenderTrace = () => stack.senderTraceRecords(share)
    const navigationUrl = relayReceiverUrl(share, proxy.url)
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
      // The direct path has no usable candidates in this fixture. Delegated
      // paths must obtain their own credentials through the production session.
      rtcConfiguration: { iceServers: [], iceTransportPolicy: 'relay' },
      transferBytes: TRANSFER_BYTES,
    }), {
      completeUrl: navigationUrl,
      fragment: new URL(navigationUrl).hash,
      separateKey: share.key,
    })

    await events.waitFor(
      'dispatch',
      (event) => event.observation.route === 'application-relay',
      'first relay dispatch',
    )
    const admitted = await events.waitFor(
      'attempt',
      (event) => event.evidence.stage === 'admitted',
      'authenticated TURN peer admission',
    )
    const lane = await events.waitFor(
      'lane-admitted',
      (event) => event.observation.route === 'turn',
      'TURN peer lane admission',
    )
    await events.waitFor('attempt', (event) => event.evidence.stage === 'provider-fact' &&
      event.evidence.fact.kind === 'selected-pair' && event.evidence.fact.route === 'turn' &&
      event.evidence.fact.clientToTurnProtocol === 'tcp', 'TCP-first TURN selection')

    await proxy.cut()
    await sealPageRelayCut(page)
    const cut = await events.waitFor('relay-ineligible', () => true, 'relay ineligibility')
    await expect.poll(() => events.snapshot().some((event) => event.kind === 'lane-admitted' &&
      event.observation.route === 'turn' && !sameLaneIdentity(event.observation, lane.observation)),
    { timeout: 45_000, message: 'fresh TURN credentials must be admitted through the surviving authenticated lane' }).toBe(true)
    const replacement = events.snapshot().find((event) => event.kind === 'lane-admitted' &&
      event.observation.route === 'turn' && !sameLaneIdentity(event.observation, lane.observation))
    if (replacement?.kind !== 'lane-admitted') throw new Error('TURN replacement was not observed')
    const retirement = await events.waitFor(
      'lane-detached',
      (event) => sameLaneIdentity(event.observation, lane.observation),
      'old TURN allocation retirement',
    )
    await releasePageOutput(page)

    const peerDispatch = await events.waitForLaneDispatch(
      replacement.observation,
      cut.dispatchSequenceBoundary,
    )
    const delivery = await events.waitFor('delivery', () => true, 'TURN delivery terminal')
    const runtime = await events.waitFor('runtime-settled', () => true, 'TURN runtime settlement')
    expect(runtime.error).toBeUndefined()
    const observed = events.snapshot()
    const retirementIndex = observed.indexOf(retirement)
    expect(observed.indexOf(replacement)).toBeLessThan(retirementIndex)
    expect(observed.slice(retirementIndex + 1).filter((event) =>
      event.kind === 'dispatch' && sameLaneIdentity(event.observation, lane.observation),
    )).toEqual([])
    expect(observed.filter((event) =>
      event.kind === 'dispatch' &&
      event.observation.route === 'application-relay' &&
      event.observation.dispatchSequence > cut.dispatchSequenceBoundary,
    )).toEqual([])

    if (admitted.evidence.stage !== 'admitted') {
      throw new Error('TURN admission wait returned a non-admitted diagnostic')
    }
    const offerSent = await events.waitFor(
      'attempt',
      (event) => event.evidence.stage === 'offer-sent' &&
        sameIdentityBytes(event.evidence.attemptIdBytes, admitted.evidence.attemptIdBytes),
      'TURN offer operation',
    )
    const admittedLane = requireEvidenceLane(admitted.evidence)
    expect(admitted.evidence).toMatchObject({
      eventName: 'peer_attempt',
      protocolSessionIdBytes: expect.any(Array),
      peerPathIdBytes: expect.any(Array),
      attemptIdBytes: expect.any(Array),
      operationIdBytes: expect.any(Array),
      stage: 'admitted',
      phase: 'admission',
      lane: {
        laneId: expect.any(Number),
        laneEpoch: expect.any(Number),
      },
    })
    expect(requireOperationIdentity(admitted.evidence)).not.toEqual(
      requireOperationIdentity(offerSent.evidence),
    )
    expect(lane.observation).toMatchObject({
      laneId: admittedLane.laneId,
      laneEpoch: admittedLane.laneEpoch,
      route: 'turn',
    })
    expect(peerDispatch.observation).toMatchObject({
      laneId: replacement.observation.laneId,
      laneEpoch: replacement.observation.laneEpoch,
      route: 'turn',
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
  } catch (error) {
    primaryFailure = error
    const diagnostic = {
      component: 'browser-network-route',
      scenarioId: SCENARIO_ID,
      operationId: 'chromium-turn-route-test',
      milestone: 'failed',
      events: events.snapshot(),
      browserDiagnostics,
      senderTrace: await readSenderTrace?.(),
      turn: turn.diagnostic(),
      processes: stack.diagnostic(),
    } as const
    const safeLog = (value: unknown): string => redactor?.text(value) ?? JSON.stringify(value)
    console.info(safeLog({
      component: diagnostic.component,
      scenarioId: diagnostic.scenarioId,
      operationId: diagnostic.operationId,
      milestone: diagnostic.milestone,
      eventCount: diagnostic.events.length,
    }))
    diagnostic.events.forEach((event, eventIndex) => console.info(safeLog({
      component: diagnostic.component,
      scenarioId: diagnostic.scenarioId,
      operationId: diagnostic.operationId,
      milestone: 'event-observed',
      eventIndex,
      event,
    })))
    console.info(safeLog({
      component: diagnostic.component,
      scenarioId: diagnostic.scenarioId,
      operationId: diagnostic.operationId,
      milestone: 'process-snapshot',
      turn: diagnostic.turn,
      processes: diagnostic.processes,
      browserDiagnostics: diagnostic.browserDiagnostics,
    }))
    await testInfo.attach('turn-route-diagnostic', {
      body: redactor?.text(diagnostic) ?? JSON.stringify(diagnostic),
      contentType: 'application/json',
    }).catch(() => undefined)
  }
  await releasePageOutput(page).catch(() => undefined)
  const cleanup = await Promise.allSettled([turn.dispose(), stack.dispose()])
  if (managed) cleanup.push(...await Promise.allSettled([managed.dispose()]))
  const failures = cleanup.flatMap((result) => result.status === 'rejected' ? [result.reason] : [])
  const allFailures = [...(primaryFailure === undefined ? [] : [primaryFailure]), ...failures]
  try {
    if (allFailures.length === 1) {
      const failure = allFailures[0]
      const message = failure instanceof Error ? failure.message : String(failure)
      throw new Error(redactor?.redactText(message) ?? message, {
        cause: redactor?.value(failure),
      })
    }
    if (allFailures.length > 1) {
      const aggregate = new AggregateError(allFailures, 'TURN route failed')
      throw new Error(redactor?.redactText(aggregate.message) ?? aggregate.message, {
        cause: redactor?.value(aggregate),
      })
    }
  } finally {
    // A failed test throws before the success tail; clear the actual capability
    // values on both terminal paths rather than retaining them in the fixture.
    redactor?.clear()
  }
})

function requireEvidenceLane(evidence: { readonly lane?: {
  readonly laneId: number
  readonly laneEpoch: number
} }): { readonly laneId: number; readonly laneEpoch: number } {
  if (evidence.lane === undefined) throw new Error('Peer evidence did not retain its lane')
  return evidence.lane
}

function requireOperationIdentity(
  evidence: { readonly operationIdBytes?: readonly number[] },
): readonly number[] {
  if (evidence.operationIdBytes === undefined) {
    throw new Error('Peer evidence did not retain its protocol operation identity')
  }
  return evidence.operationIdBytes
}

function sameIdentityBytes(left: readonly number[], right: readonly number[]): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index])
}

function deterministicBytes(length: number): Uint8Array {
  return Uint8Array.from({ length }, (_value, index) => (index * 43 + 29) & 0xff)
}
