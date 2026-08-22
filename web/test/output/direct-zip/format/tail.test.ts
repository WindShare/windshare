import { describe, expect, it } from 'vitest'

import {
  concatDirectZipBytes,
} from '../../../../src/output/direct-zip/format/canonical'
import {
  DIRECT_ZIP_CLASSIC_END_PROOF_BYTES,
  DIRECT_ZIP_ZIP64_END_PROOF_BYTES,
  directZipClosingTailReadBytes,
  parseDirectZipClosingTailV2,
  validateDirectZipClosingTailV2,
} from '../../../../src/output/direct-zip/format'
import { ZIP_UINT16_SENTINEL, encodeZipEndRecords } from '../../../../src/output/zip-layout/policy'

describe('DirectZip bounded closing-tail proof', () => {
  it('parses and validates an exact comment-free classic tail', () => {
    const layout = {
      entryCount: 2n,
      centralDirectoryOffset: 100n,
      centralDirectoryBytes: 50n,
      zip64EndRequired: false,
    }
    const tail = encodeZipEndRecords(layout).classicEnd
    const exactArchiveBytes = 150n + BigInt(DIRECT_ZIP_CLASSIC_END_PROOF_BYTES)

    expect(directZipClosingTailReadBytes(false)).toBe(22)
    expect(parseDirectZipClosingTailV2(tail, exactArchiveBytes)).toEqual({
      ...layout,
      exactArchiveBytes,
      proofBytes: DIRECT_ZIP_CLASSIC_END_PROOF_BYTES,
    })
    expect(() => validateDirectZipClosingTailV2(tail, {
      ...layout,
      exactArchiveBytes,
      centralDirectoryBytes: 51n,
    })).toThrow(/journal layout/u)
  })

  it('derives the 98-byte ZIP64 proof and checks the locator and classic mirror', () => {
    const layout = {
      entryCount: ZIP_UINT16_SENTINEL,
      centralDirectoryOffset: 100n,
      centralDirectoryBytes: 50n,
      zip64EndRequired: true,
    }
    const records = encodeZipEndRecords(layout)
    const tail = concatDirectZipBytes([
      records.zip64End!,
      records.zip64Locator!,
      records.classicEnd,
    ])
    const exactArchiveBytes = 150n + BigInt(DIRECT_ZIP_ZIP64_END_PROOF_BYTES)

    expect(directZipClosingTailReadBytes(true)).toBe(98)
    expect(validateDirectZipClosingTailV2(tail, { ...layout, exactArchiveBytes })).toEqual({
      ...layout,
      exactArchiveBytes,
      proofBytes: DIRECT_ZIP_ZIP64_END_PROOF_BYTES,
    })
    const badLocator = tail.slice()
    badLocator[56] = 0
    expect(() => parseDirectZipClosingTailV2(badLocator, exactArchiveBytes)).toThrow(/locator/u)
    const badClassicMirror = tail.slice()
    const badClassicView = new DataView(badClassicMirror.buffer)
    badClassicView.setUint16(76 + 8, 1, true)
    badClassicView.setUint16(76 + 10, 1, true)
    expect(() => parseDirectZipClosingTailV2(badClassicMirror, exactArchiveBytes))
      .toThrow(/disagree/u)
  })

  it('rejects comments, scans, and central directories that do not end at the tail', () => {
    const classic = encodeZipEndRecords({
      entryCount: 1n,
      centralDirectoryOffset: 0n,
      centralDirectoryBytes: 1n,
      zip64EndRequired: false,
    }).classicEnd
    const comment = classic.slice()
    new DataView(comment.buffer).setUint16(20, 1, true)

    expect(() => parseDirectZipClosingTailV2(comment, 23n)).toThrow(/classic end/u)
    expect(() => parseDirectZipClosingTailV2(classic, 24n)).toThrow(/does not end/u)
    expect(() => parseDirectZipClosingTailV2(new Uint8Array(23), 23n)).toThrow(/length/u)
  })
})
