import { createHash } from 'node:crypto'

import { expect, test } from '@playwright/test'

import { V2_BLOCK_BROKER_PARALLEL_READS } from '../src/content/v2-broker'
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
import { NetworkEventLog } from './fixtures/network-event-log'

const SCENARIO_ID = 'chromium-turn-route'
const FILE_NAME = 'turn-route.bin'
const TRANSFER_BYTES = (V2_BLOCK_BROKER_PARALLEL_READS + 1) * DIRECT_TEST_BLOCK_BYTES

test('continues over an authenticated TURN peer lane after relay loss', async ({ page }, testInfo) => {
  const stack = new DirectProductStack(SCENARIO_ID)
  const turn = new LocalTurnServer()
  const events = new NetworkEventLog()
  let primaryFailure: unknown
  try {
    const starts = await Promise.allSettled([stack.start(), turn.start()])
    const startFailures = starts.flatMap((result) => result.status === 'rejected' ? [result.reason] : [])
    if (startFailures.length === 1) throw startFailures[0]
    if (startFailures.length > 1) throw new AggregateError(startFailures, 'TURN route startup failed')

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
      rtcConfiguration: turn.rtcConfiguration,
      transferBytes: TRANSFER_BYTES,
    })

    await events.waitFor(
      'dispatch',
      (event) => event.observation.route === 'relay',
      'first relay dispatch',
    )
    const admitted = await events.waitFor(
      'attempt',
      (event) => event.evidence.stage === 'admitted',
      'authenticated TURN peer admission',
    )
    const lane = await events.waitFor(
      'lane-admitted',
      (event) => event.observation.route === 'peer',
      'TURN peer lane admission',
    )

    await proxy.cut()
    await sealPageRelayCut(page)
    await events.waitFor('relay-ineligible', () => true, 'relay ineligibility')
    const cutBoundary = events.latestDispatchSequence()
    await releasePageOutput(page)

    const peerDispatch = await events.waitFor(
      'dispatch',
      (event) => event.observation.route === 'peer' &&
        event.observation.dispatchSequence > cutBoundary,
      'post-cut TURN dispatch',
    )
    const delivery = await events.waitFor('delivery', () => true, 'TURN delivery terminal')

    if (admitted.evidence.stage !== 'admitted') {
      throw new Error('TURN admission wait returned a non-admitted diagnostic')
    }
    expect(admitted.evidence).toMatchObject({
      sessionId: expect.any(String),
      peerPathId: expect.any(String),
      attemptId: expect.any(String),
      selectedPair: {
        local: { candidateType: 'relay', protocol: 'udp' },
      },
    })
    expect(lane.observation).toMatchObject({
      laneId: admitted.evidence.lane.laneId,
      laneEpoch: admitted.evidence.lane.laneEpoch,
      route: 'peer',
    })
    expect(peerDispatch.observation).toMatchObject({
      laneId: lane.observation.laneId,
      laneEpoch: lane.observation.laneEpoch,
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
  } catch (error) {
    primaryFailure = error
    const diagnostic = {
      component: 'browser-network-route',
      scenarioId: SCENARIO_ID,
      operationId: 'chromium-turn-route-test',
      milestone: 'failed',
      events: events.snapshot(),
      turn: turn.diagnostic(),
      processes: stack.diagnostic(),
    } as const
    console.info(JSON.stringify({
      component: diagnostic.component,
      scenarioId: diagnostic.scenarioId,
      operationId: diagnostic.operationId,
      milestone: diagnostic.milestone,
      eventCount: diagnostic.events.length,
    }))
    diagnostic.events.forEach((event, eventIndex) => console.info(JSON.stringify({
      component: diagnostic.component,
      scenarioId: diagnostic.scenarioId,
      operationId: diagnostic.operationId,
      milestone: 'event-observed',
      eventIndex,
      event,
    })))
    console.info(JSON.stringify({
      component: diagnostic.component,
      scenarioId: diagnostic.scenarioId,
      operationId: diagnostic.operationId,
      milestone: 'process-snapshot',
      turn: diagnostic.turn,
      processes: diagnostic.processes,
    }))
    await testInfo.attach('turn-route-diagnostic', {
      body: JSON.stringify(diagnostic, null, 2),
      contentType: 'application/json',
    }).catch(() => undefined)
  }
  await releasePageOutput(page).catch(() => undefined)
  const cleanup = await Promise.allSettled([turn.dispose(), stack.dispose()])
  const failures = cleanup.flatMap((result) => result.status === 'rejected' ? [result.reason] : [])
  const allFailures = [...(primaryFailure === undefined ? [] : [primaryFailure]), ...failures]
  if (allFailures.length === 1) throw allFailures[0]
  if (allFailures.length > 1) throw new AggregateError(allFailures, 'TURN route failed')
})

function deterministicBytes(length: number): Uint8Array {
  return Uint8Array.from({ length }, (_value, index) => (index * 43 + 29) & 0xff)
}
