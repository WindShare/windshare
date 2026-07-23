import { describe, expect, it } from 'vitest'

import { encodeCanonicalCbor, requireText } from '../../src/protocol/cbor'

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
