import { afterEach, describe, expect, it, vi } from 'vitest'
import directoryVectors from '../../../testdata/network/directory.json'
import { OfficialShareReporter, type ShareReport } from '../../src/application/network/report'
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
const emptySnapshot: NetworkSnapshot = { ...snapshot, nodes: [] }

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
  const reports: ShareReport[] = []

  afterEach(() => {
    for (const report of reports.splice(0)) report.close()
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
    const report = authority.track(share, new RelayCandidates([independentRelay]), source)
    reports.push(report)
    const onChange = vi.fn()
    report.subscribe(onChange)
    source.publish(snapshot)
    expect(report.url).toBeUndefined()
    expect(onChange).not.toHaveBeenCalled()
    expect(request).not.toHaveBeenCalled()
  })

  it('publishes report URLs once official membership is confirmed across canonical, mixed, and late discovery', async () => {
    const authority = new OfficialShareReporter(api)
    const expected = `${api}/report#share=${share}`
    const warm = directory(snapshot)

    const immediate = authority.track(share, new RelayCandidates([independentRelay, 'wss://OFFICIAL.example:443/']), warm)
    reports.push(immediate)
    expect(immediate.url).toBe(expected)
    // Later directory updates must not revoke reporting for an already identified share.
    warm.publish(emptySnapshot)
    expect(immediate.url).toBe(expected)
    warm.publish(snapshot)

    const lateDirectory = directory()
    const fromDirectory = authority.track(share, new RelayCandidates([officialRelay]), lateDirectory)
    reports.push(fromDirectory)
    const dirObserved: Array<string | undefined> = []
    fromDirectory.subscribe(() => dirObserved.push(fromDirectory.url))
    expect(fromDirectory.url).toBeUndefined()
    lateDirectory.publish(snapshot)
    expect(fromDirectory.url).toBe(expected)
    expect(dirObserved).toEqual([expected])

    let expand!: (bases: readonly string[]) => void
    const expanding = new RelayCandidates([], new Promise(resolve => { expand = resolve }))
    const fromExpansion = authority.track(share, expanding, warm)
    reports.push(fromExpansion)
    const expandObserved: Array<string | undefined> = []
    fromExpansion.subscribe(() => expandObserved.push(fromExpansion.url))
    expect(fromExpansion.url).toBeUndefined()
    expand([officialRelay])
    await expanding.settled
    expect(fromExpansion.url).toBe(expected)
    expect(expandObserved).toEqual([expected])

    let expandCancelled!: (bases: readonly string[]) => void
    const cancelledCandidates = new RelayCandidates([], new Promise(resolve => { expandCancelled = resolve }))
    const cancelled = authority.track(share, cancelledCandidates, warm)
    const cancelledObserved = vi.fn()
    cancelled.subscribe(cancelledObserved)
    cancelled.close()
    expandCancelled([officialRelay])
    await cancelledCandidates.settled
    expect(cancelled.url).toBeUndefined()
    expect(cancelledObserved).not.toHaveBeenCalled()
  })

  it('scopes BuiltinNetwork report handles per resolution without cross-share mutation', async () => {
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

    const cancelledOfficial = await capabilityLink([], ['jp1'], 1)
    const cancelledResolved = network.resolve(cancelledOfficial)
    expect(cancelledResolved.report?.url).toBeUndefined()
    const cancelledNotified = vi.fn()
    cancelledResolved.report?.subscribe(cancelledNotified)
    cancelledResolved.report?.close()

    const selfHosted = await capabilityLink([independentRelay], [], 2)
    const selfHostedResolved = network.resolve(selfHosted)
    if (selfHostedResolved.report) reports.push(selfHostedResolved.report)

    const officialAlias = await capabilityLink([], ['jp1'], 3)
    const aliasResolved = network.resolve(officialAlias)
    if (aliasResolved.report) reports.push(aliasResolved.report)
    const aliasObserved: Array<string | undefined> = []
    aliasResolved.report?.subscribe(() => aliasObserved.push(aliasResolved.report?.url))

    finishLoad()
    await Promise.all([cancelledResolved.candidates.all(), aliasResolved.candidates.all()])
    expect(cancelledResolved.report?.url).toBeUndefined()
    expect(cancelledNotified).not.toHaveBeenCalled()
    expect(selfHostedResolved.report?.url).toBeUndefined()
    expect(aliasResolved.report?.url).toBe(`${api}/report#share=${officialAlias.shareId}`)
    expect(aliasObserved).toEqual([`${api}/report#share=${officialAlias.shareId}`])

    // Resolving a second share must not overwrite or clear an existing share's report handle.
    const officialExplicit = await capabilityLink(['wss://JP1.example:443/'], [], 4)
    const explicitResolved = network.resolve(officialExplicit)
    if (explicitResolved.report) reports.push(explicitResolved.report)
    expect(explicitResolved.report?.url).toBe(`${api}/report#share=${officialExplicit.shareId}`)
    expect(aliasResolved.report?.url).toBe(`${api}/report#share=${officialAlias.shareId}`)

    const hybrid = await capabilityLink([independentRelay], ['jp1'], 5)
    const hybridResolved = network.resolve(hybrid)
    if (hybridResolved.report) reports.push(hybridResolved.report)
    await hybridResolved.candidates.all()
    expect(hybridResolved.report?.url).toBe(`${api}/report#share=${hybrid.shareId}`)
    expect(explicitResolved.report?.url).toBe(`${api}/report#share=${officialExplicit.shareId}`)

    const secondSelfHosted = network.resolve(selfHosted)
    if (secondSelfHosted.report) reports.push(secondSelfHosted.report)
    expect(secondSelfHosted.report?.url).toBeUndefined()
    expect(hybridResolved.report?.url).toBe(`${api}/report#share=${hybrid.shareId}`)

    expect(request).not.toHaveBeenCalled()
    expect(openDatabase).not.toHaveBeenCalled()
  })

  it('leaves report handles absent when release configuration has no authority', async () => {
    const network = new BuiltinNetwork(undefined, {
      release: { ...testRelease, authority: null },
      directory: { cache: new MemoryDirectoryCache(), now: () => vectorNow },
    })
    networks.push(network)
    const officialAlias = await capabilityLink([], ['jp1'], 6)
    const resolved = network.resolve(officialAlias)
    await resolved.candidates.all()
    expect(resolved.report).toBeUndefined()
  })
})
