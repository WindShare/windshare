import { readFileSync } from 'node:fs'
import { afterEach, describe, expect, it } from 'vitest'
import { MemoryDirectoryCache } from '../../src/network/directory-cache'
import { NetworkDirectory } from '../../src/network/directory'
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
    await Promise.all([directory.refresh(), directory.refresh()])
    expect(calls).toEqual(['https://ws.ourworld.top/.well-known/windshare-network', 'https://mirror.example/directory'])
    expect(observations).toEqual(['rejected', 'accepted'])
    expect(directory.snapshot!.cursor.generation).toBe(2)
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
