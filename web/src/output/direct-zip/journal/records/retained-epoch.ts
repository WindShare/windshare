import { decodeBase64Url, encodeBase64Url } from '../../../../crypto/bytes'
import { chainDirectZipEpochDigestV1, requireDirectZipFsaOffset } from '../../format'
import { canonicalFrame, canonicalU8, canonicalU64, concatCanonicalBytes, type CanonicalBytes } from '../../../workspace/canonical'
import type { DirectZipRetainedEpochProofV1 } from '../model'
import { fixedFrame, snapshotFixedBase64 } from './canonical-fields'

export function snapshotRetainedEpochProof(
  input: DirectZipRetainedEpochProofV1 | undefined,
  archiveOffset: bigint,
  epochRootDigest: string,
): DirectZipRetainedEpochProofV1 | undefined {
  if (input === undefined) return undefined
  requireDirectZipFsaOffset(input.start, 'retained epoch start')
  requireDirectZipFsaOffset(input.end, 'retained epoch end')
  const predecessorRootDigest = snapshotFixedBase64(input.predecessorRootDigest, 32, 'retained epoch predecessor root', true)
  const contentDigest = snapshotFixedBase64(input.contentDigest, 32, 'retained epoch content digest', true)
  const root = snapshotFixedBase64(input.epochRootDigest, 32, 'retained epoch root', true)
  if (input.start >= input.end || input.end !== archiveOffset || root !== epochRootDigest ||
      root !== encodeBase64Url(chainDirectZipEpochDigestV1({
        start: input.start, end: input.end,
        predecessorRoot: decodeBase64Url(predecessorRootDigest)!,
        contentDigest: decodeBase64Url(contentDigest)!,
      }))) {
    throw new TypeError('Direct ZIP retained epoch proof disagrees with its terminal prefix')
  }
  return Object.freeze({ start: input.start, end: input.end, predecessorRootDigest, contentDigest, epochRootDigest: root })
}

export function canonicalRetainedEpochProof(input: DirectZipRetainedEpochProofV1 | undefined): CanonicalBytes {
  if (input === undefined) return canonicalU8(1)
  return concatCanonicalBytes([
    canonicalU8(2), canonicalFrame(canonicalU64(input.start)), canonicalFrame(canonicalU64(input.end)),
    fixedFrame(input.predecessorRootDigest, 32, 'retained epoch predecessor root', true),
    fixedFrame(input.contentDigest, 32, 'retained epoch content digest', true),
    fixedFrame(input.epochRootDigest, 32, 'retained epoch root', true),
  ])
}

export function sameRetainedEpochProof(
  left: DirectZipRetainedEpochProofV1 | undefined,
  right: DirectZipRetainedEpochProofV1 | undefined,
): boolean {
  if (left === undefined || right === undefined) return left === right
  return left.start === right.start && left.end === right.end &&
    left.predecessorRootDigest === right.predecessorRootDigest &&
    left.contentDigest === right.contentDigest && left.epochRootDigest === right.epochRootDigest
}
