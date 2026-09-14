import { describe, expect, it } from 'vitest'
import { encodeSuite02CapabilityKey } from '../../src/crypto/suite02-link'
import { capabilityFromInput, capabilityInputFingerprint } from '../../src/ui/capability/input'

describe('capability input identity', () => {
  it('recognizes a complete link and its separate key as the same capability', async () => {
    const key = await encodeSuite02CapabilityKey(new Uint8Array(16).fill(1), new Uint8Array(16).fill(2))
    const pageUrl = 'https://receiver.invalid/?r=https%3A%2F%2Frelay.invalid'
    const link = `https://receiver.invalid/s/${key.shareId}?r=https%3A%2F%2Frelay.invalid#${key.encoded}`
    const expected = await capabilityInputFingerprint(link, pageUrl)
    expect(expected).toBeDefined()
    expect(expected).not.toContain(key.encoded)
    expect(await capabilityInputFingerprint(` #${key.encoded} `, pageUrl)).toBe(expected)
    expect(await capabilityFromInput(key.encoded, pageUrl)).toMatchObject({
      shareId: key.shareId, relayHints: ['https://relay.invalid'],
    })
  })

  it('distinguishes read credentials and relay routes, and never accepts a mismatched URL route', async () => {
    const first = await encodeSuite02CapabilityKey(new Uint8Array(16).fill(1), new Uint8Array(16).fill(2))
    const second = await encodeSuite02CapabilityKey(new Uint8Array(16).fill(3), new Uint8Array(16).fill(2))
    const pageUrl = 'https://receiver.invalid/'
    const expected = await capabilityInputFingerprint(first.encoded, pageUrl)
    expect(await capabilityInputFingerprint(second.encoded, pageUrl)).not.toBe(expected)
    expect(await capabilityInputFingerprint(first.encoded, `${pageUrl}?r=https://other.invalid`)).not.toBe(expected)
    expect(await capabilityInputFingerprint(`${pageUrl}s/invalid#${first.encoded}`, pageUrl)).toBeUndefined()
    expect(await capabilityInputFingerprint('invalid-key', pageUrl)).toBeUndefined()
    await expect(capabilityFromInput(first.encoded, 'invalid-url')).rejects.toThrow()
  })
})
