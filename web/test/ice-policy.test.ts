import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { CandidateBudget } from '../src/connectivity/ice-policy/candidates'
import { ICEEndpointPool, selectAttemptProfile, type ICEEndpoint, type SelectionRequest } from '../src/connectivity/ice-policy/endpoints'
import { ICEFactStore } from '../src/connectivity/ice-policy/facts'
import { defaultICEEndpointPool } from '../src/connectivity/ice-policy/defaults'

const request: SelectionRequest = {
 networkGenerationID: 'network', waveID: 'wave', sequence: 1, nowMs: 100_000,
 usedEndpointIDs: [], ipv4: true, ipv6: true,
}
function pool(): ICEEndpointPool {
 return new ICEEndpointPool(Array.from({ length: 6 }, (_, i): ICEEndpoint => ({
  id: String(i), url: 'stun:node' + i + '.test:3478', region: String(i % 2), failureDomain: String(i),
  provider: '', family: 'any', trust: 'reviewed', priority: 6 - i, enabled: true,
 })))
}
describe('trusted ICE policy', () => {
 it('uses distinct bounded profiles and immutable snapshots', () => {
  const directory = pool()
  const first = selectAttemptProfile(directory, request)
  const vector = JSON.parse(readFileSync(new URL('../../testdata/ice-policy/selection.json', import.meta.url), 'utf8')) as { endpointIDs: string[]; profileID: string }
  expect(first.endpointIDs).toEqual(vector.endpointIDs)
  expect(first.id).toBe(vector.profileID)
  expect(Object.isFrozen(first.urls)).toBe(true)
  const second = selectAttemptProfile(directory, { ...request, sequence: 2, usedEndpointIDs: first.endpointIDs, usedFailureDomains: first.failureDomains })
  expect(second.endpointIDs).toEqual(['2', '3'])
  expect(selectAttemptProfile(directory, { ...request, usedEndpointIDs: [...first.endpointIDs, ...second.endpointIDs] }).urls).toEqual([])
  expect(selectAttemptProfile(defaultICEEndpointPool(), request).urls).toEqual(['stun:stun.l.google.com:19302'])
 })
 it('attributes only known endpoint failures and clears network facts', () => {
  const directory = pool()
  const first = selectAttemptProfile(directory, request)
  const facts = new ICEFactStore('network')
  expect(facts.recordEndpointFailure(first, 'unknown', request.nowMs)).toBe(false)
  expect(facts.recordEndpointFailure(first, '0', request.nowMs)).toBe(true)
  facts.recordProfile(first, true, 12)
  expect(selectAttemptProfile(directory, request, facts.snapshot()).endpointIDs[0]).toBe('1')
  facts.setGeneration('next')
  expect(facts.snapshot().endpoints).toEqual([])
  expect(facts.snapshot().profiles).toEqual([])
  expect(facts.recordEndpointFailure(first, '0', request.nowMs)).toBe(false)
  expect(selectAttemptProfile(directory, request, facts.snapshot()).endpointIDs[0]).toBe('0')
 })
 it('rejects untrusted or malformed enabled endpoints without networking', () => {
  const base = pool().endpoints[0]!
  for (const url of ['turn:node:3478', 'stun:node', 'stun:user@node:3478', 'stun:node:0', 'stun:node:65536']) {
   expect(() => new ICEEndpointPool([{ ...base, url }])).toThrow()
  }
  expect(() => new ICEEndpointPool([{ ...base, trust: 'unreviewed' }])).toThrow()
  expect(() => new ICEEndpointPool([base, base])).toThrow()
  expect(selectAttemptProfile(new ICEEndpointPool([{ ...base, enabled: false }]), request).urls).toEqual([])
 })
 it('shares Go candidate vectors and preserves late paths under host flood', () => {
  const vectors = JSON.parse(readFileSync(new URL('../../testdata/ice-policy/candidates.json', import.meta.url), 'utf8')) as { candidate: string; class: string; reason: string }[]
  const budget = new CandidateBudget()
  for (const vector of vectors) expect(budget.accept(vector.candidate)).toEqual({
   accepted: vector.reason === 'accepted', reason: vector.reason, candidateClass: vector.class,
  })
  const flood = new CandidateBudget(12)
  for (let i = 0; i < 4; i++) expect(flood.accept('candidate:1 1 udp 100 192.168.1.2 ' + (5000 + i) + ' typ host').accepted).toBe(true)
  expect(flood.accept('candidate:1 1 udp 100 192.168.1.2 5010 typ host').reason).toBe('reserved')
  expect(flood.accept('candidate:2 1 udp 100 2001:db8::1 5000 typ host').accepted).toBe(true)
  expect(flood.accept('candidate:3 1 udp 100 192.0.2.1 5000 typ srflx').accepted).toBe(true)
  expect(flood.accept('candidate:4 1 tcp 100 192.0.2.1 5000 typ host tcptype passive').accepted).toBe(true)
  expect(flood.accept('candidate:5 1 udp 100 fd00::1 5000 typ host').accepted).toBe(true)
  expect(flood.acceptMapped('candidate:6 1 udp 100 192.0.2.55 6000 typ srflx')).toEqual({
   accepted: true, reason: 'accepted', candidateClass: 'mapped',
  })
 })
})
