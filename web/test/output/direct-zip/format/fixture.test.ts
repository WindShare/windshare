import { readFile } from 'node:fs/promises'

import { describe, expect, it } from 'vitest'

import { bytesToHex } from '../../../../src/crypto/bytes'
import {
  digestDirectZipArchiveBytes,
  parseDirectZipBootstrapPrefixV1,
  parseDirectZipClosingTailV2,
  parseDirectZipOwnershipCentralRecordV1,
  requireMatchingDirectZipOwnershipRecordsV1,
} from '../../../../src/output/direct-zip/format'
import { encodeDirectZipMarkerFixture } from './test-values'

export const DIRECT_ZIP_FIXTURE_SHA256 =
  '6fb91a264510bbd4edde826a8f0b0c390df32e1152c16fd46907491e98cdbb9a'

describe('DirectZip marker fixture', () => {
  it('matches the deterministic cross-language fixture and all bounded proofs', async () => {
    const generated = encodeDirectZipMarkerFixture()
    const fixture = new Uint8Array(await readFile(new URL(
      '../../../../../testdata/direct-zip/v1/ownership-marker-v1.zip',
      import.meta.url,
    )))
    const local = parseDirectZipBootstrapPrefixV1(generated.subarray(0, 184))
    const centralOffset = 184
    const centralBytes = 184
    const central = parseDirectZipOwnershipCentralRecordV1(
      generated.subarray(centralOffset, centralOffset + centralBytes),
    )
    const tail = generated.subarray(-22)

    expect(fixture).toEqual(generated)
    expect(bytesToHex(digestDirectZipArchiveBytes(generated))).toBe(DIRECT_ZIP_FIXTURE_SHA256)
    expect(() => requireMatchingDirectZipOwnershipRecordsV1(local, central)).not.toThrow()
    expect(parseDirectZipClosingTailV2(tail, BigInt(generated.byteLength))).toMatchObject({
      entryCount: 1n,
      centralDirectoryOffset: BigInt(centralOffset),
      centralDirectoryBytes: BigInt(centralBytes),
    })
  })
})
