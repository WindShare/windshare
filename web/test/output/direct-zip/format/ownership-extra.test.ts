import { describe, expect, it } from 'vitest'

import { bytesToHex } from '../../../../src/crypto/bytes'
import {
  DIRECT_ZIP_OWNERSHIP_EXTRA_DATA_BYTES,
  DIRECT_ZIP_OWNERSHIP_EXTRA_FIELD_BYTES,
  DIRECT_ZIP_OWNERSHIP_EXTRA_HEADER_ID,
  decodeDirectZipOwnershipExtraFieldsV1,
  directZipOwnershipExtraFormatCanonicalV1,
  directZipOwnershipExtraFormatDigestV1,
  encodeDirectZipOwnershipExtraFieldV1,
  equalDirectZipOwnershipMarkersV1,
  requireDirectZipOwnershipExtraFieldsV1,
} from '../../../../src/output/direct-zip/format'
import { TEST_DIRECT_ZIP_MARKER } from './test-values'

describe('DirectZip ownership extra field V1', () => {
  it('encodes the exact collision-aware 0x5357 payload', () => {
    const field = encodeDirectZipOwnershipExtraFieldV1(TEST_DIRECT_ZIP_MARKER)
    const view = new DataView(field.buffer)

    expect(field.byteLength).toBe(DIRECT_ZIP_OWNERSHIP_EXTRA_FIELD_BYTES)
    expect(view.getUint16(0, true)).toBe(DIRECT_ZIP_OWNERSHIP_EXTRA_HEADER_ID)
    expect(view.getUint16(2, true)).toBe(DIRECT_ZIP_OWNERSHIP_EXTRA_DATA_BYTES)
    expect(bytesToHex(field.subarray(4, 20))).toBe('57696e6453686172655a69704f776e00')
    expect(view.getUint16(20, true)).toBe(1)
    expect(view.getUint16(22, true)).toBe(0)
    expect(requireDirectZipOwnershipExtraFieldsV1(field)).toEqual({
      version: 1,
      ...TEST_DIRECT_ZIP_MARKER,
    })
  })

  it('ignores selector collisions but rejects duplicate matching signatures', () => {
    const canonical = encodeDirectZipOwnershipExtraFieldV1(TEST_DIRECT_ZIP_MARKER)
    const collision = Uint8Array.of(
      0x57, 0x53, 0x10, 0x00,
      ...new TextEncoder().encode('AnotherVendorSig'),
    )
    const combined = new Uint8Array(collision.byteLength + canonical.byteLength)
    combined.set(collision)
    combined.set(canonical, collision.byteLength)

    expect(decodeDirectZipOwnershipExtraFieldsV1(combined)).toEqual({
      version: 1,
      ...TEST_DIRECT_ZIP_MARKER,
    })
    const duplicate = new Uint8Array(canonical.byteLength * 2)
    duplicate.set(canonical)
    duplicate.set(canonical, canonical.byteLength)
    expect(() => decodeDirectZipOwnershipExtraFieldsV1(duplicate)).toThrow(/multiple/u)
  })

  it('rejects truncation, unsupported versions and flags, and extended matching payloads', () => {
    const canonical = encodeDirectZipOwnershipExtraFieldV1(TEST_DIRECT_ZIP_MARKER)
    expect(() => decodeDirectZipOwnershipExtraFieldsV1(canonical.subarray(0, -1)))
      .toThrow(/truncated/u)

    const version = canonical.slice()
    new DataView(version.buffer).setUint16(20, 2, true)
    expect(() => decodeDirectZipOwnershipExtraFieldsV1(version)).toThrow(/version/u)

    const flags = canonical.slice()
    new DataView(flags.buffer).setUint16(22, 1, true)
    expect(() => decodeDirectZipOwnershipExtraFieldsV1(flags)).toThrow(/flags/u)

    const extended = new Uint8Array(canonical.byteLength + 1)
    extended.set(canonical)
    new DataView(extended.buffer).setUint16(2, DIRECT_ZIP_OWNERSHIP_EXTRA_DATA_BYTES + 1, true)
    expect(() => decodeDirectZipOwnershipExtraFieldsV1(extended)).toThrow(/length/u)
    expect(decodeDirectZipOwnershipExtraFieldsV1(Uint8Array.of(1, 0, 0, 0))).toBeUndefined()
  })

  it('binds every frozen format field into a stable canonical policy digest', async () => {
    const canonical = directZipOwnershipExtraFormatCanonicalV1()
    const digest = await directZipOwnershipExtraFormatDigestV1()

    expect(new TextDecoder().decode(canonical.subarray(0, 40)))
      .toContain('windshare/direct-zip-ownership-extra/v1\0')
    expect(digest.byteLength).toBe(32)
    expect(digest).toEqual(await directZipOwnershipExtraFormatDigestV1())
    expect(equalDirectZipOwnershipMarkersV1(TEST_DIRECT_ZIP_MARKER, {
      ...TEST_DIRECT_ZIP_MARKER,
      bindingDigest: TEST_DIRECT_ZIP_MARKER.bindingDigest.slice(),
    })).toBe(true)
  })
})
