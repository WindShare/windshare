import { describe, expect, it } from 'vitest'
import { ed25519 } from '@noble/curves/ed25519.js'
import fixture from '../../../testdata/network/authority.json'
import { concatBytes, decodeBase64Url, encodeBase64Url } from '../../src/crypto/bytes'
import { freshBlocks, rejectBlockRollback, verifyBlocks } from '../../src/network/authority/blocks'
import { verifyGeneration } from '../../src/network/authority/trust'
import { MemoryBlockCache, type BlockCache } from '../../src/network/authority/cache'
import { AuthorityBlockFeed } from '../../src/network/authority/feed'

const encoder = new TextEncoder()
const share = Array<number>(12).fill(7)
const shareId = encodeBase64Url(Uint8Array.from(share))
function envelope(version: number, shares: number[][] = [share], certificate = fixture.session.certificate): string {
  const body = encoder.encode(JSON.stringify({ version, shares, accounts: [], issued_at: fixture.now }))
  const signature = ed25519.sign(concatBytes([encoder.encode('windshare/public-blocks/v1\0'), body]), new Uint8Array(32).fill(42))
  return JSON.stringify({ certificate, body: encodeBase64Url(concatBytes([body, signature])) })
}

describe('official authority blocks', () => {
  it('verifies the shared Rust/Go certificate and public block vector', async () => {
    const generation = await verifyGeneration(fixture.session.certificate, fixture.root)
    expect(generation.number).toBe(1)
    const snapshot = await verifyBlocks(JSON.stringify(fixture.blocks), fixture.root)
    expect(snapshot.cursor.version).toBe(0)
    expect(freshBlocks(snapshot, fixture.now * 1000)).toBe(true)
    expect(freshBlocks(snapshot, (fixture.now + 3600) * 1000)).toBe(false)
  })
  it('rejects modified payloads, a foreign root, and noncanonical certificates', async () => {
    const signed = JSON.parse(envelope(1)) as { certificate: string; body: string }
    const body = decodeBase64Url(signed.body)!
    body[0] = body[0]! ^ 1
    await expect(verifyBlocks(JSON.stringify({ ...signed, body: encodeBase64Url(body) }), fixture.root)).rejects.toThrow('signature')
    await expect(verifyBlocks(envelope(1), encodeBase64Url(ed25519.getPublicKey(new Uint8Array(32).fill(9))))).rejects.toThrow('signature')
    const certificate = decodeBase64Url(signed.certificate)!
    const noncanonical = concatBytes([certificate.subarray(0, 1), Uint8Array.of(24, 1), certificate.subarray(2, -64)])
    const signature = ed25519.sign(concatBytes([encoder.encode('windshare/authority-certificate/v1\0'), noncanonical]), new Uint8Array(32).fill(41))
    await expect(verifyGeneration(encodeBase64Url(concatBytes([noncanonical, signature])), fixture.root)).rejects.toThrow('strict CBOR')
  })
  it('keeps the block revision barrier independent of signing generations and time', async () => {
    const previous = await verifyBlocks(envelope(3), fixture.root)
    const rotatedOlder = await verifyBlocks(envelope(2, [], fixture.next_certificate), fixture.root)
    expect(() => rejectBlockRollback(previous.cursor, rotatedOlder.cursor)).toThrow('rollback')
    const conflicting = await verifyBlocks(envelope(3, []), fixture.root)
    expect(() => rejectBlockRollback(previous.cursor, conflicting.cursor)).toThrow('equivocation')
    const next = await verifyBlocks(envelope(4, [], fixture.next_certificate), fixture.root)
    expect(() => rejectBlockRollback(previous.cursor, next.cursor)).not.toThrow()
  })
  it('retains signed cached blocks through expiry, network failure, and stale replies', async () => {
    const cache = new MemoryBlockCache()
    await cache.commit(await verifyBlocks(envelope(3), fixture.root))
    let online = false
    const feed = new AuthorityBlockFeed({
      url: 'https://authority.example', root: fixture.root, cache, now: () => (fixture.now + 7200) * 1000,
      request: async () => {
        if (!online) throw new Error('offline')
        return new Response(envelope(2, []))
      },
    })
    try {
      await feed.ready
      expect(feed.blocked(shareId)).toBe(true)
      await feed.refresh()
      online = true
      await feed.refresh()
      expect(feed.blocked(shareId)).toBe(true)
    } finally { feed.close() }
  })
  it('never publishes an unblock rejected by another tab?s newer revision', async () => {
    const cache = new MemoryBlockCache()
    await cache.commit(await verifyBlocks(envelope(3), fixture.root))
    let release!: () => void
    const network = new Promise<void>(resolve => { release = resolve })
    const feed = new AuthorityBlockFeed({
      url: 'https://authority.example', root: fixture.root, cache, now: () => fixture.now * 1000,
      request: async () => { await network; return new Response(envelope(4, [])) },
    })
    const observed: boolean[] = []
    try {
      await feed.ready
      feed.observe(() => observed.push(feed.blocked(shareId)))
      await cache.commit(await verifyBlocks(envelope(5), fixture.root))
      release()
      await feed.refresh()
      expect(observed.length).toBeGreaterThan(0)
      expect(observed.every(Boolean)).toBe(true)
      expect(feed.blocked(shareId)).toBe(true)
    } finally { release(); feed.close() }
  })
  it('retains new denials through failed persistence and requires a committed unblock', async () => {
    const shared = new MemoryBlockCache()
    let canWrite = false, response = envelope(3)
    const cache: BlockCache = {
      load: () => shared.load(),
      commit: snapshot => {
        if (!canWrite) return Promise.reject(new Error('disk unavailable'))
        return shared.commit(snapshot)
      },
    }
    const feed = new AuthorityBlockFeed({
      url: 'https://authority.example', root: fixture.root, cache, now: () => fixture.now * 1000,
      request: async () => new Response(response),
    })
    try {
      await feed.ready
      await feed.refresh()
      expect(feed.blocked(shareId)).toBe(true)
      response = envelope(4, [])
      await feed.refresh()
      expect(feed.blocked(shareId)).toBe(true)
      canWrite = true
      await feed.refresh()
      expect(feed.blocked(shareId)).toBe(false)
    } finally { feed.close() }
  })
  it('reuses verified bytes while still adopting a newer revision from another tab', async () => {
    const shared = new MemoryBlockCache()
    const original = envelope(1, [])
    await shared.commit(await verifyBlocks(original, fixture.root))
    const feed = new AuthorityBlockFeed({
      url: 'https://authority.example', root: fixture.root, now: () => fixture.now * 1000,
      request: async () => new Response(original),
      cache: {
        load: () => shared.load(),
        commit: () => Promise.reject(new Error('An identical response needs no write')),
      },
    })
    const transitions: string[] = []
    try {
      await feed.ready
      feed.observe(() => transitions.push(feed.blocked(shareId) ? 'blocked' : 'unblocked'))
      await feed.refresh()
      expect(transitions).toEqual([])
      await shared.commit(await verifyBlocks(envelope(2), fixture.root))
      await feed.refresh()
      expect(transitions).toEqual(['blocked'])
      expect(feed.blocked(shareId)).toBe(true)
    } finally { feed.close() }
  })
  it('applies a fresh unblock and never lets an older cached reply restore it', async () => {
    const cache = new MemoryBlockCache()
    await cache.commit(await verifyBlocks(envelope(3), fixture.root))
    const feed = new AuthorityBlockFeed({
      url: 'https://authority.example', root: fixture.root, cache, now: () => fixture.now * 1000,
      request: async () => new Response(envelope(4, [])),
    })
    try {
      await feed.ready
      await feed.refresh()
      expect(feed.blocked(shareId)).toBe(false)
      expect((await cache.load())?.cursor.version).toBe(4)
      await expect(cache.commit(await verifyBlocks(envelope(3), fixture.root))).rejects.toThrow('rollback')
    } finally { feed.close() }
  })
})
