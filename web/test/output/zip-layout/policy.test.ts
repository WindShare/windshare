import { describe, expect, it } from 'vitest'

import {
  ZIP_UINT16_SENTINEL,
  ZIP_UINT32_SENTINEL,
  ZIP_UINT64_MAXIMUM,
  ZIP_CRC32_INITIAL_ACCUMULATOR,
  ZipCrc32,
  checkedZipAdd,
  checkedZipMultiply,
  encodeZipCentralDirectoryRecord,
  encodeZipCentralDirectoryRecordWithTrailingExtraFields,
  encodeZipDataDescriptor,
  encodeZipEndRecords,
  encodeZipLocalHeader,
  encodeZipLocalHeaderWithTrailingExtraFields,
  normalizeZipEntry,
  planZipEntry,
  requiresZip64End,
} from '../../../src/output/zip-layout/policy'

describe('ZipEncodingPolicyV1', () => {
  it('uses ZIP32 with descriptor-authoritative local placeholders when values fit', () => {
    const plan = planZipEntry(normalizeZipEntry({
      kind: 'file',
      path: ['root', 'a'],
      exactSize: ZIP_UINT32_SENTINEL - 1n,
    }), 0n)
    const local = encodeZipLocalHeader(plan)
    const descriptor = encodeZipDataDescriptor(plan, 0x1234_5678)
    const central = encodeZipCentralDirectoryRecord(plan, 0x1234_5678)
    const localView = new DataView(local.buffer)
    const centralView = new DataView(central.buffer)

    expect(plan.zip64Size).toBe(false)
    expect(plan.zip64Offset).toBe(false)
    expect(plan.versionNeeded).toBe(20)
    expect(plan.localExtraBytes).toBe(0n)
    expect(plan.descriptorBytes).toBe(16n)
    expect(localView.getUint32(18, true)).toBe(0)
    expect(localView.getUint32(22, true)).toBe(0)
    expect(centralView.getUint32(20, true)).toBe(0xffff_fffe)
    expect(centralView.getUint32(24, true)).toBe(0xffff_fffe)
    expect(descriptor.byteLength).toBe(16)
  })

  it('introduces only the ZIP64 size fields at the exact size threshold', () => {
    const plan = planZipEntry(normalizeZipEntry({
      kind: 'file',
      path: ['root', 'large'],
      exactSize: ZIP_UINT32_SENTINEL,
    }), 0n)
    const local = encodeZipLocalHeader(plan)
    const descriptor = encodeZipDataDescriptor(plan, 7)
    const central = encodeZipCentralDirectoryRecord(plan, 7)
    const localView = new DataView(local.buffer)
    const centralView = new DataView(central.buffer)

    expect(plan.zip64Size).toBe(true)
    expect(plan.zip64Offset).toBe(false)
    expect(plan.versionNeeded).toBe(45)
    expect(plan.localExtraBytes).toBe(20n)
    expect(plan.centralZip64ValueCount).toBe(2)
    expect(plan.centralExtraBytes).toBe(20n)
    expect(localView.getUint32(18, true)).toBe(0xffff_ffff)
    expect(localView.getBigUint64(30 + plan.nameBytes.length + 4, true))
      .toBe(ZIP_UINT32_SENTINEL)
    expect(descriptor.byteLength).toBe(24)
    expect(centralView.getUint32(42, true)).toBe(0)
  })

  it('introduces only the ZIP64 offset field at the exact offset threshold', () => {
    const plan = planZipEntry(normalizeZipEntry({
      kind: 'file',
      path: ['root', 'empty'],
      exactSize: 0n,
    }), ZIP_UINT32_SENTINEL)
    const central = encodeZipCentralDirectoryRecord(plan, 0)
    const view = new DataView(central.buffer)
    const extraOffset = 46 + plan.nameBytes.length

    expect(plan.zip64Size).toBe(false)
    expect(plan.zip64Offset).toBe(true)
    expect(plan.centralZip64ValueCount).toBe(1)
    expect(plan.centralExtraBytes).toBe(12n)
    expect(view.getUint32(20, true)).toBe(0)
    expect(view.getUint32(42, true)).toBe(0xffff_ffff)
    expect(view.getBigUint64(extraOffset + 4, true)).toBe(ZIP_UINT32_SENTINEL)
  })

  it('uses UTC, clamps the DOS range, and truncates to two-second precision', () => {
    const early = normalizeZipEntry({
      kind: 'directory',
      path: ['root'],
      modifiedTimeMilliseconds: -1n,
    })
    const precise = normalizeZipEntry({
      kind: 'directory',
      path: ['root'],
      modifiedTimeMilliseconds: BigInt(Date.UTC(2024, 5, 7, 8, 9, 11, 999)),
    })
    const late = normalizeZipEntry({
      kind: 'directory',
      path: ['root'],
      modifiedTimeMilliseconds: ZIP_UINT64_MAXIMUM,
    })

    expect(early).toMatchObject({ dosDate: 0x0021, dosTime: 0 })
    expect(precise.dosTime).toBe((8 << 11) | (9 << 5) | 5)
    expect(precise.dosDate).toBe(((2024 - 1980) << 9) | (6 << 5) | 7)
    expect(late.dosDate).toBe(0xff9f)
    expect(late.dosTime).toBe(0xbf7d)
  })

  it('uses sentinels only for global values carried by ZIP64 end records', () => {
    expect(requiresZip64End({
      entryCount: ZIP_UINT16_SENTINEL - 1n,
      centralDirectoryOffset: ZIP_UINT32_SENTINEL - 1n,
      centralDirectoryBytes: ZIP_UINT32_SENTINEL - 1n,
    })).toBe(false)
    expect(requiresZip64End({
      entryCount: ZIP_UINT16_SENTINEL,
      centralDirectoryOffset: 12n,
      centralDirectoryBytes: 34n,
    })).toBe(true)
    const records = encodeZipEndRecords({
      entryCount: ZIP_UINT16_SENTINEL,
      centralDirectoryOffset: 12n,
      centralDirectoryBytes: 34n,
      zip64EndRequired: true,
    })
    const classic = new DataView(records.classicEnd.buffer)
    expect(classic.getUint16(8, true)).toBe(0xffff)
    expect(classic.getUint32(12, true)).toBe(34)
    expect(classic.getUint32(16, true)).toBe(12)
    expect(records.zip64End?.byteLength).toBe(56)
    expect(records.zip64Locator?.byteLength).toBe(20)
  })

  it('checks every uint64 addition and multiplication', () => {
    expect(checkedZipAdd(ZIP_UINT64_MAXIMUM - 1n, 1n)).toBe(ZIP_UINT64_MAXIMUM)
    expect(() => checkedZipAdd(ZIP_UINT64_MAXIMUM, 1n)).toThrow(/addition overflow/u)
    expect(checkedZipMultiply(0xffff_ffffn, 2n)).toBe(0x1_ffff_fffen)
    expect(() => checkedZipMultiply(ZIP_UINT64_MAXIMUM, 2n)).toThrow(/multiplication overflow/u)
  })

  it('uses the frozen IEEE CRC-32 polynomial', () => {
    const crc = new ZipCrc32()
    crc.update(new TextEncoder().encode('1234'))
    const resumed = new ZipCrc32(crc.snapshot())
    resumed.update(new TextEncoder().encode('56789'))

    expect(new ZipCrc32().snapshot()).toBe(ZIP_CRC32_INITIAL_ACCUMULATOR)
    expect(resumed.digest()).toBe(0xcbf4_3926)
    expect(() => new ZipCrc32(-1)).toThrow(/accumulator/u)
  })

  it('appends pre-encoded extra fields without changing the V1 record plan', () => {
    const plan = planZipEntry(normalizeZipEntry({
      kind: 'directory',
      path: ['root'],
    }), 0n)
    const extra = Uint8Array.of(0x34, 0x12, 0x02, 0x00, 0xab, 0xcd)
    const v1Local = encodeZipLocalHeader(plan)
    const local = encodeZipLocalHeaderWithTrailingExtraFields(plan, extra)
    const central = encodeZipCentralDirectoryRecordWithTrailingExtraFields(plan, 0, extra)

    expect(new DataView(v1Local.buffer).getUint16(28, true)).toBe(0)
    expect(new DataView(local.buffer).getUint16(28, true)).toBe(extra.byteLength)
    expect(local.slice(-extra.byteLength)).toEqual(extra)
    expect(new DataView(central.buffer).getUint16(30, true)).toBe(extra.byteLength)
    expect(central.slice(-extra.byteLength)).toEqual(extra)
  })
})
