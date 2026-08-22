import { describe, expect, it } from 'vitest'

import {
  DIRECT_ZIP_MAXIMUM_BOOTSTRAP_PREFIX_BYTES,
  DIRECT_ZIP_MAXIMUM_OWNERSHIP_HEADER_READ_BYTES,
  DIRECT_ZIP_MAXIMUM_POSITIONED_FSA_OFFSET,
  compareDirectZipEntriesV2,
  deriveDirectZipOwnershipHeaderReadBytes,
  directZipFsaOffset,
  directZipFsaOffsetBigInt,
  encodeDirectZipBootstrapPrefixV1,
  encodeDirectZipCentralDirectoryRecordV2,
  parseDirectZipBootstrapPrefixV1,
  parseDirectZipOwnershipCentralRecordV1,
  planDirectZipClosingLayoutV2,
  planDirectZipEntryV2,
  requireDirectZipCanonicalSuccessorV2,
  requireMatchingDirectZipOwnershipRecordsV1,
  validateDirectZipEntryPlanV2,
} from '../../../../src/output/direct-zip/format'
import { TEST_DIRECT_ZIP_MARKER, sequence } from './test-values'

describe('DirectZip layout policy V2', () => {
  it('derives and parses the complete marker-bearing bootstrap prefix', () => {
    const prefix = encodeDirectZipBootstrapPrefixV1('root', TEST_DIRECT_ZIP_MARKER)
    const headerBytes = deriveDirectZipOwnershipHeaderReadBytes(prefix.subarray(0, 30))
    const parsed = parseDirectZipBootstrapPrefixV1(prefix)

    expect(headerBytes).toBe(30 + 5 + 120)
    expect(prefix.byteLength).toBe(headerBytes + 16)
    expect(parsed).toEqual({
      rootComponent: 'root',
      rootNameBytes: new TextEncoder().encode('root/'),
      marker: { version: 1, ...TEST_DIRECT_ZIP_MARKER },
      headerBytes,
    })
  })

  it('reaches the semantic maximum read bounds only for a 255-byte root component', () => {
    const prefix = encodeDirectZipBootstrapPrefixV1('r'.repeat(255), TEST_DIRECT_ZIP_MARKER)

    expect(deriveDirectZipOwnershipHeaderReadBytes(prefix.subarray(0, 30)))
      .toBe(DIRECT_ZIP_MAXIMUM_OWNERSHIP_HEADER_READ_BYTES)
    expect(prefix.byteLength).toBe(DIRECT_ZIP_MAXIMUM_BOOTSTRAP_PREFIX_BYTES)
    const oversizedExtra = prefix.slice(0, 30)
    new DataView(oversizedExtra.buffer).setUint16(28, 121, true)
    expect(() => deriveDirectZipOwnershipHeaderReadBytes(oversizedExtra)).toThrow(/lengths/u)
  })

  it('rejects partial prefixes and unsigned descriptors instead of accepting a header alone', () => {
    const prefix = encodeDirectZipBootstrapPrefixV1('root', TEST_DIRECT_ZIP_MARKER)
    expect(() => parseDirectZipBootstrapPrefixV1(prefix.subarray(0, -1))).toThrow(/trailing|length/u)

    const unsigned = prefix.slice()
    unsigned[prefix.byteLength - 16] = 0
    expect(() => parseDirectZipBootstrapPrefixV1(unsigned)).toThrow(/descriptor/u)
  })

  it('writes the identical ownership field in the root central record', () => {
    const root = planDirectZipEntryV2({
      ordinal: 0n,
      localHeaderOffset: 0n,
      entry: { kind: 'directory', path: ['root'] },
      ownershipMarker: TEST_DIRECT_ZIP_MARKER,
    })
    const local = parseDirectZipBootstrapPrefixV1(
      encodeDirectZipBootstrapPrefixV1('root', TEST_DIRECT_ZIP_MARKER),
    )
    const central = parseDirectZipOwnershipCentralRecordV1(
      encodeDirectZipCentralDirectoryRecordV2(root, 0),
    )

    expect(() => requireMatchingDirectZipOwnershipRecordsV1(local, central)).not.toThrow()
    const foreignCentral = {
      ...central,
      marker: { ...central.marker, bindingDigest: sequence(0x61, 32) },
    }
    expect(() => requireMatchingDirectZipOwnershipRecordsV1(local, foreignCentral))
      .toThrow(/disagree/u)
  })

  it('enforces ordinal, offset, and unsigned UTF-8 order independently of worker completion', () => {
    const root = planDirectZipEntryV2({
      ordinal: 0n,
      localHeaderOffset: 0n,
      entry: { kind: 'directory', path: ['root'] },
      ownershipMarker: TEST_DIRECT_ZIP_MARKER,
    })
    const child = planDirectZipEntryV2({
      ordinal: 1n,
      localHeaderOffset: root.entryStreamBytes,
      entry: { kind: 'file', path: ['root', 'a'], exactSize: 3n },
    })

    expect(() => requireDirectZipCanonicalSuccessorV2(root, child)).not.toThrow()
    expect(validateDirectZipEntryPlanV2(root)).toEqual(root)
    expect(compareDirectZipEntriesV2(
      { kind: 'file', path: ['root', 'same'], exactSize: 0n },
      { kind: 'directory', path: ['root', 'same'] },
    )).toBeGreaterThan(0)
    expect(() => planDirectZipEntryV2({
      ordinal: 1n,
      localHeaderOffset: root.entryStreamBytes,
      entry: { kind: 'file', path: ['root', 'a'], exactSize: 0n },
      ownershipMarker: TEST_DIRECT_ZIP_MARKER,
    })).toThrow(/entry zero/u)
  })

  it('rejects numeric offsets outside the positioned FSA safe-integer boundary', () => {
    expect(directZipFsaOffset(DIRECT_ZIP_MAXIMUM_POSITIONED_FSA_OFFSET)).toBe(Number.MAX_SAFE_INTEGER)
    expect(directZipFsaOffsetBigInt(Number.MAX_SAFE_INTEGER)).toBe(
      DIRECT_ZIP_MAXIMUM_POSITIONED_FSA_OFFSET,
    )
    expect(() => directZipFsaOffset(DIRECT_ZIP_MAXIMUM_POSITIONED_FSA_OFFSET + 1n))
      .toThrow(/File System Access/u)
    expect(() => planDirectZipEntryV2({
      ordinal: 1n,
      localHeaderOffset: DIRECT_ZIP_MAXIMUM_POSITIONED_FSA_OFFSET,
      entry: { kind: 'file', path: ['root', 'a'], exactSize: 0n },
    })).toThrow(/member end/u)
  })

  it('derives classic or ZIP64 closing bytes from exact accumulated layout values', () => {
    expect(planDirectZipClosingLayoutV2({
      entryCount: 1n,
      centralDirectoryOffset: 184n,
      centralDirectoryBytes: 184n,
    })).toEqual({
      entryCount: 1n,
      centralDirectoryOffset: 184n,
      centralDirectoryBytes: 184n,
      zip64EndRequired: false,
      closingTailBytes: 22n,
      exactArchiveBytes: 390n,
    })
    expect(planDirectZipClosingLayoutV2({
      entryCount: 0xffffn,
      centralDirectoryOffset: 10n,
      centralDirectoryBytes: 20n,
    })).toMatchObject({ zip64EndRequired: true, closingTailBytes: 98n })
    expect(() => planDirectZipClosingLayoutV2({
      entryCount: 0n,
      centralDirectoryOffset: 0n,
      centralDirectoryBytes: 0n,
    })).toThrow(/ownership root/u)
  })
})
