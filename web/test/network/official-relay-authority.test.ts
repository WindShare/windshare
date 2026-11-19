import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import fixture from '../../../testdata/network/authority.json'
import { OfficialRelayAuthority } from '../../src/application/network/authority'
import type { NetworkSnapshot } from '../../src/network/node-directory'

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
    ready: Promise.resolve(),
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
    await independent.ready
    await vi.advanceTimersByTimeAsync(120_000)
    expect(request).not.toHaveBeenCalled()
    independent.close()
  })

  it('shares one refresh cadence across official dials and pauses while only self-hosted traffic remains', async () => {
    const admission = authority.admission(share, directory(snapshot))
    let respond!: (response: Response) => void
    request.mockImplementationOnce(() => new Promise(resolve => { respond = resolve }))
    const independent = admission.watch(independentRelay, vi.fn())
    const official = admission.watch('wss://OFFICIAL.example:443/v2/ws', vi.fn())
    const parallel = admission.watch(officialRelay, vi.fn())
    await Promise.all([official.ready, parallel.ready, independent.ready])
    expect(request).toHaveBeenCalledExactlyOnceWith(`${api}/v1/blocks`, expect.anything())
    respond(new Response(null, { status: 503 }))
    await vi.advanceTimersByTimeAsync(0)

    official.close()
    await vi.advanceTimersByTimeAsync(60_000)
    expect(request).toHaveBeenCalledTimes(2)
    parallel.close()

    const retry = admission.watch(officialRelay, vi.fn())
    await retry.ready
    retry.close()
    await vi.advanceTimersByTimeAsync(120_000)
    expect(request).toHaveBeenCalledTimes(2)

    const resumed = admission.watch(officialRelay, vi.fn())
    await resumed.ready
    await vi.advanceTimersByTimeAsync(0)
    expect(request).toHaveBeenCalledTimes(3)
    resumed.close()
    independent.close()
  })

  it('starts policy for an already connected endpoint discovered by a later directory refresh', async () => {
    const source = directory()
    const watch = authority.admission(share, source).watch(officialRelay, vi.fn())
    await watch.ready
    await vi.advanceTimersByTimeAsync(0)
    expect(request).not.toHaveBeenCalled()

    source.publish(snapshot)
    await vi.advanceTimersByTimeAsync(0)
    expect(request).toHaveBeenCalledTimes(1)
    watch.close()
  })
})
