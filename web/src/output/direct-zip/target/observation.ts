import {
  DIRECT_ZIP_LOCAL_HEADER_FIXED_BYTES,
  DIRECT_ZIP_SIGNED_ZIP32_DESCRIPTOR_BYTES,
  DirectZipSha256Accumulator,
  chainDirectZipEpochDigestV1,
  deriveDirectZipOwnershipHeaderReadBytes,
  directZipEpochGenesisRoot,
  equalDirectZipOwnershipMarkersV1,
  parseDirectZipBootstrapPrefixV1,
  type DirectZipOwnershipMarkerInputV1,
} from '../format'
import { equalDirectZipBytes } from '../format/canonical'
import type {
  DirectZipCommittedEpochProofV1,
  DirectZipEvidenceComparison,
  DirectZipTargetObservationV1,
} from './model'
import type { DirectZipFileSnapshotPort } from './ports'

export const DIRECT_ZIP_RANGE_HASH_CHUNK_BYTES = 256 * 1024

export async function observeDirectZipTarget(
  snapshot: DirectZipFileSnapshotPort,
  expected: Readonly<{
    readonly resultRootComponent: string
    readonly marker: DirectZipOwnershipMarkerInputV1
    readonly parentLocator: DirectZipEvidenceComparison
    readonly fileLocator: DirectZipEvidenceComparison
  }>,
): Promise<DirectZipTargetObservationV1> {
  const marker = await observeOwnershipMarker(snapshot, expected)
  return Object.freeze({
    size: snapshot.size,
    lastModified: snapshot.lastModified,
    marker,
    parentLocator: expected.parentLocator,
    fileLocator: expected.fileLocator,
  })
}

export async function verifyDirectZipEpochProof(
  snapshot: DirectZipFileSnapshotPort,
  proof: DirectZipCommittedEpochProofV1,
): Promise<boolean> {
  if (proof.start < 0n || proof.end <= proof.start || proof.end > snapshot.size) return false
  const digest = new DirectZipSha256Accumulator()
  let offset = proof.start
  while (offset < proof.end) {
    const end = minBigInt(proof.end, offset + BigInt(DIRECT_ZIP_RANGE_HASH_CHUNK_BYTES))
    const bytes = await snapshot.read(offset, end)
    if (bytes.byteLength !== Number(end - offset)) return false
    digest.update(bytes)
    offset = end
  }
  const epochRoot = chainDirectZipEpochDigestV1({
    predecessorRoot: proof.predecessorRoot,
    start: proof.start,
    end: proof.end,
    contentDigest: digest.digest(),
  })
  return equalDirectZipBytes(epochRoot, proof.epochRoot)
}

export async function verifyDirectZipCommittedEpochChain(
  snapshot: DirectZipFileSnapshotPort,
  proofs: readonly DirectZipCommittedEpochProofV1[],
  committedLength: bigint,
): Promise<boolean> {
  if (proofs.length === 0 || committedLength <= 0n) return false
  let expectedStart = 0n
  let predecessorRoot = directZipEpochGenesisRoot()
  for (const proof of proofs) {
    if (proof.start !== expectedStart || !equalDirectZipBytes(proof.predecessorRoot, predecessorRoot) ||
        !await verifyDirectZipEpochProof(snapshot, proof)) {
      return false
    }
    expectedStart = proof.end
    predecessorRoot = proof.epochRoot
  }
  return expectedStart === committedLength
}

async function observeOwnershipMarker(
  snapshot: DirectZipFileSnapshotPort,
  expected: Readonly<{
    readonly resultRootComponent: string
    readonly marker: DirectZipOwnershipMarkerInputV1
  }>,
): Promise<DirectZipTargetObservationV1['marker']> {
  if (snapshot.size < BigInt(DIRECT_ZIP_LOCAL_HEADER_FIXED_BYTES)) {
    return Object.freeze({ kind: 'partial' })
  }
  const fixed = await snapshot.read(0n, BigInt(DIRECT_ZIP_LOCAL_HEADER_FIXED_BYTES))
  if (fixed.byteLength !== DIRECT_ZIP_LOCAL_HEADER_FIXED_BYTES) {
    return Object.freeze({ kind: 'partial' })
  }
  let prefixBytes: number
  try {
    prefixBytes = deriveDirectZipOwnershipHeaderReadBytes(fixed) +
      DIRECT_ZIP_SIGNED_ZIP32_DESCRIPTOR_BYTES
  } catch {
    return Object.freeze({ kind: 'malformed' })
  }
  if (snapshot.size < BigInt(prefixBytes)) return Object.freeze({ kind: 'partial' })
  const prefix = await snapshot.read(0n, BigInt(prefixBytes))
  if (prefix.byteLength !== prefixBytes) return Object.freeze({ kind: 'partial' })
  try {
    const parsed = parseDirectZipBootstrapPrefixV1(prefix)
    if (parsed.rootComponent !== expected.resultRootComponent ||
        !equalDirectZipOwnershipMarkersV1(parsed.marker, expected.marker)) {
      return Object.freeze({ kind: 'foreign' })
    }
    return Object.freeze({ kind: 'matching', prefixLength: BigInt(prefixBytes) })
  } catch {
    return Object.freeze({ kind: 'malformed' })
  }
}

function minBigInt(left: bigint, right: bigint): bigint {
  return left < right ? left : right
}
