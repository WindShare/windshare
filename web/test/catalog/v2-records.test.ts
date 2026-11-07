import { evidenceFor } from '../../src/diagnostics/failure/evidence'
import { describe, expect, it } from 'vitest'
import { encode, rfc8949EncodeOptions } from 'cborg'

import { decodeCatalogPage, decodeDirectoryFailure, decodeShareDescriptor, PATH_POLICY } from '../../src/catalog/records'
import { encodeCanonicalCbor } from '../../src/protocol/cbor'

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

describe('v2 catalog text boundaries', () => {
  it('rejects a leading format character instead of silently renaming a catalog entry', async () => {
    const page = (name: string) => encodeCanonicalCbor(new Map<number, unknown>([
      [0, 1], [1, identity(1)], [2, identity(2)], [3, identity(3)], [4, 0],
      [5, true], [6, new Uint8Array(32)],
      [7, [[2, identity(4), name, 12, null, 0, 0]]], [8, 0],
    ]))
    const senderObject = Uint8Array.of(1)
    const decoded = await decodeCatalogPage(page('a.txt'), senderObject)
    expect(decoded.entries[0]?.name).toBe('a.txt')
    await expect(decodeCatalogPage(page('\ufeffa.txt'), senderObject)).rejects.toThrow(/safe path segment/)
  })

  it('accepts a catalog entry name that is NFC under the pinned Unicode 15 path policy', async () => {
    // Go's catalog canonicalizer preserves these post-Unicode-15 Kirat Rai scalars;
    // newer browser ICU data would compose them to U+16D69.
    const name = '\u{16D63}\u{16D67}.txt'
    const page = encode(new Map<number, unknown>([
      [0, 1], [1, identity(1)], [2, identity(2)], [3, identity(3)], [4, 0],
      [5, true], [6, new Uint8Array(32)],
      [7, [[2, identity(4), name, 12, null, 0, 0]]], [8, 0],
    ]), rfc8949EncodeOptions)
    const decoded = await decodeCatalogPage(page, Uint8Array.of(1))
    expect(decoded.entries[0]?.name).toBe(name)
  })

  it('rejects a leading U+FEFF in the descriptor path policy', () => {
    const fields = new Map<number, unknown>([
      [0, 1], [1, 2], [2, 2], [3, identity(1)], [4, identity(2)], [5, 1 << 20],
      [6, 0], [7, new Uint8Array(32).fill(1)], [8, 0], [9, PATH_POLICY],
    ])
    expect(decodeShareDescriptor(encodeCanonicalCbor(fields)).pathPolicy).toBe(PATH_POLICY)
    fields.set(9, `\ufeff${PATH_POLICY}`)
    expect(() => decodeShareDescriptor(encodeCanonicalCbor(fields))).toThrow(/path policy/)
  })
})

describe('v2 directory failure retry semantics', () => {
  const failure = (code: number, retryable: boolean, retryAfter: number | null) =>
    encodeCanonicalCbor(new Map<number, unknown>([
      [0, 1],
      [1, identity(1)],
      [2, identity(2)],
      [3, identity(3)],
      [4, code],
      [5, retryable],
      [6, retryAfter],
    ]))

  it('allows retryable budget failures but requires transient I/O to be retryable', () => {
    const budget = decodeDirectoryFailure(failure(0x2005, true, 250))
    expect(budget.retryable).toBe(true)
    expect(budget.retryAfterMilliseconds).toBe(250)
    let thrown: unknown
    try { decodeDirectoryFailure(failure(0x2007, false, null)) } catch (error) { thrown = error }
    expect(thrown).toMatchObject({ name: 'CborError' })
    expect(evidenceFor(thrown)).toMatchObject({ operation: 'catalog.validate',
      context: { invariant: 'transient-retryable', expected: true, actual: false },
      exception: { message: 'Directory transient I/O must be retryable' },
    })
  })
})
