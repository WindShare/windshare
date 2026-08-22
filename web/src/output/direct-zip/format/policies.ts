import {
  digestDirectZipCanonicalBytes,
  directZipCanonicalFrame,
  directZipCanonicalRecord,
  directZipCanonicalU64,
  type DirectZipCanonicalBytes,
} from './canonical'
import { DIRECT_ZIP_MAXIMUM_POSITIONED_FSA_OFFSET } from './offset'
import { directZipOwnershipExtraFormatDigestV1 } from './ownership-extra'

export const ZIP_ENCODING_POLICY_V2_DOMAIN =
  'windshare/zip-encoding/v2-store-data-descriptor-owned-marker' as const
export const DIRECT_ZIP_LAYOUT_POLICY_V2_DOMAIN =
  'windshare/zip-layout/v2-paged-owned-marker' as const

export interface DirectZipFormatPolicyDigestsV2 {
  readonly ownershipExtraFormat: DirectZipCanonicalBytes
  readonly encodingPolicy: DirectZipCanonicalBytes
  readonly layoutPolicy: DirectZipCanonicalBytes
}

export async function zipEncodingPolicyCanonicalV2(): Promise<DirectZipCanonicalBytes> {
  const ownershipDigest = await directZipOwnershipExtraFormatDigestV1()
  return directZipCanonicalRecord(ZIP_ENCODING_POLICY_V2_DOMAIN, [
    directZipCanonicalFrame(ownershipDigest),
  ])
}

export async function zipEncodingPolicyDigestV2(): Promise<DirectZipCanonicalBytes> {
  return digestDirectZipCanonicalBytes(await zipEncodingPolicyCanonicalV2())
}

export async function directZipLayoutPolicyCanonicalV2(): Promise<DirectZipCanonicalBytes> {
  const encodingDigest = await zipEncodingPolicyDigestV2()
  return directZipCanonicalRecord(DIRECT_ZIP_LAYOUT_POLICY_V2_DOMAIN, [
    directZipCanonicalFrame(encodingDigest),
    directZipCanonicalFrame(directZipCanonicalU64(DIRECT_ZIP_MAXIMUM_POSITIONED_FSA_OFFSET)),
  ])
}

export async function directZipLayoutPolicyDigestV2(): Promise<DirectZipCanonicalBytes> {
  return digestDirectZipCanonicalBytes(await directZipLayoutPolicyCanonicalV2())
}

export async function directZipPolicyDigestsV2(): Promise<DirectZipFormatPolicyDigestsV2> {
  const ownershipExtraFormat = await directZipOwnershipExtraFormatDigestV1()
  const encodingPolicy = await zipEncodingPolicyDigestV2()
  const layoutPolicy = await directZipLayoutPolicyDigestV2()
  return Object.freeze({ ownershipExtraFormat, encodingPolicy, layoutPolicy })
}
