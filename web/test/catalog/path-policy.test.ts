import { describe, expect, it } from 'vitest'

import {
  canonicalizePortableCatalogPath,
  catalogNameCollisionKey,
  catalogPathCollisionKey,
  isPortableCatalogName,
  V2_CATALOG_PATH_BYTES,
  V2_CATALOG_PATH_DEPTH,
  V2_PATH_POLICY,
  V2_PATH_POLICY_UNICODE_VERSION,
} from '../../src/catalog/path-policy'
import { loadVectorFile } from '../vectors'

interface PathPolicyVector {
  readonly name: string
  readonly input?: string
  readonly expected?: 'valid' | 'invalid-path'
  readonly canonical?: string
  readonly collisionKey?: string
  readonly collisionGroup?: string
  readonly policyVersion?: string
  readonly unicodeVersion?: string
}

const vectors = loadVectorFile(
  new URL('../../../core/testvectors/path-policy.json', import.meta.url),
).cases as unknown as readonly PathPolicyVector[]

describe('v2 catalog path policy', () => {
  it('pins Unicode 15 NFC and full-fold collision identity', () => {
    const version = vectors.find((vector) => vector.name === 'policy-version')
    expect(V2_PATH_POLICY).toBe(version?.policyVersion)
    expect(V2_PATH_POLICY_UNICODE_VERSION).toBe(version?.unicodeVersion)
    expect(isPortableCatalogName('Ångström')).toBe(true)
    expect(catalogNameCollisionKey('Straße')).toBe(catalogNameCollisionKey('STRASSE'))
    expect(catalogNameCollisionKey('Ångström')).toBe('ångström')
  })

  it.each(vectors.filter((vector) => vector.expected === 'valid'))(
    'reproduces the portable path vector: $name',
    (vector) => {
      const canonical = canonicalizePortableCatalogPath(vector.input ?? '')
      expect(canonical).toBe(vector.canonical)
      expect(catalogPathCollisionKey(canonical)).toBe(vector.collisionKey)
    },
  )

  it.each(vectors.filter((vector) => vector.expected === 'invalid-path'))(
    'rejects the hostile portable path vector: $name',
    (vector) => {
      expect(() => canonicalizePortableCatalogPath(vector.input ?? '')).toThrow(TypeError)
    },
  )

  it('keeps every declared collision group internally consistent', () => {
    const groups = new Map<string, Set<string>>()
    for (const vector of vectors) {
      if (vector.collisionGroup === undefined || vector.collisionKey === undefined) continue
      const keys = groups.get(vector.collisionGroup) ?? new Set<string>()
      keys.add(vector.collisionKey)
      groups.set(vector.collisionGroup, keys)
    }
    for (const keys of groups.values()) expect(keys.size).toBe(1)
  })

  it.each([
    '',
    '.',
    '..',
    '/absolute',
    'a/b',
    'a\\b',
    'C:drive',
    'trailing-space ',
    'trailing-dot.',
    'CON.txt',
    'safe\u007fname',
    'zero\u200bwidth',
    'A\u030a',
    String.fromCharCode(0xd800),
  ])('rejects a non-portable catalog segment: %j', (name) => {
    expect(isPortableCatalogName(name)).toBe(false)
    expect(() => catalogNameCollisionKey(name)).toThrow(TypeError)
  })

  it('bounds canonical names by UTF-8 bytes rather than UTF-16 units', () => {
    expect(isPortableCatalogName('界'.repeat(85))).toBe(true)
    expect(isPortableCatalogName('界'.repeat(86))).toBe(false)
  })

  it('enforces the shared protocol path depth and byte boundaries', () => {
    const maximumDepth = Array.from({ length: V2_CATALOG_PATH_DEPTH }, () => 'a').join('/')
    expect(canonicalizePortableCatalogPath(maximumDepth)).toBe(maximumDepth)
    expect(() => canonicalizePortableCatalogPath(`${maximumDepth}/a`)).toThrow(TypeError)

    const maximumBytes = [
      ...Array.from({ length: 127 }, () => 'a'.repeat(255)),
      'a'.repeat(252),
      'b',
      'c',
    ].join('/')
    expect(new TextEncoder().encode(maximumBytes)).toHaveLength(V2_CATALOG_PATH_BYTES)
    expect(canonicalizePortableCatalogPath(maximumBytes)).toBe(maximumBytes)
    const overBytes = [
      ...Array.from({ length: 127 }, () => 'a'.repeat(255)),
      'a'.repeat(253),
      'b',
      'c',
    ].join('/')
    expect(() => canonicalizePortableCatalogPath(overBytes))
      .toThrow(TypeError)
  })
})
