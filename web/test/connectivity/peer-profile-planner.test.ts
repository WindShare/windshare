import { expect, it } from 'vitest'
import { PeerProfilePlanner } from '../../src/connectivity/peer-set/profile-planner'
import { ICEEndpointPool } from '../../src/connectivity/ice-policy/endpoints'
import { PeerNetworkGeneration } from '../../src/connectivity/peer-set/network-generation'

it('retains singleton primary STUN across every later fresh attempt', () => {
  const planner = new PeerProfilePlanner()
  const primary = planner.select(1, 1, 0)
  expect(primary.urls).toHaveLength(1)
  for (let attempt = 2; attempt <= 6; attempt++) {
    expect(planner.select(1, attempt, attempt * 1000).urls).toEqual(primary.urls)
  }
})

it('rotates once into unused domains and reuses backup across network notices without expanding the footprint', () => {
  const pool = new ICEEndpointPool(Array.from({ length: 8 }, (_, index) => ({
    id: String(index), url: `stun:stun${index}.example:3478`, failureDomain: String(index),
    region: '', provider: '', family: 'any', trust: 'local', enabled: true, priority: 1,
  })))
  const planner = new PeerProfilePlanner(pool, new PeerNetworkGeneration())
  const first = planner.select(1, 1, 0)
  const backup = planner.select(1, 2, 1000)
  expect(new Set([...first.endpointIDs, ...backup.endpointIDs]).size).toBe(4)
  planner.networkChanged(2000)
  const third = planner.select(1, 3, 2000)
  expect(new Set(third.endpointIDs)).toEqual(new Set(backup.endpointIDs))
  expect(third.networkGenerationID).not.toBe(backup.networkGenerationID)
  expect(new Set(planner.select(1, 4, 3000).endpointIDs)).toEqual(new Set(backup.endpointIDs))
})

it('cools attributable failures without converting profile exhaustion into permanent host-only recovery', () => {
  const planner = new PeerProfilePlanner()
  const first = planner.select(1, 1, 0)
  planner.observe(first, { kind: 'ice-error', endpoint: first.urls[0]!, code: 701 }, 0, 1)
  expect(planner.select(1, 2, 1000).urls).toEqual([])
  expect(planner.select(1, 3, 31_000).urls).toEqual(first.urls)
  expect(new PeerProfilePlanner(new ICEEndpointPool()).select(1, 1, 0).urls).toEqual([])
})
