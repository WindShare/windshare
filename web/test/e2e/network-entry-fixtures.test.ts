import { describe, expect, it } from 'vitest'

import { parseLocalTurnReadyRecord } from '../../e2e/fixtures/local-turn-server'
import { NetworkEventLog } from '../../e2e/fixtures/network-event-log'

describe('direct weekly network fixtures', () => {
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
      observation: { dispatchSequence: 7, laneId: 1, laneEpoch: 1, route: 'relay' },
    })
    await expect(log.waitFor(
      'dispatch',
      (event) => event.observation.dispatchSequence === 7,
      'existing relay dispatch',
    )).resolves.toMatchObject({ kind: 'dispatch' })

    const admitted = log.waitFor(
      'lane-admitted',
      (event) => event.observation.route === 'peer',
      'peer admission',
    )
    log.accept({
      kind: 'lane-admitted',
      observation: { laneId: 2, laneEpoch: 3, route: 'peer' },
    })
    await expect(admitted).resolves.toMatchObject({
      observation: { laneId: 2, laneEpoch: 3, route: 'peer' },
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
