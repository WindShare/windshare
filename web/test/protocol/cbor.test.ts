import { describe, expect, it } from 'vitest'

import { decode, encode, rfc8949EncodeOptions } from 'cborg'
import { decodeCanonicalCbor, encodeCanonicalCbor, requireText, MAXIMUM_CBOR_DEPTH } from '../../src/protocol/cbor'

describe('canonical CBOR text boundary', () => {
  it('rejects lone UTF-16 surrogates before TextEncoder can replace them', () => {
    expect(() => encodeCanonicalCbor('\ud800')).toThrow(/well-formed/i)
    expect(() => encodeCanonicalCbor(['prefix', '\udc00'])).toThrow(/well-formed/i)
    expect(() => requireText('\ud800', 'test text')).toThrow(/well-formed/i)
  })

  it('accepts valid supplementary Unicode scalars', () => {
    expect(() => encodeCanonicalCbor('WindShare \ud83c\udf2c\ufe0f')).not.toThrow()
    expect(requireText('\ud83d\ude80', 'test text')).toBe('\ud83d\ude80')
  })
})

describe('canonical CBOR wire validation', () => {
  const open = (bytes: Uint8Array) => decodeCanonicalCbor(bytes, 2 << 20, 'test')

  it.each([
    [0x18, 0x17], [0x19, 0, 0xff], [0x1a, 0, 0, 0xff, 0xff],
    [0x1b, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff],
    [0x58, 1, 0], [0x5f, 0x40, 0xff], [0x9f, 0xff],
    [0xf9, 0x3c, 0], [0xf7], [0xf8, 20], [0xc0, 0],
    [0xa2, 1, 0, 0, 0], [0xa2, 0, 0, 0, 1], [0xa1, 0x20, 0],
    [0xa1, 0x1b, 0, 0x20, 0, 0, 0, 0, 0, 0, 0],
    [0x62, 0xc0, 0x80], [0x63, 0xed, 0xa0, 0x80], [0x61, 0x80],
    [0x82, 0], [0x5b, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff],
  ])('rejects unsupported wire form %j', (...value) => {
    expect(() => open(Uint8Array.from(value))).toThrow()
  })

  it('keeps a leading U+FEFF in decoded text like the Go codec', () => {
    // 0x68 = text(8): EF BB BF "a.txt"; 0x78 0x20 = text(32) takes cborg's TextDecoder path.
    const short = '\ufeffa.txt'
    const long = `\ufeff${'x'.repeat(29)}`
    const bomOnly = '\ufeff'
    expect(Array.from(encodeCanonicalCbor(short).subarray(0, 4))).toEqual([0x68, 0xef, 0xbb, 0xbf])
    expect(Array.from(encodeCanonicalCbor(long).subarray(0, 2))).toEqual([0x78, 0x20])
    for (const text of [short, long, bomOnly, `a${bomOnly}`]) {
      expect(open(encodeCanonicalCbor(text))).toBe(text)
    }
    const nested = new Map<number, unknown>([[0, ['', short, '', 'plain', bomOnly, '']], [1, new Map([[2, long]])]])
    expect(open(encodeCanonicalCbor(nested))).toEqual(nested)
  })

  it.each([
    [255, [0x78, 0xff]],
    [256, [0x79, 0x01, 0x00]],
    [65_535, [0x79, 0xff, 0xff]],
    [65_536, [0x7a, 0x00, 0x01, 0x00, 0x00]],
  ] as const)('preserves leading U+FEFF in %i-byte text across length headers and input offsets', (length, header) => {
    const text = `\ufeff${'x'.repeat(length - 3)}`
    const encoded = encodeCanonicalCbor(text)
    expect(encoded.subarray(0, header.length)).toEqual(Uint8Array.from(header))
    expect(open(encoded)).toBe(text)

    // Invalid surrounding bytes expose any decoder that ignores the input view's bounds.
    const offset = 5
    const padded = new Uint8Array(encoded.length + offset * 2).fill(0xff)
    padded.set(encoded, offset)
    for (const input of [padded, Buffer.from(padded)]) {
      expect(open(input.subarray(offset, offset + encoded.length))).toBe(text)
    }
  })

  it('decodes byte strings as plain Uint8Array even from a Buffer input', () => {
    const decoded = open(Buffer.from(encodeCanonicalCbor(Uint8Array.of(1, 2, 3))))
    expect(Object.getPrototypeOf(decoded)).toBe(Uint8Array.prototype)
    expect(decoded).toEqual(Uint8Array.of(1, 2, 3))
  })

  it('bounds nesting before decoding and rejects trailing data and non-NFC text', () => {
    expect(() => open(Uint8Array.from([...Array<number>(MAXIMUM_CBOR_DEPTH + 1).fill(0x81), 0]))).toThrow()
    expect(() => open(Uint8Array.of(0, 0))).toThrow()
    expect(() => open(encode('e\u0301'))).toThrow()
  })

  it('accepts canonical integer boundaries, nested numeric maps and opaque payloads', () => {
    const values = [0, 23, 24, 255, 256, 65_535, 65_536, 0xffff_ffff, 0x1_0000_0000,
      Number.MAX_SAFE_INTEGER, 1n << 63n, (1n << 64n) - 1n, -(1n << 64n), null, true, false,
      'WindShare \ud83d\ude80', new Uint8Array(8 << 10).fill(0xff),
      new Map<number, unknown>([[24, [new Map([[0, 'ok']])]], [0, false]])]
    for (const value of values) expect(open(encodeCanonicalCbor(value))).toEqual(decode(encodeCanonicalCbor(value), { useMaps: true }))
  })

  it('matches re-encoding validation for deterministic mutations of canonical values', () => {
    const source = encodeCanonicalCbor(new Map<number, unknown>([
      [0, [23, 24, 256, (1n << 64n) - 1n, -32]], [24, 'caf\u00e9'], [256, new Uint8Array(32).fill(0x81)],
    ]))
    const legacyAccepts = (bytes: Uint8Array): boolean => {
      try {
        const value: unknown = decode(bytes, { useMaps: true, rejectDuplicateMapKeys: true, strict: true,
          allowIndefinite: false, allowUndefined: false, allowInfinity: false, allowNaN: false })
        const canonical = encodeCanonicalCbor(value)
        return canonical.length === bytes.length && canonical.every((byte, i) => byte === bytes[i])
      } catch { return false }
    }
    for (let offset = 0; offset < source.length; offset++) {
      for (const mask of [1, 0x1f, 0x80, 0xff]) {
        const changed = source.slice()
        changed[offset] = changed[offset]! ^ mask
        let accepted = false
        try { open(changed); accepted = true } catch { /* Both validators reject malformed input. */ }
        expect(accepted, `offset ${offset}, mask ${mask}`).toBe(legacyAccepts(changed))
      }
    }
    expect(encode(open(source), rfc8949EncodeOptions)).toEqual(source)
  })
})

describe('canonical CBOR NFC version', () => {
  // U+16D63 U+16D67 are Kirat Rai scalars assigned after Unicode 15.0.0. They are
  // already NFC under the pinned path policy, which Go's catalog canonicalizer
  // preserves, but newer runtime ICU data composes them to U+16D69.
  const postPolicyName = '\u{16D63}\u{16D67}.txt'

  it('decodes text that is NFC under the pinned Unicode 15 policy', () => {
    const encoded = encode(new Map([[0, postPolicyName]]), rfc8949EncodeOptions)
    const decoded = decodeCanonicalCbor(encoded, 1024, 'catalog page') as Map<number, string>
    expect(decoded.get(0)).toBe(postPolicyName)
    expect(requireText(postPolicyName, 'catalog entry name')).toBe(postPolicyName)
    expect(() => encodeCanonicalCbor([postPolicyName])).not.toThrow()
  })

  it('still rejects text that is not NFC under the pinned policy', () => {
    const decomposed = 'e\u0301'
    expect(() => requireText(decomposed, 'catalog entry name')).toThrow(/NFC/)
    expect(() => decodeCanonicalCbor(encode([decomposed], rfc8949EncodeOptions), 1024, 'test')).toThrow(/NFC/)
  })
})
