import { describe, expect, it } from 'vitest'

import {
  ManifestError,
  PATH_POLICY_UNICODE_VERSION,
  canonicalizePath,
  fullCaseFold15,
  pathCollisionKey,
  quotePathForDiagnostic,
  validateCanonicalPath,
} from '../../src/manifest'
import { PATH_POLICY_VERSION } from '../../src/contracts'
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
  new URL('../../../testvectors/path-policy.json', import.meta.url),
).cases as unknown as readonly PathPolicyVector[]

describe('Unicode 15 path policy', () => {
  it('pins the policy and table versions from the shared vector', () => {
    const version = vectors.find((vector) => vector.name === 'policy-version')

    expect(PATH_POLICY_VERSION).toBe(version?.policyVersion)
    expect(PATH_POLICY_UNICODE_VERSION).toBe(version?.unicodeVersion)
  })

  it.each(vectors.filter((vector) => vector.expected === 'valid'))(
    'canonicalizes and folds the Go path vector: $name',
    (vector) => {
      const canonical = canonicalizePath(vector.input ?? '')

      expect(canonical).toBe(vector.canonical)
      expect(pathCollisionKey(canonical)).toBe(vector.collisionKey)
      expect(validateCanonicalPath(canonical)).toBe(canonical)
    },
  )

  it.each(vectors.filter((vector) => vector.expected === 'invalid-path'))(
    'rejects the hostile Go path vector: $name',
    (vector) => {
      expect(() => canonicalizePath(vector.input ?? '')).toThrowError(
        expect.objectContaining<Partial<ManifestError>>({ code: 'invalid-path' }),
      )
    },
  )

  it.each([
    '',
    '/absolute',
    'a//b',
    'a/../b',
    'a\\b',
    'C:/drive',
    'trailing-space ',
    'trailing-dot.',
    'CON.txt',
    'safe\u007fname',
  ])('rejects the corresponding Go path hazard: %j', (path) => {
    expect(() => canonicalizePath(path)).toThrowError(
      expect.objectContaining<Partial<ManifestError>>({ code: 'invalid-path' }),
    )
  })

  it('implements full folding rather than lowercase approximations', () => {
    expect(fullCaseFold15('Straße')).toBe('strasse')
    expect(fullCaseFold15('ẞ-ﬃ-ſ')).toBe('ss-ffi-s')
    expect(fullCaseFold15('İ-I-ı')).toBe('i̇-i-ı')
  })

  it('rejects malformed Unicode and bounds attacker-controlled diagnostics', () => {
    expect(() => canonicalizePath(String.fromCharCode(0xd800))).toThrowError(
      expect.objectContaining<Partial<ManifestError>>({ code: 'invalid-path' }),
    )
    const hostile = `${'\u0000'.repeat(1_024)}${'界'.repeat(1_024)}`
    const diagnostic = quotePathForDiagnostic(hostile)

    expect(new TextEncoder().encode(diagnostic).byteLength).toBeLessThanOrEqual(256)
    expect(diagnostic).toContain('…')
  })

  it('keeps allowed Unicode separators from injecting diagnostic lines', () => {
    const path = 'safe\u2028line\u2029paragraph'
    const diagnostic = quotePathForDiagnostic(path)

    expect(validateCanonicalPath(path)).toBe(path)
    expect(diagnostic).toBe('"safe\\u2028line\\u2029paragraph"')
    expect(diagnostic).not.toContain('\u2028')
    expect(diagnostic).not.toContain('\u2029')
  })

  it('keeps every declared collision group internally consistent', () => {
    const groups = new Map<string, Set<string>>()
    for (const vector of vectors) {
      if (vector.collisionGroup === undefined || vector.collisionKey === undefined) {
        continue
      }
      const keys = groups.get(vector.collisionGroup) ?? new Set<string>()
      keys.add(vector.collisionKey)
      groups.set(vector.collisionGroup, keys)
    }
    for (const keys of groups.values()) {
      expect(keys.size).toBe(1)
    }
  })
})
