import { describe, expect, it } from 'vitest'
import { encode, rfc8949EncodeOptions } from 'cborg'
import { decodeCatalogPage, decodeDirectoryFailure, decodeShareDescriptor, openCatalogObject, PATH_POLICY } from '../../src/catalog/records'
import { canonicalizeCatalogPath, snapshotCatalogPath } from '../../src/catalog/path-policy'
import { sha256 } from '../../src/crypto/digest'
import { decodeCanonicalCbor, encodeCanonicalCbor } from '../../src/protocol/cbor'
import { decodeMessage, encodeMessage, MESSAGE_KIND } from '../../src/session/message'
import { createSignedCatalogFixture } from './signed-catalog-fixture'

const name = '\u{16D63}\u{16D67}.txt'
const id = (n: number) => new Uint8Array(16).fill(n)
const wire = (value: unknown) => encode(value, rfc8949EncodeOptions)
const entry = [2, id(4), name, 12, null, 0, 0]
const page = () => new Map<number, unknown>([
  [0, 1], [1, id(1)], [2, id(2)], [3, id(3)], [4, 0],
  [5, true], [6, new Uint8Array(32)], [7, [[...entry]]], [8, 0],
])

describe('catalog field-owned NFC', () => {
  it.each(['\ud800', '\udc00'])('rejects malformed UTF-16 in local path APIs: %j', (text) => {
    expect(() => snapshotCatalogPath([text])).toThrow()
    expect(() => canonicalizeCatalogPath(text)).toThrow()
  })

  it('authenticates a name in an opaque catalog result without changing its bytes', async () => {
    const fixture = await createSignedCatalogFixture()
    try {
      const directoryId = fixture.descriptor.syntheticRoot
      const sealed = await fixture.sealPage({ directoryId, generation: id(3), pageIndex: 0,
        terminal: true, previousCommitment: new Uint8Array(32),
        entries: [{ kind: 'file', id: id(4), name, expectedSize: 12n }] })
      const body = encodeCanonicalCbor(new Map<number, unknown>([[0, 1], [1, sealed.object]]))
      const message = encodeMessage(MESSAGE_KIND.catalogResult, id(5), body)
      const decoded = decodeMessage(message.plaintext)
      expect(decoded.body).toEqual(body)
      const fields = decodeCanonicalCbor(decoded.body, 65536, 'catalog result') as Map<number, Uint8Array>
      const object = fields.get(1)!
      expect(object).toEqual(sealed.object)
      const result = await openCatalogObject(object, fixture.descriptor, fixture.readSecret, { directoryId, pageIndex: 0, generation: id(3) })
      expect(result.kind).toBe('page')
      if (result.kind !== 'page') throw new Error('Expected a catalog page')
      expect(result.page.entries[0]?.name).toBe(name)
      expect(result.page.objectCommitment).toEqual(await sha256(sealed.object))
      expect(snapshotCatalogPath([name])).toEqual([name])
      expect(canonicalizeCatalogPath(`${name}/child`)).toBe(`${name}/child`)
      const corrupt = object.slice()
      corrupt[corrupt.length - 1]! ^= 1
      await expect(openCatalogObject(corrupt, fixture.descriptor, fixture.readSecret, { directoryId, pageIndex: 0, generation: id(3) })).rejects.toThrow()
    } finally { fixture.close() }
  })

  it.each(['e\u0301', '\ufeffa.txt', 'a\ufeff.txt', '../x', 'a/b', 'a\\b', '..', ''])('rejects unsafe name %j', async (text) => {
    const fields = page()
    fields.set(7, [[2, id(4), text, 12, null, 0, 0]])
    await expect(decodeCatalogPage(wire(fields), id(1))).rejects.toThrow()
  })

  it('rejects malformed UTF-8 instead of decoding a replacement scalar', async () => {
    // Replace the first ASCII name byte with a lone continuation byte.
    const encoded = wire(page())
    const offset = encoded.indexOf(0x2e, encoded.length - 12)
    expect(offset).toBeGreaterThan(0)
    encoded[offset] = 0x80
    await expect(decodeCatalogPage(encoded, id(1))).rejects.toThrow(/strict CBOR/)
  })

  it('does not admit text into any non-name page or entry field', async () => {
    for (const key of [0, 1, 2, 3, 4, 5, 6, 8]) {
      const fields = page()
      fields.set(key, name)
      await expect(decodeCatalogPage(wire(fields), id(1))).rejects.toThrow()
    }
    for (const index of [0, 1, 3, 4, 5, 6]) {
      const fields = page()
      const invalid = [...entry]
      invalid[index] = name
      fields.set(7, [invalid])
      await expect(decodeCatalogPage(wire(fields), id(1))).rejects.toThrow()
    }
    for (const invalid of [null, id(1), [name], new Map([[0, name]])]) {
      const fields = page()
      fields.set(7, [[2, id(4), invalid, 12, null, 0, 0]])
      await expect(decodeCatalogPage(wire(fields), id(1))).rejects.toThrow()
    }
  })

  it('keeps descriptor policy and directory failures outside the name policy', () => {
    const descriptor = new Map<number, unknown>([
      [0, 1], [1, 2], [2, 2], [3, id(1)], [4, id(2)], [5, 1 << 20],
      [6, 0], [7, new Uint8Array(32).fill(1)], [8, 0], [9, PATH_POLICY],
    ])
    descriptor.set(9, name)
    expect(() => decodeShareDescriptor(wire(descriptor))).toThrow()
    const failure = new Map<number, unknown>([[0, 1], [1, id(1)], [2, id(2)], [3, id(3)], [4, 0x2001], [5, false], [6, null]])
    for (const key of failure.keys()) {
      const invalid = new Map(failure)
      invalid.set(key, name)
      expect(() => decodeDirectoryFailure(wire(invalid))).toThrow()
    }
  })
})
