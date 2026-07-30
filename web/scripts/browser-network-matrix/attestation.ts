import { isIP } from 'node:net'
import { createHash, createPublicKey } from 'node:crypto'

import {
  NETWORK_MATRIX_FAILED_FAILURE_CODES,
  NETWORK_MATRIX_INVALID_FAILURE_CODES,
  NETWORK_MATRIX_PREREQUISITE_OUTCOMES,
  NETWORK_MATRIX_PROOF_KINDS,
  NETWORK_RUNTIME_ATTESTATION_SCHEMA,
  NETWORK_MATRIX_UNAVAILABLE_FAILURE_CODES,
  type NetworkMatrixAuthorityKind,
  type NetworkMatrixPrerequisiteOutcome,
  type NetworkMatrixProfileId,
  type NetworkMatrixProofKind,
} from './vocabulary.ts'
import type { NetworkMatrixManifest, NetworkMatrixProfileReference } from './manifest.ts'
import type { NetworkMatrixAuthorityRequirement } from './profile.ts'
import {
  networkMatrixError,
  parseNetworkMatrixJsonText,
  requireCanonicalEncoding,
  requireEnum,
  requireExactKeys,
  requireLiteral,
  requireRecord,
  requireRunId,
  requireSha256,
} from './contract-support.ts'
import {
  authenticateSignedExternalFixtureAttestation,
  EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
  parseSignedExternalFixtureAttestation,
  type SignedExternalFixtureAttestation,
} from './linux-topology/external-fixture-attestation.ts'

export interface NetworkRuntimeAttestationContext {
  readonly manifest: NetworkMatrixManifest
  readonly manifestSha256: string
  readonly runId: string
}

export interface ExternalFixtureTrustProof {
  readonly controllerOrigin: string
  readonly tlsCertificateSha256: string
  readonly tlsCertificateAuthoritySha256: string
  readonly attestationPublicKeySpki: string
  readonly attestationPublicKeySha256: string
}

export interface NetworkRuntimeProof {
  readonly proofKind: NetworkMatrixProofKind
  readonly externalFixtureTrust: ExternalFixtureTrustProof
}

export interface NetworkPrerequisiteFailure {
  readonly failureKind: Exclude<NetworkMatrixPrerequisiteOutcome, 'satisfied'>
  readonly failureCode: string
}

export interface NetworkRuntimeAttestation {
  readonly schemaVersion: typeof NETWORK_RUNTIME_ATTESTATION_SCHEMA
  readonly runId: string
  readonly manifestSha256: string
  readonly profileId: NetworkMatrixProfileId
  readonly profileSha256: string
  readonly authorityId: NetworkMatrixProfileReference['authorityId']
  readonly authorityKind: NetworkMatrixAuthorityKind
  readonly prerequisiteOutcome: NetworkMatrixPrerequisiteOutcome
  readonly proof: NetworkRuntimeProof | null
  readonly failure: NetworkPrerequisiteFailure | null
}

export interface PinnedExternalFixtureAttestation {
  readonly attestationPublicKeySpki: string
  readonly attestationPublicKeyPem: string
  readonly signedAttestation: SignedExternalFixtureAttestation
  readonly authenticated: ReturnType<typeof authenticateSignedExternalFixtureAttestation>
}

/** Authenticates sample-local proof without trusting any key or digest embedded by the sample. */
export function authenticatePinnedExternalFixtureAttestation(
  signedValue: unknown,
  attestationPublicKeySpki: unknown,
  profile: NetworkMatrixProfileReference,
  profileAuthority: NetworkMatrixAuthorityRequirement,
  manifest: NetworkMatrixManifest,
): PinnedExternalFixtureAttestation {
  const authority = manifest.authorities.find(
    (candidate) => candidate.authorityId === profile.authorityId,
  )
  if (
    authority === undefined || profileAuthority.authorityId !== profile.authorityId ||
    profileAuthority.authorityKind !== profile.authorityKind ||
    profileAuthority.attestationPublicKeySha256 !== authority.attestationPublicKeySha256
  ) networkMatrixError('profile and manifest do not agree on the external fixture trust anchor')
  const key = parseAttestationPublicKeySpki(attestationPublicKeySpki, profile, manifest)
  let signedAttestation: SignedExternalFixtureAttestation
  let authenticated: ReturnType<typeof authenticateSignedExternalFixtureAttestation>
  try {
    signedAttestation = parseSignedExternalFixtureAttestation(
      signedValue,
      EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
    )
    authenticated = authenticateSignedExternalFixtureAttestation(
      signedAttestation,
      key.publicKeyPem,
    )
  } catch {
    networkMatrixError('external fixture sample signature is invalid')
  }
  return Object.freeze({
    attestationPublicKeySpki: key.encoded,
    attestationPublicKeyPem: key.publicKeyPem,
    signedAttestation,
    authenticated,
  })
}

export function parseNetworkRuntimeAttestation(
  value: unknown,
  context: NetworkRuntimeAttestationContext,
): NetworkRuntimeAttestation {
  const record = requireRecord(value, 'network runtime attestation')
  requireExactKeys(record, [
    'schemaVersion',
    'runId',
    'manifestSha256',
    'profileId',
    'profileSha256',
    'authorityId',
    'authorityKind',
    'prerequisiteOutcome',
    'proof',
    'failure',
  ], 'network runtime attestation')
  const profile = profileReference(context.manifest, record.profileId)
  const prerequisiteOutcome = requireEnum(
    record.prerequisiteOutcome,
    NETWORK_MATRIX_PREREQUISITE_OUTCOMES,
    'network prerequisite outcome',
  )
  const proof = prerequisiteOutcome === 'satisfied'
    ? parseProof(record.proof, profile, context)
    : requireNull(record.proof, 'unsatisfied prerequisite proof')
  const failure = prerequisiteOutcome === 'satisfied'
    ? requireNull(record.failure, 'satisfied prerequisite failure')
    : parseFailure(record.failure, prerequisiteOutcome)
  return Object.freeze({
    schemaVersion: requireLiteral(
      record.schemaVersion,
      NETWORK_RUNTIME_ATTESTATION_SCHEMA,
      'network runtime attestation schema',
    ),
    runId: requireLiteral(
      requireRunId(record.runId, 'network runtime attestation run ID'),
      requireRunId(context.runId, 'expected network matrix run ID'),
      'network runtime attestation run ID',
    ),
    manifestSha256: requireLiteral(
      requireSha256(record.manifestSha256, 'network runtime attestation manifest digest'),
      requireSha256(context.manifestSha256, 'expected network matrix manifest digest'),
      'network runtime attestation manifest digest',
    ),
    profileId: profile.profileId,
    profileSha256: requireLiteral(
      requireSha256(record.profileSha256, 'network runtime attestation profile digest'),
      profile.profileSha256,
      'network runtime attestation profile digest',
    ),
    authorityId: requireLiteral(
      record.authorityId,
      profile.authorityId,
      'network runtime attestation authority ID',
    ),
    authorityKind: requireLiteral(
      record.authorityKind,
      profile.authorityKind,
      'network runtime attestation authority kind',
    ),
    prerequisiteOutcome,
    proof,
    failure,
  })
}

export function parseNetworkRuntimeAttestationJson(
  encoded: string,
  context: NetworkRuntimeAttestationContext,
): NetworkRuntimeAttestation {
  return requireCanonicalEncoding(
    encoded,
    parseNetworkRuntimeAttestation(
      parseNetworkMatrixJsonText(encoded, 'network runtime attestation'),
      context,
    ),
    'network runtime attestation',
  )
}

export function canonicalNetworkRuntimeAttestationJson(
  attestation: NetworkRuntimeAttestation,
  context: NetworkRuntimeAttestationContext,
): string {
  return `${JSON.stringify(parseNetworkRuntimeAttestation(attestation, context))}\n`
}

function parseProof(
  value: unknown,
  profile: NetworkMatrixProfileReference,
  context: NetworkRuntimeAttestationContext,
): NetworkRuntimeProof {
  const proof = requireRecord(value, 'satisfied network authority proof')
  requireExactKeys(
    proof,
    ['proofKind', 'externalFixtureTrust'],
    'satisfied network authority proof',
  )
  const expectedKind = proofKindFor(profile.authorityKind)
  const proofKind = requireLiteral(proof.proofKind, expectedKind, 'network authority proof kind')
  return Object.freeze({
    proofKind,
    externalFixtureTrust: parseExternalFixtureTrustProof(
      proof.externalFixtureTrust,
      profile,
      context,
    ),
  })
}

function parseExternalFixtureTrustProof(
  value: unknown,
  profile: NetworkMatrixProfileReference,
  context: NetworkRuntimeAttestationContext,
): ExternalFixtureTrustProof {
  const proof = requireRecord(value, 'external fixture trust proof')
  requireExactKeys(proof, [
    'controllerOrigin',
    'tlsCertificateSha256',
    'tlsCertificateAuthoritySha256',
    'attestationPublicKeySpki',
    'attestationPublicKeySha256',
  ], 'external fixture trust proof')
  const attestationPublicKeySpki = parseAttestationPublicKeySpki(
    proof.attestationPublicKeySpki,
    profile,
    context.manifest,
  )
  return Object.freeze({
    controllerOrigin: requireCanonicalHttpsOrigin(proof.controllerOrigin),
    tlsCertificateSha256: requireSha256(
      proof.tlsCertificateSha256,
      'external fixture TLS certificate digest',
    ),
    tlsCertificateAuthoritySha256: requireSha256(
      proof.tlsCertificateAuthoritySha256,
      'external fixture TLS certificate authority digest',
    ),
    attestationPublicKeySpki: attestationPublicKeySpki.encoded,
    attestationPublicKeySha256: requireLiteral(
      requireSha256(
        proof.attestationPublicKeySha256,
        'external fixture attestation public-key digest',
      ),
      attestationPublicKeySpki.sha256,
      'external fixture attestation public-key digest',
    ),
  })
}

function parseAttestationPublicKeySpki(
  value: unknown,
  profile: NetworkMatrixProfileReference,
  manifest: NetworkMatrixManifest,
): { readonly encoded: string; readonly publicKeyPem: string; readonly sha256: string } {
  if (typeof value !== 'string' || !/^[A-Za-z0-9_-]{32,256}$/u.test(value)) {
    networkMatrixError('external fixture attestation public key is not canonical SPKI')
  }
  const bytes = Buffer.from(value, 'base64url')
  if (bytes.toString('base64url') !== value) {
    networkMatrixError('external fixture attestation public key SPKI is not canonical base64url')
  }
  let publicKey: ReturnType<typeof createPublicKey>
  try {
    publicKey = createPublicKey({ key: bytes, format: 'der', type: 'spki' })
  } catch {
    networkMatrixError('external fixture attestation public key SPKI is invalid')
  }
  const canonical = Buffer.from(publicKey.export({ type: 'spki', format: 'der' }))
  const sha256 = createHash('sha256').update(canonical).digest('hex')
  const authority = manifest.authorities.find(
    (candidate) => candidate.authorityId === profile.authorityId,
  )
  if (
    publicKey.asymmetricKeyType !== 'ed25519' || !canonical.equals(bytes) ||
    authority === undefined ||
    sha256 !== authority.attestationPublicKeySha256
  ) networkMatrixError('external fixture attestation key differs from the manifest trust anchor')
  return Object.freeze({
    encoded: value,
    publicKeyPem: publicKey.export({ type: 'spki', format: 'pem' }).toString(),
    sha256,
  })
}

function parseFailure(
  value: unknown,
  outcome: Exclude<NetworkMatrixPrerequisiteOutcome, 'satisfied'>,
): NetworkPrerequisiteFailure {
  const failure = requireRecord(value, 'network prerequisite failure')
  requireExactKeys(failure, ['failureKind', 'failureCode'], 'network prerequisite failure')
  const codes = failureCodesFor(outcome)
  return Object.freeze({
    failureKind: requireLiteral(failure.failureKind, outcome, 'network prerequisite failure kind'),
    failureCode: requireEnum(failure.failureCode, codes, 'network prerequisite failure code'),
  })
}

function failureCodesFor(
  outcome: Exclude<NetworkMatrixPrerequisiteOutcome, 'satisfied'>,
): readonly string[] {
  if (outcome === 'unavailable') return NETWORK_MATRIX_UNAVAILABLE_FAILURE_CODES
  if (outcome === 'invalid') return NETWORK_MATRIX_INVALID_FAILURE_CODES
  return NETWORK_MATRIX_FAILED_FAILURE_CODES
}

function proofKindFor(authorityKind: NetworkMatrixAuthorityKind): NetworkMatrixProofKind {
  requireLiteral(authorityKind, 'external-fixture', 'network matrix authority kind')
  return NETWORK_MATRIX_PROOF_KINDS[0]
}

function requireCanonicalHttpsOrigin(value: unknown): string {
  if (typeof value !== 'string') networkMatrixError('external fixture controller origin is invalid')
  let endpoint: URL
  try {
    endpoint = new URL(value)
  } catch {
    networkMatrixError('external fixture controller origin is invalid')
  }
  const hostname = endpoint.hostname.replace(/^\[|\]$/gu, '')
  if (
    endpoint.protocol !== 'https:' || endpoint.username !== '' || endpoint.password !== '' ||
    endpoint.pathname !== '/' || endpoint.search !== '' || endpoint.hash !== '' ||
    value !== `${endpoint.origin}/` || isIP(hostname) === 6
  ) networkMatrixError('external fixture controller origin is invalid')
  return value
}

function profileReference(
  manifest: NetworkMatrixManifest,
  profileId: unknown,
): NetworkMatrixProfileReference {
  const profile = manifest.profiles.find((candidate) => candidate.profileId === profileId)
  if (profile === undefined) networkMatrixError('network runtime attestation profile is not in the manifest')
  return profile
}

function requireNull(value: unknown, label: string): null {
  if (value !== null) networkMatrixError(`${label} must be null`)
  return null
}
