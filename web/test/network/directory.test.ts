import { readFileSync } from 'node:fs'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { resolveLinkRelays } from '../../src/application/network/resolve'
import { encodeSuite02CapabilityKey } from '../../src/crypto/suite02-link'
import { MemoryDirectoryCache } from '../../src/network/directory-cache'
import { NetworkDirectory, type DirectoryObservation } from '../../src/network/directory'
import { rejectRollback, usableDirectory, verifyDirectory, type NetworkRoot } from '../../src/network/node-directory'

const vectors = JSON.parse(readFileSync(new URL('../../../testdata/network/directory.json', import.meta.url), 'utf8')) as {
  network: string; now: string; roots: NetworkRoot[]; entries: { name: string; envelope: unknown; valid: boolean }[]
}
function envelope(name: string): string { return JSON.stringify(vectors.entries.find(entry => entry.name === name)!.envelope) }
const verify = (name: string) => verifyDirectory(envelope(name), vectors.network, vectors.roots)
const now = Date.parse(vectors.now)
const directories: NetworkDirectory[] = []
afterEach(() => { for (const directory of directories.splice(0)) directory.close() })

describe('root-authorized network directory', () => {
  for (const vector of vectors.entries) it(`shares Go exact-byte vector: ${vector.name}`, async () => {
    if (vector.valid) expect((await verify(vector.name)).cursor.generation).toBeGreaterThan(0)
    else await expect(verify(vector.name)).rejects.toThrow()
  })
  it('preserves generation history after expiry and rejects serial rollback', async () => {
    const first = await verify('valid'), old = await verify('older_serial'), rotated = await verify('new_generation')
    expect(() => rejectRollback(first.cursor, old.cursor)).toThrow()
    expect(() => rejectRollback(first.cursor, rotated.cursor)).not.toThrow()
    const expired = await verify('expired_generation')
    expect(usableDirectory(expired, now + 8 * 86400_000)).toBe(false)
    expect(() => rejectRollback(expired.cursor, rotated.cursor)).toThrow()
  })
  it('retains alias tombstones through removal and root-authorized generations', async () => {
    const cache = new MemoryDirectoryCache()
    await cache.commit(await verify('valid'))
    await cache.commit(await verify('draining'))
    await cache.commit(await verify('removed'))
    await expect(cache.commit(await verify('reassigned'))).rejects.toThrow('alias reassigned')
    expect((await cache.load())!.aliases.jp1).toBe('wss://jp1.example/v2/ws')
  })
  it('races cache commits against the same durable cursor', async () => {
    const cache = new MemoryDirectoryCache()
    const current = await verify('new_generation'), old = await verify('valid')
    const results = await Promise.allSettled([cache.commit(current), cache.commit(old)])
    expect(results.map(result => result.status)).toEqual(['fulfilled', 'rejected'])
    expect((await cache.load())!.cursor.generation).toBe(2)
  })
  it('refreshes same-origin first, shares one operation and falls back after rejected bytes', async () => {
    const calls: string[] = []
    const observations: string[] = []
    const directory = new NetworkDirectory({ network: vectors.network, roots: vectors.roots, embedded: envelope('valid'),
      origin: 'https://ws.ourworld.top', mirrors: ['https://mirror.example/directory'], now: () => now,
      cache: new MemoryDirectoryCache(), observe: value => observations.push(value.transition),
      fetch: async input => { calls.push(String(input)); return new Response(envelope(calls.length === 1 ? 'bad_signature' : 'new_generation')) },
    })
    directories.push(directory)
    const publishedGenerations: number[] = []
    directory.observeSnapshot(() => publishedGenerations.push(directory.snapshot!.cursor.generation))
    await Promise.all([directory.refresh(), directory.refresh()])
    expect(calls).toEqual(['https://ws.ourworld.top/.well-known/windshare-network', 'https://mirror.example/directory'])
    expect(observations).toEqual(['rejected', 'accepted'])
    expect(publishedGenerations).toEqual([1, 2])
    expect(directory.snapshot!.cursor.generation).toBe(2)
  })
  it('adds missing aliases to an in-flight startup pass and tries live mirrors before the release asset', async () => {
    const cold = 'https://github.com/releases/directory'
    const live = ['https://ws.ourworld.top/directory', 'https://windshare.cc/directory']
    const requests: string[] = []
    let release!: (response: Response) => void
    let began!: () => void
    const entered = new Promise<void>(resolve => { began = resolve })
    const directory = new NetworkDirectory({ network: vectors.network, roots: vectors.roots,
      mirrors: [cold, ...live], random: () => 0.99, now: () => now, cache: new MemoryDirectoryCache(),
      fetch: input => {
        requests.push(String(input))
        if (requests.length === 1) { began(); return new Promise<Response>(resolve => { release = resolve }) }
        return Promise.resolve(new Response(envelope('reassigned')))
      },
    })
    directories.push(directory)
    await entered
    const target = directory.refresh(['jp1'])
    release(new Response(envelope('removed')))
    await target
    expect(requests).toEqual(live)
    expect(directory.snapshot?.nodes[0]?.alias).toBe('jp1')
    await directory.refresh(['missing'])
    expect(requests.slice(2)).toEqual([...live, cold])
    await directory.refresh()
    expect(requests.slice(5)).toEqual([live[0]])
  })
  it('emits a verified local alias while another alias is still waiting on the network', async () => {
    let finish!: (value: Response) => void
    const pending = new Promise<Response>(resolve => { finish = resolve })
    const directory = new NetworkDirectory({ network: vectors.network, roots: vectors.roots, embedded: envelope('valid'),
      mirrors: ['https://mirror.example/directory'], now: () => now, cache: new MemoryDirectoryCache(), fetch: () => pending })
    directories.push(directory)
    const key = await encodeSuite02CapabilityKey(new Uint8Array(16), new Uint8Array(16))
    const known = 'https://explicit.example'
    const candidates = resolveLinkRelays({ ...key, relays: [known], nodeAliases: ['jp1', 'missing'] }, directory)
    expect(candidates.bases).toContain(known)
    await directory.ready
    expect(candidates.bases).toEqual([known, 'https://jp1.example'])
    expect(candidates.expanding).toBe(true)
    finish(new Response(envelope('new_generation')))
    await expect(candidates.all()).resolves.toEqual([known, 'https://jp1.example'])
  })
  it('publishes only the authoritative initial snapshot when the release seed supersedes the cache', async () => {
    const cache = new MemoryDirectoryCache()
    await cache.commit(await verify('valid'))
    const directory = new NetworkDirectory({ network: vectors.network, roots: vectors.roots, embedded: envelope('new_generation'),
      mirrors: [], now: () => now, cache,
    })
    directories.push(directory)
    const publishedGenerations: number[] = []
    directory.observeSnapshot(() => publishedGenerations.push(directory.snapshot!.cursor.generation))

    await directory.ready
    expect(publishedGenerations).toEqual([2])
  })
  it('resolves an alias-only link and keeps refreshing when IndexedDB access is denied', async () => {
    const key = await encodeSuite02CapabilityKey(new Uint8Array(16), new Uint8Array(16))
    const denial = new DOMException('Storage access denied', 'SecurityError')
    const open = vi.fn(() => { throw denial })
    vi.stubGlobal('indexedDB', { open })
    const observations: DirectoryObservation[] = []
    const fetch = vi.fn(async () => new Response(envelope('new_generation')))
    const directory = new NetworkDirectory({ network: vectors.network, roots: vectors.roots, embedded: envelope('valid'),
      mirrors: ['https://mirror.example/directory'], now: () => now, fetch, observe: value => observations.push(value),
    })
    directories.push(directory)

    const candidates = resolveLinkRelays({ ...key, relays: [], nodeAliases: ['jp1'] }, directory)
    await expect(candidates.all()).resolves.toEqual(['https://jp1.example'])
    await directory.refresh()
    expect(directory.snapshot?.cursor.generation).toBe(2)
    expect(open).toHaveBeenCalledTimes(1)
    expect(fetch).toHaveBeenCalled()
    expect(observations[0]).toMatchObject({ transition: 'storage_fallback', failure: denial })
  })
  it('preserves a database open failure without an explicit storage policy denial', async () => {
    const failure = new DOMException('Failed to open the backing store', 'UnknownError')
    vi.stubGlobal('indexedDB', { open: () => { throw failure } })
    const fetch = vi.fn()
    const directory = new NetworkDirectory({ network: vectors.network, roots: vectors.roots, embedded: envelope('valid'),
      mirrors: ['https://mirror.example/directory'], now: () => now, fetch,
    })
    directories.push(directory)

    await expect(directory.ready).rejects.toBe(failure)
    await expect(directory.refresh()).rejects.toBe(failure)
    expect(directory.snapshot).toBeUndefined()
    expect(fetch).not.toHaveBeenCalled()
  })
  it('keeps an expired higher generation instead of reviving an older release seed', async () => {
    const cache = new MemoryDirectoryCache()
    await cache.commit(await verify('expired_generation'))
    let calls = 0
    const directory = new NetworkDirectory({ network: vectors.network, roots: vectors.roots, embedded: envelope('valid'),
      mirrors: ['https://mirror.example/directory'], now: () => now + 8 * 86400_000, cache,
      fetch: async () => { calls++; return new Response(envelope('valid')) },
    })
    directories.push(directory)
    await expect(directory.refresh()).rejects.toThrow()
    await expect(directory.refresh()).rejects.toThrow('backoff')
    expect(calls).toBe(1)
    expect(directory.snapshot).toBeUndefined()
    expect((await cache.load())!.cursor.generation).toBe(4)
  })
})
