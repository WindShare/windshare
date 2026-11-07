import { describe, expect, it, vi } from 'vitest'
import { encode, rfc8949EncodeOptions } from 'cborg'
import { decodeCanonicalCborWithSchema, MAXIMUM_CBOR_DEPTH, requireText } from '../../src/protocol/cbor'

describe('schema-owned CBOR text validation', () => {
  it.each([
    [0x18, 0x17], // Nonminimal integer.
    [0xa2, 0, 0, 0, 1], // Duplicate map key.
    [0xa2, 1, 0, 0, 0], // Unordered map keys.
    [0x63, 0xed, 0xa0, 0x80], // Surrogate encoded as UTF-8.
    [0x61, 0x80], // Lone continuation byte.
    [0xc0, 0], [0xf9, 0x3c, 0], [0x9f, 0xff], [0, 0],
    [...Array<number>(MAXIMUM_CBOR_DEPTH + 1).fill(0x81), 0],
  ])('rejects malformed wire before invoking the schema: %j', (...bytes) => {
    const schema = vi.fn()
    expect(() => decodeCanonicalCborWithSchema(Uint8Array.from(bytes), 1024, 'test', schema)).toThrow()
    expect(schema).not.toHaveBeenCalled()
  })

  it('enforces authenticated byte limits before invoking the schema', () => {
    const schema = vi.fn()
    for (const [bytes, limit] of [[Uint8Array.of(0), 0], [new Uint8Array(), 1], [Uint8Array.of(0x41, 0), 1]] as const) {
      expect(() => decodeCanonicalCborWithSchema(bytes, limit, 'test', schema)).toThrow()
    }
    expect(schema).not.toHaveBeenCalled()
  })

  it('retains semantic bounds even when text is validated by the schema', () => {
    const schema = vi.fn()
    for (const value of [Array(2049).fill(0), new Map(Array.from({ length: 257 }, (_, i) => [i, 0]))]) {
      expect(() => decodeCanonicalCborWithSchema(encode(value, rfc8949EncodeOptions), 65536, 'test', schema)).toThrow()
    }
    expect(schema).not.toHaveBeenCalled()
  })

  it('does not normalize text or strip U+FEFF before field validation', () => {
    const text = '\ufeffe\u0301'
    const raw = encode(text, rfc8949EncodeOptions)
    const schema = vi.fn((value: unknown) => requireText(value, 'schema text'))
    expect(() => decodeCanonicalCborWithSchema(raw, 1024, 'test', schema)).toThrow(/NFC/)
    expect(schema).toHaveBeenCalledExactlyOnceWith(text)
  })
})
