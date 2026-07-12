import { describe, expect, it, vi } from 'vitest'

import {
  CryptoError,
  encodeCapabilityKey,
  formatCapabilityLink,
  mergeCapabilityLink,
  parseCapabilityLink,
  splitCapabilityLink,
} from '../../src/crypto'
import { b64ToBytes, loadVectorFile } from '../vectors'

interface LinkVector {
  readonly name: string
  readonly suite: number
  readonly readSecretB64: string
  readonly shareId: string
  readonly relays: readonly string[] | null
  readonly base: string
  readonly url: string
  readonly bareUrl: string
  readonly keyString: string
}

const vectors = loadVectorFile(
  new URL('../../../testvectors/link.json', import.meta.url),
).cases as unknown as readonly LinkVector[]

function capturedCryptoError(action: () => unknown): CryptoError {
  try {
    action()
  } catch (error) {
    expect(error).toBeInstanceOf(CryptoError)
    return error as CryptoError
  }
  throw new Error('expected CryptoError')
}

describe('capability link parsing', () => {
  it.each(vectors)('round-trips the Go link vector: $name', (vector) => {
    const expectedSecret = b64ToBytes(vector.readSecretB64)
    const parsed = parseCapabilityLink(vector.url)

    expect(parsed).toMatchObject({
      suite: vector.suite,
      shareId: vector.shareId,
      relayHints: vector.relays ?? [],
    })
    expect(parsed.readSecret).toEqual(expectedSecret)
    expect(encodeCapabilityKey(parsed.suite, parsed.readSecret)).toBe(vector.keyString)
    expect(formatCapabilityLink(vector.base, parsed)).toBe(vector.url)
    expect(splitCapabilityLink(vector.base, parsed)).toEqual({
      bareUrl: vector.bareUrl,
      key: vector.keyString,
    })

    for (const keyInput of [vector.keyString, `#${vector.keyString}`, vector.url]) {
      const merged = mergeCapabilityLink(vector.bareUrl, keyInput)
      expect(merged.shareId).toBe(vector.shareId)
      expect(merged.readSecret).toEqual(expectedSecret)
    }
  })

  it('distinguishes missing split keys, conflicts, malformed IDs, and future suites', () => {
    const vector = vectors[0]
    if (vector === undefined) {
      throw new Error('link vector is missing')
    }
    expect(capturedCryptoError(() => parseCapabilityLink(vector.bareUrl)).code).toBe(
      'missing-key',
    )
    expect(
      capturedCryptoError(() =>
        mergeCapabilityLink(vector.url, vectors[1]?.keyString ?? 'missing'),
      ).code,
    ).toBe('key-conflict')
    expect(
      capturedCryptoError(() =>
        parseCapabilityLink(`https://windshare.example/not-a-share#${vector.keyString}`),
      ).code,
    ).toBe('malformed-share-id')
    expect(
      capturedCryptoError(() =>
        parseCapabilityLink(
          `https://windshare.example/${vector.shareId}#AgABAgMEBQYHCAkKCwwNDg8`,
        ),
      ).code,
    ).toBe('unsupported-suite')
    const suiteTwoKey = `A${'g'.repeat(43)}`
    expect(
      capturedCryptoError(() =>
        mergeCapabilityLink(vector.bareUrl, suiteTwoKey),
      ).code,
    ).toBe('unsupported-suite')

    const nonCanonicalTail = `${vector.keyString.slice(0, -1)}9`
    expect(
      capturedCryptoError(() =>
        mergeCapabilityLink(vector.bareUrl, nonCanonicalTail),
      ).code,
    ).toBe('malformed-key')
  })

  it('never copies a capability fragment into malformed-link diagnostics', () => {
    const marker = 'TOP-SECRET-CAPABILITY'
    const error = capturedCryptoError(() => parseCapabilityLink(`not a URL#${marker}`))

    expect(error.code).toBe('malformed-link')
    expect(error.message).not.toContain(marker)
  })

  it('matches Go URL authority and decoded-path semantics without browser repairs', () => {
    const vector = vectors[0]
    if (vector === undefined) {
      throw new Error('link vector is missing')
    }

    const escapedSeparator = parseCapabilityLink(
      `https://windshare.example/prefix%2F${vector.shareId}#${vector.keyString}`,
    )
    expect(escapedSeparator.shareId).toBe(vector.shareId)

    for (const malformed of [
      `https:windshare.example/${vector.shareId}#${vector.keyString}`,
      `https:////windshare.example/${vector.shareId}#${vector.keyString}`,
      `https://windshare.example\\${vector.shareId}#${vector.keyString}`,
      `https://windshare.example/prefix\\ignored/${vector.shareId}#${vector.keyString}`,
      `https://windshare.example/ignored\n/${vector.shareId}#${vector.keyString}`,
      `https://windshare.example/%zz/${vector.shareId}#${vector.keyString}`,
      `https://windshare.example/${vector.shareId}?r=%zz#${vector.keyString}`,
      `https://windshare.example/${vector.shareId}?r=%FF#${vector.keyString}`,
      `https://windshare.example/${vector.shareId}?r=a;b#${vector.keyString}`,
    ]) {
      const error = capturedCryptoError(() => parseCapabilityLink(malformed))
      expect(error.code).toBe('malformed-link')
      expect(error.message).not.toContain(vector.keyString)
    }

    const parsed = parseCapabilityLink(vector.url)
    expect(formatCapabilityLink('https://windshare.example/app//nested/', parsed)).toBe(
      `https://windshare.example/app/nested/${vector.shareId}?r=relay-a.example&r=relay-b.example#${vector.keyString}`,
    )
    expect(
      capturedCryptoError(() =>
        formatCapabilityLink('https:windshare.example/app', parsed),
      ).code,
    ).toBe('malformed-link')
    expect(
      capturedCryptoError(() =>
        formatCapabilityLink('https://windshare.example/app?r=%FF', parsed),
      ).code,
    ).toBe('malformed-link')

    expect(
      parseCapabilityLink(
        `https://windshare.example/${vector.shareId}?r=a%3Bb&r=c#${vector.keyString}`,
      ).relayHints,
    ).toEqual(['a;b', 'c'])
  })

  it('rejects fixed-size link fields before base64 allocation', () => {
    const vector = vectors[0]
    if (vector === undefined) {
      throw new Error('link vector is missing')
    }
    const atob = vi.spyOn(globalThis, 'atob')
    try {
      const error = capturedCryptoError(() =>
        parseCapabilityLink(
          `https://windshare.example/${'A'.repeat(1 << 20)}#${vector.keyString}`,
        ),
      )
      expect(error.code).toBe('malformed-share-id')
      expect(atob).not.toHaveBeenCalled()
    } finally {
      atob.mockRestore()
    }

    const marker = 'CAPABILITY-MARKER'
    const oversizedFragment = capturedCryptoError(() =>
      parseCapabilityLink(
        `https://windshare.example/${vector.shareId}#${marker.repeat(32)}`,
      ),
    )
    expect(oversizedFragment.code).toBe('malformed-key')
    expect(oversizedFragment.message).not.toContain(marker)
  })

  it('snapshots decoded secret bytes and freezes relay ordering', () => {
    const vector = vectors[0]
    if (vector === undefined) {
      throw new Error('link vector is missing')
    }
    const parsed = parseCapabilityLink(vector.url)

    expect(Object.isFrozen(parsed)).toBe(true)
    expect(Object.isFrozen(parsed.relayHints)).toBe(true)
    expect(() => (parsed.relayHints as unknown as string[]).reverse()).toThrow(TypeError)
  })
})
