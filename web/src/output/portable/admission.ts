import { decodeBase64Url, encodeBase64Url } from '../../crypto/bytes'

const DIGEST_BYTES = 32
const U64_MAXIMUM = 0xffff_ffff_ffff_ffffn

export type PortableSealedArtifactEvidence =
  | Readonly<{
      artifactKind: 'original-file'
      preparationManifestDigest: string
    }>
  | Readonly<{
      artifactKind: 'zip-archive'
      preparationManifestDigest: string
      sealedZipLayoutDigest: string
    }>

export interface PortableArtifactAdmission {
  readonly kind: 'portable-artifact-admission'
  readonly receiveIntentDigest: string
  readonly artifactDigest: string
  readonly sealedArtifact: PortableSealedArtifactEvidence
  readonly exactArtifactBytes: bigint
}

export interface PortableArtifactAdmissionInput {
  readonly receiveIntentDigest: string
  readonly artifactDigest: string
  readonly sealedArtifact: PortableSealedArtifactEvidence
  readonly exactArtifactBytes: bigint
}

const ISSUED_PORTABLE_ADMISSIONS = new WeakSet<PortableArtifactAdmission>()

/**
 * Admission is a runtime capability, not a structural TypeScript promise. Keeping
 * issuance here prevents delivery from accepting a hand-authored proof that never
 * passed exact preparation and the bounded artifact gate.
 */
export function issuePortableArtifactAdmission(
  input: PortableArtifactAdmissionInput,
): PortableArtifactAdmission {
  const receiveIntentDigest = snapshotDigest(input.receiveIntentDigest, 'receive intent digest')
  const artifactDigest = snapshotDigest(input.artifactDigest, 'artifact digest')
  const sealedArtifact = snapshotSealedArtifact(input.sealedArtifact)
  const exactArtifactBytes = requireU64(input.exactArtifactBytes, 'portable artifact bytes')
  const admission = Object.freeze({
    kind: 'portable-artifact-admission' as const,
    receiveIntentDigest,
    artifactDigest,
    sealedArtifact,
    exactArtifactBytes,
  })
  ISSUED_PORTABLE_ADMISSIONS.add(admission)
  return admission
}

export function assertIssuedPortableArtifactAdmission(
  candidate: PortableArtifactAdmission,
  expected: Readonly<{
    receiveIntentDigest: string
    artifactDigest: string
    artifactKind: PortableSealedArtifactEvidence['artifactKind']
  }>,
): PortableArtifactAdmission {
  if (candidate === null || typeof candidate !== 'object' ||
      !ISSUED_PORTABLE_ADMISSIONS.has(candidate)) {
    throw new TypeError('Portable artifact admission was not issued by exact preparation')
  }
  if (candidate.kind !== 'portable-artifact-admission' ||
      candidate.receiveIntentDigest !== expected.receiveIntentDigest ||
      candidate.artifactDigest !== expected.artifactDigest ||
      candidate.sealedArtifact.artifactKind !== expected.artifactKind) {
    throw new TypeError('Portable artifact admission does not bind the receive intent')
  }
  snapshotDigest(candidate.receiveIntentDigest, 'receive intent digest')
  snapshotDigest(candidate.artifactDigest, 'artifact digest')
  snapshotSealedArtifact(candidate.sealedArtifact)
  requireU64(candidate.exactArtifactBytes, 'portable artifact bytes')
  return candidate
}

function snapshotSealedArtifact(
  evidence: PortableSealedArtifactEvidence,
): PortableSealedArtifactEvidence {
  if (evidence === null || typeof evidence !== 'object') {
    throw new TypeError('Portable artifact admission requires sealed preparation evidence')
  }
  const preparationManifestDigest = snapshotDigest(
    evidence.preparationManifestDigest,
    'preparation manifest digest',
  )
  if (evidence.artifactKind === 'original-file') {
    return Object.freeze({
      artifactKind: evidence.artifactKind,
      preparationManifestDigest,
    })
  }
  if (evidence.artifactKind === 'zip-archive') {
    return Object.freeze({
      artifactKind: evidence.artifactKind,
      preparationManifestDigest,
      sealedZipLayoutDigest: snapshotDigest(
        evidence.sealedZipLayoutDigest,
        'sealed ZIP layout digest',
      ),
    })
  }
  throw new TypeError('Portable artifact admission has an invalid artifact kind')
}

function snapshotDigest(value: string, label: string): string {
  const bytes = typeof value === 'string' ? decodeBase64Url(value) : undefined
  if (bytes === undefined || bytes.byteLength !== DIGEST_BYTES ||
      bytes.every(byte => byte === 0) || encodeBase64Url(bytes) !== value) {
    throw new TypeError(`${label} is invalid`)
  }
  return value
}

function requireU64(value: bigint, label: string): bigint {
  if (typeof value !== 'bigint' || value < 0n || value > U64_MAXIMUM) {
    throw new TypeError(`${label} is outside u64`)
  }
  return value
}
