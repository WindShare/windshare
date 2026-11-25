import { describe, expect, it } from 'vitest'
import type { PublicWallResponse } from '../src/model'
import { computePublicWallEtag, matchesIfNoneMatch } from '../src/wall-store'
import { createInMemoryWallStoreWithDb, createWallStoreFromDb } from './wall-harness'

describe('public wall strong ETag representation binding', () => {
  it('binds ETags to serialized representation bytes so same-version schema or projection changes invalidate caches', () => {
    const legacyJson = JSON.stringify({ version: 5, entries: [] })
    const rankedJson = JSON.stringify({ version: 5, entries: [], topSupporters: [] })

    const legacyDigestTag = computePublicWallEtag(5, legacyJson)
    const rankedDigestTag = computePublicWallEtag(5, rankedJson)

    expect(legacyDigestTag).toMatch(/^"wall-v5-[0-9a-f]{16}"$/)
    expect(rankedDigestTag).toMatch(/^"wall-v5-[0-9a-f]{16}"$/)
    expect(rankedDigestTag).not.toBe(legacyDigestTag)

    // Legacy pre-ranking version-only ETags (strong or edge-weakened) never match the content-hashed ETag.
    expect(matchesIfNoneMatch('"wall-v5"', rankedDigestTag)).toBe(false)
    expect(matchesIfNoneMatch('W/"wall-v5"', rankedDigestTag)).toBe(false)
    expect(matchesIfNoneMatch(legacyDigestTag, rankedDigestTag)).toBe(false)
    expect(matchesIfNoneMatch(rankedDigestTag, rankedDigestTag)).toBe(true)
    expect(matchesIfNoneMatch(`W/${rankedDigestTag}`, rankedDigestTag)).toBe(true)
  })

  it('produces identical ETags across Durable Object cold starts and invalidates only on wall mutations', () => {
    const { store, db } = createInMemoryWallStoreWithDb()

    const initial = store.getPublicWallSnapshot()
    expect(initial.etag).toBe(computePublicWallEtag(0, initial.json))
    expect(initial.etag).toMatch(/^"wall-v0-[0-9a-f]{16}"$/)
    expect(createWallStoreFromDb(db).getPublicWallSnapshot().etag).toBe(initial.etag)

    const committed = store.commitDonation({
      provider: 'kofi',
      externalId: 'KOFI-ETAG-UNIT-1',
      displayName: 'Alice',
      amountMinor: 2500,
      currency: 'USD',
      minorDigits: 2,
      message: 'Keep shipping!',
      isPublic: true,
      donatedAt: 1_760_000_100,
    }, 1_760_000_100)
    expect(committed.outcome).toBe('inserted')

    const afterCommit = store.getPublicWallSnapshot()
    const payload = JSON.parse(afterCommit.json) as PublicWallResponse
    expect(payload.version).toBe(1)
    expect(payload.topSupporters).toHaveLength(1)
    expect(afterCommit.etag).toBe(computePublicWallEtag(1, afterCommit.json))
    expect(afterCommit.etag).not.toBe(initial.etag)
    expect(matchesIfNoneMatch('"wall-v1"', afterCommit.etag)).toBe(false)

    // A fresh WallStore reading the same SQLite state computes the exact same ETag.
    const coldStarted = createWallStoreFromDb(db).getPublicWallSnapshot()
    expect(coldStarted.etag).toBe(afterCommit.etag)
    expect(coldStarted.json).toBe(afterCommit.json)

    // Config updates do not change the public wall representation or ETag.
    store.patchConfig({ moderationEnabled: true })
    expect(store.getPublicWallSnapshot().etag).toBe(afterCommit.etag)
    expect(createWallStoreFromDb(db).getPublicWallSnapshot().etag).toBe(afterCommit.etag)
  })
})

describe('RFC 9110 If-None-Match parsing', () => {
  const target = '"wall-v3-0123456789abcdef"'

  it('matches strong tags, edge-weakened tags, wildcard, and valid entity-tag lists', () => {
    expect(matchesIfNoneMatch(target, target)).toBe(true)
    expect(matchesIfNoneMatch(`W/${target}`, target)).toBe(true)
    expect(matchesIfNoneMatch('*', target)).toBe(true)
    expect(matchesIfNoneMatch(' \t*\t ', target)).toBe(true)
    expect(matchesIfNoneMatch(`"old-tag", ${target}`, target)).toBe(true)
    expect(matchesIfNoneMatch(`"other,tag", W/${target}`, target)).toBe(true)
    expect(matchesIfNoneMatch(` , , ${target} , `, target)).toBe(true)
    expect(matchesIfNoneMatch('"", ' + target, target)).toBe(true)
    expect(matchesIfNoneMatch('"v1,gzip"', '"v1,gzip"')).toBe(true)
    expect(matchesIfNoneMatch('W/"v1,gzip"', '"v1,gzip"')).toBe(true)
  })

  it('does not split quoted commas or treat quoted asterisks as wildcards', () => {
    expect(matchesIfNoneMatch('"foo,*,bar"', target)).toBe(false)
    expect(matchesIfNoneMatch('"*"', target)).toBe(false)
    expect(matchesIfNoneMatch('"other,tag"', target)).toBe(false)
    expect(matchesIfNoneMatch(`"${target.slice(1, -1)},extra"`, target)).toBe(false)
  })

  it('rejects malformed weak prefixes, mixed wildcards, and invalid syntax', () => {
    expect(matchesIfNoneMatch(null, target)).toBe(false)
    expect(matchesIfNoneMatch('', target)).toBe(false)
    expect(matchesIfNoneMatch(' , , ', target)).toBe(false)
    expect(matchesIfNoneMatch(`W/ ${target}`, target)).toBe(false)
    expect(matchesIfNoneMatch(`W/\t${target}`, target)).toBe(false)
    expect(matchesIfNoneMatch(`w/${target}`, target)).toBe(false)
    expect(matchesIfNoneMatch('W/*', target)).toBe(false)
    expect(matchesIfNoneMatch('W/', target)).toBe(false)
    expect(matchesIfNoneMatch(`${target}, *`, target)).toBe(false)
    expect(matchesIfNoneMatch(`*, ${target}`, target)).toBe(false)
    expect(matchesIfNoneMatch(`${target} "another"`, target)).toBe(false)
    expect(matchesIfNoneMatch(`${target}, "unterminated`, target)).toBe(false)
    expect(matchesIfNoneMatch(`${target}, "has space"`, target)).toBe(false)
    expect(matchesIfNoneMatch(`"foo", W/ ${target}`, target)).toBe(false)
    expect(matchesIfNoneMatch('*', 'not-a-valid-etag')).toBe(false)
  })
})
