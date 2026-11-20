import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import fixture from '../../../testdata/network/authority.json'
import directoryVectors from '../../../testdata/network/directory.json'
import { OfficialRelayAuthority } from '../../src/application/network/authority'
import { BuiltinNetwork } from '../../src/application/network/builtin'
import { encodeSuite02CapabilityKey, type Suite02CapabilityLink } from '../../src/crypto/suite02-link'
import { NetworkDirectory } from '../../src/network/directory'
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

describe('official relay policy demand', () => {
  const request = vi.fn(async () => new Response(null, { status: 503 }))
  let authority: OfficialRelayAuthority

  beforeEach(() => {
    vi.useFakeTimers()
    request.mockClear()
    vi.stubGlobal('fetch', request)
    authority = new OfficialRelayAuthority({ url: api, root: fixture.root })
  })
  afterEach(() => {
    authority.close()
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })

  it('leaves the feed idle without an official dial, including report links and self-hosted dials', async () => {
    authority.reportURL(share)
    const independent = authority.admission(share, directory(snapshot)).watch(independentRelay, vi.fn())
    expect(independent.beforeDial).toBeUndefined()
    await vi.advanceTimersByTimeAsync(120_000)
    expect(request).not.toHaveBeenCalled()
    independent.close()
  })

  it('shares one refresh cadence across official dials and pauses while only self-hosted traffic remains', async () => {
    const admission = authority.admission(share, directory(snapshot))
    let respond!: (response: Response) => void
    request.mockImplementationOnce(() => new Promise(resolve => { respond = resolve }))
    const independent = admission.watch(independentRelay, vi.fn())
    const official = admission.watch('wss://OFFICIAL.example:443/', vi.fn())
    const parallel = admission.watch(officialRelay, vi.fn())
    expect(official.beforeDial).toBeInstanceOf(Promise)
    expect(parallel.beforeDial).toBe(official.beforeDial)
    expect(independent.beforeDial).toBeUndefined()
    await official.beforeDial
    expect(request).toHaveBeenCalledExactlyOnceWith(`${api}/v1/blocks`, expect.anything())
    respond(new Response(null, { status: 503 }))
    await vi.advanceTimersByTimeAsync(0)

    official.close()
    await vi.advanceTimersByTimeAsync(60_000)
    expect(request).toHaveBeenCalledTimes(2)
    parallel.close()

    const retry = admission.watch(officialRelay, vi.fn())
    await retry.beforeDial
    retry.close()
    await vi.advanceTimersByTimeAsync(120_000)
    expect(request).toHaveBeenCalledTimes(2)

    const resumed = admission.watch(officialRelay, vi.fn())
    await resumed.beforeDial
    await vi.advanceTimersByTimeAsync(0)
    expect(request).toHaveBeenCalledTimes(3)
    resumed.close()
    independent.close()
  })

  it('uses the initialized directory to classify an endpoint without holding its dial', async () => {
    const cache = new MemoryDirectoryCache()
    let finishLoad!: () => void
    const load = new Promise<undefined>(resolve => { finishLoad = () => resolve(undefined) })
    const source = new NetworkDirectory({
      network: directoryVectors.network,
      roots: directoryVectors.roots,
      embedded: JSON.stringify(directoryVectors.entries.find(entry => entry.name === 'valid')!.envelope),
      cache: { load: () => load, commit: value => cache.commit(value) },
      mirrors: [],
      now: () => Date.parse(directoryVectors.now),
    })
    const admission = authority.admission(share, source)
    const independent = admission.watch(independentRelay, vi.fn())
    const official = admission.watch('https://jp1.example', vi.fn())
    try {
      expect(independent.beforeDial).toBeUndefined()
      expect(official.beforeDial).toBeUndefined()
      expect(request).not.toHaveBeenCalled()

      finishLoad()
      await source.ready
      await vi.advanceTimersByTimeAsync(0)
      expect(request).toHaveBeenCalledExactlyOnceWith(`${api}/v1/blocks`, expect.anything())

      official.close()
      await vi.advanceTimersByTimeAsync(120_000)
      expect(request).toHaveBeenCalledTimes(1)
    } finally {
      official.close()
      independent.close()
      source.close()
    }
  })

  it('starts policy for an already connected endpoint discovered by a later directory refresh', async () => {
    const source = directory()
    const watch = authority.admission(share, source).watch(officialRelay, vi.fn())
    expect(watch.beforeDial).toBeUndefined()
    await vi.advanceTimersByTimeAsync(0)
    expect(request).not.toHaveBeenCalled()

    source.publish(snapshot)
    await vi.advanceTimersByTimeAsync(0)
    expect(request).toHaveBeenCalledTimes(1)
    watch.close()
  })
})

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
    authority: { url: api, root: fixture.root },
  }
  const networks: BuiltinNetwork[] = []

  afterEach(() => {
    for (const network of networks.splice(0)) network.close()
  })

  it('omits report URLs for self-hosted relays without waking the block feed', () => {
    const request = vi.fn(async () => new Response(null, { status: 503 }))
    vi.stubGlobal('fetch', request)
    const authority = new OfficialRelayAuthority({ url: api, root: fixture.root })
    try {
      const source = directory(snapshot)
      const onAvailable = vi.fn()
      const stop = authority.watchReport(share, new RelayCandidates([independentRelay]), source, onAvailable)
      source.publish(snapshot)
      expect(onAvailable).not.toHaveBeenCalled()
      expect(request).not.toHaveBeenCalled()
      stop()
    } finally {
      authority.close()
      vi.unstubAllGlobals()
    }
  })

  it('publishes report URLs once official membership is confirmed across canonical, mixed, and late discovery', async () => {
    const authority = new OfficialRelayAuthority({ url: api, root: fixture.root })
    try {
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
    } finally {
      authority.close()
    }
  })

  it('filters BuiltinNetwork report URLs by official directory membership and cleans up across share transitions', async () => {
    const cache = new MemoryDirectoryCache()
    let finishLoad!: () => void
    const loadGate = new Promise<undefined>(resolve => { finishLoad = () => resolve(undefined) })
    const network = new BuiltinNetwork(undefined, undefined, {
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
    expect(observed).toEqual([
      `${api}/report#share=${officialAlias.shareId}`,
      `${api}/report#share=${officialExplicit.shareId}`,
      `${api}/report#share=${hybrid.shareId}`,
      undefined,
    ])
  })

  it('leaves report URLs absent when release configuration has no authority', async () => {
    const network = new BuiltinNetwork(undefined, undefined, {
      release: { ...testRelease, authority: null },
      directory: { cache: new MemoryDirectoryCache(), now: () => vectorNow },
    })
    networks.push(network)
    const officialAlias = await capabilityLink([], ['jp1'], 6)
    await network.resolve(officialAlias).all()
    expect(network.getReportURL()).toBeUndefined()
  })
})
