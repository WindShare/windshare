import { afterEach, describe, expect, it, vi } from 'vitest'
import directoryVectors from '../../../testdata/network/directory.json'
import { OfficialShareReporter } from '../../src/application/network/report'
import { BuiltinNetwork } from '../../src/application/network/builtin'
import { encodeSuite02CapabilityKey, type Suite02CapabilityLink } from '../../src/crypto/suite02-link'
import { MemoryDirectoryCache } from '../../src/network/directory-cache'
import type { NetworkSnapshot } from '../../src/network/node-directory'
import { RelayCandidates } from '../../src/receiver/relay-selection/candidates'

const api = 'https://authority.example'
const officialRelay = 'https://official.example'
const independentRelay = 'https://self-hosted.example'
const share = 'AQEBAQEBAQEBAQEB'
const snapshot: NetworkSnapshot = {
  cursor: { generation: 1, serial: 1, digest: '' },
  nodes: [{
    alias: 'official', nodeKey: '', relay: officialRelay, identity: 'wss://official.example/v2/ws',
    stun: [], roles: ['relay'], zone: 'global', region: 'test', provider: 'test', operator: 'official',
    capacityMbps: 1, status: 'active',
  }],
  publicSTUN: [], notBefore: 0, trustedUntil: Infinity, envelope: '',
}

function directory(initial?: NetworkSnapshot) {
  let current = initial
  const listeners = new Set<() => void>()
  return {
    get snapshot() { return current },
    observeSnapshot(listener: () => void) {
      listeners.add(listener)
      return () => { listeners.delete(listener) }
    },
    publish(next: NetworkSnapshot) {
      current = next
      for (const listener of listeners) listener()
    },
  }
}

async function capabilityLink(
  relays: readonly string[],
  nodeAliases: readonly string[],
  seed = 1,
): Promise<Suite02CapabilityLink> {
  const key = await encodeSuite02CapabilityKey(new Uint8Array(16).fill(seed), new Uint8Array(16).fill(seed))
  return Object.freeze({ ...key, relays, nodeAliases })
}

describe('official share report scope', () => {
  const validEnvelope = directoryVectors.entries.find(entry => entry.name === 'valid')!.envelope
  const vectorNow = Date.parse(directoryVectors.now)
  const testRelease = {
    network: directoryVectors.network,
    roots: directoryVectors.roots,
    mirrors: [],
    snapshot: validEnvelope,
    authority: { url: api },
  }
  const networks: BuiltinNetwork[] = []

  afterEach(() => {
    for (const network of networks.splice(0)) network.close()
    vi.unstubAllGlobals()
  })

  it.each(['http://api.example', 'https://user:pass@api.example', 'https://api.example/?x', 'https://api.example/#share',
    'https://preview.workers.dev', 'https://preview.workers.dev.'])('rejects an invalid report endpoint %s', endpoint => {
    expect(() => new OfficialShareReporter(endpoint)).toThrow(TypeError)
  })

  it('omits report URLs for self-hosted relays without fetching', () => {
    const request = vi.fn(async () => new Response(null, { status: 503 }))
    vi.stubGlobal('fetch', request)
    const authority = new OfficialShareReporter(api)
    const source = directory(snapshot)
    const onAvailable = vi.fn()
    const stop = authority.watchReport(share, new RelayCandidates([independentRelay]), source, onAvailable)
    source.publish(snapshot)
    expect(onAvailable).not.toHaveBeenCalled()
    expect(request).not.toHaveBeenCalled()
    stop()
  })

  it('publishes report URLs once official membership is confirmed across canonical, mixed, and late discovery', async () => {
    const authority = new OfficialShareReporter(api)
    const expected = `${api}/report#share=${share}`
    const warm = directory(snapshot)
    const immediate = vi.fn()
    authority.watchReport(share, new RelayCandidates([independentRelay, 'wss://OFFICIAL.example:443/']), warm, immediate)
    expect(immediate).toHaveBeenCalledExactlyOnceWith(expected)

    const lateDirectory = directory()
    const fromDirectory = vi.fn()
    authority.watchReport(share, new RelayCandidates([officialRelay]), lateDirectory, fromDirectory)
    expect(fromDirectory).not.toHaveBeenCalled()
    lateDirectory.publish(snapshot)
    expect(fromDirectory).toHaveBeenCalledExactlyOnceWith(expected)

    let expand!: (bases: readonly string[]) => void
    const expanding = new RelayCandidates([], new Promise(resolve => { expand = resolve }))
    const fromExpansion = vi.fn()
    authority.watchReport(share, expanding, warm, fromExpansion)
    expect(fromExpansion).not.toHaveBeenCalled()
    expand([officialRelay])
    await expanding.settled
    expect(fromExpansion).toHaveBeenCalledExactlyOnceWith(expected)

    let expandCancelled!: (bases: readonly string[]) => void
    const cancelledCandidates = new RelayCandidates([], new Promise(resolve => { expandCancelled = resolve }))
    const cancelled = vi.fn()
    const stopCancelled = authority.watchReport(share, cancelledCandidates, warm, cancelled)
    stopCancelled()
    expandCancelled([officialRelay])
    await cancelledCandidates.settled
    expect(cancelled).not.toHaveBeenCalled()
  })

  it('filters BuiltinNetwork report URLs by official directory membership and cleans up across share transitions', async () => {
    const request = vi.fn<typeof fetch>()
    vi.stubGlobal('fetch', request)
    const openDatabase = vi.fn()
    vi.stubGlobal('indexedDB', { open: openDatabase })
    const cache = new MemoryDirectoryCache()
    let finishLoad!: () => void
    const loadGate = new Promise<undefined>(resolve => { finishLoad = () => resolve(undefined) })
    const network = new BuiltinNetwork(undefined, {
      release: testRelease,
      directory: {
        cache: { load: () => loadGate, commit: value => cache.commit(value) },
        now: () => vectorNow,
      },
    })
    networks.push(network)
    const observed: Array<string | undefined> = []
    network.observeReport(() => observed.push(network.getReportURL()))

    const cancelledOfficial = await capabilityLink([], ['jp1'], 1)
    const cancelledCandidates = network.resolve(cancelledOfficial)
    expect(network.getReportURL()).toBeUndefined()
    network.clearReport()

    const selfHosted = await capabilityLink([independentRelay], [], 2)
    network.resolve(selfHosted)
    finishLoad()
    await cancelledCandidates.all()
    expect(network.getReportURL()).toBeUndefined()
    expect(observed).toEqual([])

    const officialAlias = await capabilityLink([], ['jp1'], 3)
    const aliasCandidates = network.resolve(officialAlias)
    await aliasCandidates.all()
    expect(network.getReportURL()).toBe(`${api}/report#share=${officialAlias.shareId}`)

    const officialExplicit = await capabilityLink(['wss://JP1.example:443/'], [], 4)
    network.resolve(officialExplicit)
    expect(network.getReportURL()).toBe(`${api}/report#share=${officialExplicit.shareId}`)

    const hybrid = await capabilityLink([independentRelay], ['jp1'], 5)
    const hybridCandidates = network.resolve(hybrid)
    await hybridCandidates.all()
    expect(network.getReportURL()).toBe(`${api}/report#share=${hybrid.shareId}`)

    network.resolve(selfHosted)
    expect(network.getReportURL()).toBeUndefined()
    expect(request).not.toHaveBeenCalled()
    expect(openDatabase).not.toHaveBeenCalled()
    expect(observed).toEqual([
      `${api}/report#share=${officialAlias.shareId}`,
      `${api}/report#share=${officialExplicit.shareId}`,
      `${api}/report#share=${hybrid.shareId}`,
      undefined,
    ])
  })

  it('leaves report URLs absent when release configuration has no authority', async () => {
    const network = new BuiltinNetwork(undefined, {
      release: { ...testRelease, authority: null },
      directory: { cache: new MemoryDirectoryCache(), now: () => vectorNow },
    })
    networks.push(network)
    const officialAlias = await capabilityLink([], ['jp1'], 6)
    await network.resolve(officialAlias).all()
    expect(network.getReportURL()).toBeUndefined()
  })
})
