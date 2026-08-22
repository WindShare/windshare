import { describe, expect, it } from 'vitest'

import { bytesToHex } from '../../../../src/crypto/bytes'
import { sha256 } from '../../../../src/crypto/digest'
import {
  DIRECT_ZIP_LAYOUT_POLICY_V2_DOMAIN,
  DIRECT_ZIP_OWNERSHIP_EXTRA_FORMAT_DOMAIN,
  DirectZipSha256Accumulator,
  ZIP_ENCODING_POLICY_V2_DOMAIN,
  chainDirectZipEpochDigestV1,
  digestDirectZipArchiveBytes,
  directZipEpochDigestCanonicalV1,
  directZipEpochGenesisRoot,
  directZipLayoutPolicyDigestV2,
  directZipOwnershipExtraFormatDigestV1,
  directZipPolicyDigestsV2,
  zipEncodingPolicyDigestV2,
} from '../../../../src/output/direct-zip/format'

describe('DirectZip digest primitives', () => {
  it('streams SHA-256 across block and padding boundaries without retaining the epoch', async () => {
    const input = Uint8Array.from({ length: 257 }, (_, index) => index & 0xff)
    const accumulator = new DirectZipSha256Accumulator()
    for (const [start, end] of [[0, 1], [1, 63], [63, 64], [64, 129], [129, 257]]) {
      accumulator.update(input.subarray(start, end))
    }

    expect(accumulator.byteLength).toBe(257n)
    expect(accumulator.digest()).toEqual(await sha256(input))
    expect(digestDirectZipArchiveBytes(new TextEncoder().encode('abc'))).toEqual(
      await sha256(new TextEncoder().encode('abc')),
    )
    expect(bytesToHex(digestDirectZipArchiveBytes(new Uint8Array(0)))).toBe(
      'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855',
    )
  })

  it('domain-separates epoch content from its predecessor and exact byte range', () => {
    const contentDigest = digestDirectZipArchiveBytes(Uint8Array.of(1, 2, 3))
    const base = {
      predecessorRoot: directZipEpochGenesisRoot(),
      start: 0n,
      end: 3n,
      contentDigest,
    }

    expect(chainDirectZipEpochDigestV1(base)).toEqual(chainDirectZipEpochDigestV1(base))
    expect(chainDirectZipEpochDigestV1({ ...base, start: 1n })).not.toEqual(
      chainDirectZipEpochDigestV1(base),
    )
    expect(new TextDecoder().decode(directZipEpochDigestCanonicalV1(base).subarray(0, 40)))
      .toContain('windshare/direct-zip-epoch-digest/v1\0')
    expect(() => chainDirectZipEpochDigestV1({ ...base, start: 4n })).toThrow(/inverted/u)
  })

  it('produces the frozen ownership, encoding, and layout policy digest chain', async () => {
    const ownership = await directZipOwnershipExtraFormatDigestV1()
    const encoding = await zipEncodingPolicyDigestV2()
    const layout = await directZipLayoutPolicyDigestV2()
    const all = await directZipPolicyDigestsV2()

    expect(all).toEqual({
      ownershipExtraFormat: ownership,
      encodingPolicy: encoding,
      layoutPolicy: layout,
    })
    expect(DIRECT_ZIP_OWNERSHIP_EXTRA_FORMAT_DOMAIN).toBe(
      'windshare/direct-zip-ownership-extra/v1',
    )
    expect(ZIP_ENCODING_POLICY_V2_DOMAIN).toBe(
      'windshare/zip-encoding/v2-store-data-descriptor-owned-marker',
    )
    expect(DIRECT_ZIP_LAYOUT_POLICY_V2_DOMAIN).toBe(
      'windshare/zip-layout/v2-paged-owned-marker',
    )
    expect(bytesToHex(ownership)).toBe(
      '86715d2bfc5e0de6089ecca1bf98b861d539ed451c59e0065182d36a459a650c',
    )
    expect(bytesToHex(encoding)).toBe(
      '2d6363da388be94ded4d935a2f2e6dc631650d26a853386b8d3d09e38af476b7',
    )
    expect(bytesToHex(layout)).toBe(
      '55257e0f54f0873871b82658831d7e644136ea816efa61d7038a1610b3a0187e',
    )
  })
})
