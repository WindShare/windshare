import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { CandidateBudget } from '../src/connectivity/ice-policy/candidates'
import { ICEEndpointPool, selectAttemptProfile, type ICEEndpoint, type SelectionRequest } from '../src/connectivity/ice-policy/endpoints'
import { ICEFactStore } from '../src/connectivity/ice-policy/facts'
import {
 CLOUDFLARE_STUN_SERVER,
 DIRECTORY_CANARY_STUN_PRIORITY,
 DIRECTORY_DRAINING_STUN_PRIORITY,
 DIRECTORY_NODE_STUN_PRIORITY,
 DIRECTORY_PUBLIC_STUN_PRIORITY,
 EXISTING_DEFAULT_STUN_SERVER,
 FALLBACK_STUN_PRIORITY,
 defaultICEEndpointPool,
 directoryICEEndpointPool,
} from '../src/connectivity/ice-policy/defaults'
import type { NetworkSnapshot } from '../src/network/node-directory'
import { canonicalSTUN } from '../src/network/stun-endpoint'

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
  const defaults = selectAttemptProfile(defaultICEEndpointPool(), request)
  expect(defaults.urls).toHaveLength(2)
  expect(new Set(defaults.failureDomains).size).toBe(2)
 })
 it('attributes only known endpoint failures and clears network facts', () => {
  const directory = pool()
  const first = selectAttemptProfile(directory, request)
  const facts = new ICEFactStore('network')
  expect(facts.recordEndpointFailure(first, 'unknown', request.nowMs)).toBe(false)
  expect(facts.recordEndpointFailure(first, '0', request.nowMs)).toBe(true)
  facts.recordProfile(first, true, request.nowMs)
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
 it('keeps the public STUN candidate catalog canonical, unreviewed, and inactive', () => {
  const entries = JSON.parse(readFileSync(new URL('../../deploy/network/public-stun-candidates.json', import.meta.url), 'utf8')) as ICEEndpoint[]
  expect(entries.length).toBeGreaterThan(0)
  const candidatePool = new ICEEndpointPool(entries)
  const urls = new Set<string>()
  for (const entry of entries) {
   expect(canonicalSTUN(entry.url)).toBe(entry.url)
   expect(urls.has(entry.url)).toBe(false)
   urls.add(entry.url)
   expect(entry.trust).toBe('unreviewed')
   expect(entry.enabled).toBe(false)
  }
  expect(selectAttemptProfile(candidatePool, request).urls).toEqual([])
 })
 it('prioritizes signed directory STUN endpoints ahead of embedded fallbacks', () => {
  const snapshot: NetworkSnapshot = {
   cursor: { generation: 1, serial: 3, digest: 'digest' },
   nodes: [
    { alias: 'cn1', nodeKey: 'key-cn1', relay: 'https://cn1.example', identity: 'https://cn1.example', stun: ['stun:cn1.example:3478'], roles: ['relay', 'stun'], zone: 'cn', region: 'cn-wuhan', provider: 'aliyun', operator: 'official', capacityMbps: 200, status: 'active' },
    { alias: 'us1', nodeKey: 'key-us1', relay: 'https://us1.example', identity: 'https://us1.example', stun: ['stun:us1.example:3478'], roles: ['relay', 'stun'], zone: 'global', region: 'us-los-angeles', provider: 'quickpacket', operator: 'official', capacityMbps: 100, status: 'draining' },
    { alias: 'relay-only', nodeKey: 'key-ro', relay: 'https://ro.example', identity: 'https://ro.example', stun: ['stun:ignored.example:3478'], roles: ['relay'], zone: 'global', region: 'de-frankfurt', provider: 'other', operator: 'official', capacityMbps: 50, status: 'active' },
   ],
   publicSTUN: [
    { url: CLOUDFLARE_STUN_SERVER, region: 'global', provider: 'cloudflare', failureDomain: 'directory-cloudflare' },
    { url: 'stun:cn-public.example:3478', region: 'cn', provider: 'tencent', failureDomain: 'tencent-stun' },
   ],
   notBefore: 0,
   trustedUntil: 1_000_000,
   envelope: '{}',
  }
  const directoryPool = directoryICEEndpointPool(snapshot)
  const byURL = new Map(directoryPool.endpoints.map(endpoint => [endpoint.url, endpoint]))
  expect(byURL.has('stun:us1.example:3478')).toBe(true)
  expect(byURL.has('stun:ignored.example:3478')).toBe(false)
  expect(byURL.get('stun:cn1.example:3478')?.priority).toBe(DIRECTORY_NODE_STUN_PRIORITY)
  expect(byURL.get('stun:us1.example:3478')?.priority).toBe(DIRECTORY_DRAINING_STUN_PRIORITY)
  expect(byURL.get(CLOUDFLARE_STUN_SERVER)).toMatchObject({
   priority: DIRECTORY_PUBLIC_STUN_PRIORITY,
   failureDomain: 'directory-cloudflare',
   region: 'global',
  })
  expect(byURL.get(EXISTING_DEFAULT_STUN_SERVER)?.priority).toBe(FALLBACK_STUN_PRIORITY)
  for (let i = 0; i < 16; i++) {
   const waveRequest: SelectionRequest = { ...request, waveID: `wave-${i}` }
   const primary = selectAttemptProfile(directoryPool, waveRequest)
   expect(new Set(primary.urls)).toEqual(new Set(['stun:cn1.example:3478', 'stun:us1.example:3478']))
   const backup = selectAttemptProfile(directoryPool, {
    ...waveRequest, sequence: waveRequest.sequence + 1,
    usedEndpointIDs: primary.endpointIDs, usedFailureDomains: primary.failureDomains,
   })
   expect(new Set(backup.urls)).toEqual(new Set(['stun:cn-public.example:3478', CLOUDFLARE_STUN_SERVER]))
  }
  const facts = new ICEFactStore(request.networkGenerationID)
  const primary = selectAttemptProfile(directoryPool, request)
  const backup = selectAttemptProfile(directoryPool, {
   ...request, sequence: request.sequence + 1,
   usedEndpointIDs: primary.endpointIDs, usedFailureDomains: primary.failureDomains,
  })
  facts.recordProfile(backup, true, request.nowMs)
  expect(selectAttemptProfile(directoryPool, request, facts.snapshot()).urls).toEqual(backup.urls)
 })
 it('ranks active directory STUN ahead of canary, draining, and public tiers', () => {
  const threeNode = directoryICEEndpointPool({
   cursor: { generation: 1, serial: 4, digest: 'digest' },
   nodes: [
    { alias: 'cn1', nodeKey: 'key-cn1', relay: 'https://cn1.example', identity: 'https://cn1.example', stun: ['stun:cn1.example:3478'], roles: ['relay', 'stun'], zone: 'cn', region: 'cn-wuhan', provider: 'aliyun', operator: 'official', capacityMbps: 200, status: 'active' },
    { alias: 'us1', nodeKey: 'key-us1', relay: 'https://us1.example', identity: 'https://us1.example', stun: ['stun:us1.example:3478'], roles: ['relay', 'stun'], zone: 'global', region: 'us-los-angeles', provider: 'quickpacket', operator: 'official', capacityMbps: 100, status: 'draining' },
    { alias: 'eu1', nodeKey: 'key-eu1', relay: 'https://eu1.example', identity: 'https://eu1.example', stun: ['stun:eu1.example:3478'], roles: ['relay', 'stun'], zone: 'global', region: 'eu-frankfurt', provider: 'gtt', operator: 'official', capacityMbps: 300, status: 'active' },
   ],
   publicSTUN: [],
   notBefore: 0,
   trustedUntil: 1_000_000,
   envelope: '{}',
  })
  for (let i = 0; i < 16; i++) {
   const waveRequest: SelectionRequest = { ...request, waveID: `three-${i}` }
   const primary = selectAttemptProfile(threeNode, waveRequest)
   expect(new Set(primary.urls)).toEqual(new Set(['stun:cn1.example:3478', 'stun:eu1.example:3478']))
   const backup = selectAttemptProfile(threeNode, {
    ...waveRequest, sequence: waveRequest.sequence + 1,
    usedEndpointIDs: primary.endpointIDs, usedFailureDomains: primary.failureDomains,
   })
   expect(backup.urls).toHaveLength(2)
   expect(backup.urls[0]).toBe('stun:us1.example:3478')
  }

  const mixed = directoryICEEndpointPool({
   cursor: { generation: 1, serial: 5, digest: 'digest' },
   nodes: [
    { alias: 'cn1', nodeKey: 'key-cn1', relay: 'https://cn1.example', identity: 'https://cn1.example', stun: ['stun:cn1.example:3478'], roles: ['relay', 'stun'], zone: 'cn', region: 'cn-wuhan', provider: 'aliyun', operator: 'official', capacityMbps: 200, status: 'active' },
    { alias: 'jp1', nodeKey: 'key-jp1', relay: 'https://jp1.example', identity: 'https://jp1.example', stun: ['stun:jp1.example:3478'], roles: ['relay', 'stun'], zone: 'global', region: 'jp-tokyo', provider: 'vultr', operator: 'official', capacityMbps: 150, status: 'canary' },
    { alias: 'us1', nodeKey: 'key-us1', relay: 'https://us1.example', identity: 'https://us1.example', stun: ['stun:us1.example:3478'], roles: ['relay', 'stun'], zone: 'global', region: 'us-los-angeles', provider: 'quickpacket', operator: 'official', capacityMbps: 100, status: 'draining' },
   ],
   publicSTUN: [
    { url: 'stun:cn-public.example:3478', region: 'cn', provider: 'tencent', failureDomain: 'tencent-stun' },
   ],
   notBefore: 0,
   trustedUntil: 1_000_000,
   envelope: '{}',
  })
  const byURL = new Map(mixed.endpoints.map(endpoint => [endpoint.url, endpoint]))
  expect(byURL.get('stun:cn1.example:3478')?.priority).toBe(DIRECTORY_NODE_STUN_PRIORITY)
  expect(byURL.get('stun:jp1.example:3478')?.priority).toBe(DIRECTORY_CANARY_STUN_PRIORITY)
  expect(byURL.get('stun:us1.example:3478')?.priority).toBe(DIRECTORY_DRAINING_STUN_PRIORITY)
  expect(byURL.get('stun:cn-public.example:3478')?.priority).toBe(DIRECTORY_PUBLIC_STUN_PRIORITY)
  for (let i = 0; i < 16; i++) {
   const waveRequest: SelectionRequest = { ...request, waveID: `mixed-${i}` }
   const primary = selectAttemptProfile(mixed, waveRequest)
   expect(primary.urls).toEqual(['stun:cn1.example:3478', 'stun:jp1.example:3478'])
   const backup = selectAttemptProfile(mixed, {
    ...waveRequest, sequence: waveRequest.sequence + 1,
    usedEndpointIDs: primary.endpointIDs, usedFailureDomains: primary.failureDomains,
   })
   expect(backup.urls).toEqual(['stun:us1.example:3478', 'stun:cn-public.example:3478'])
  }
 })
})
