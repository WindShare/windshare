import {
  NETWORK_MANUAL_SUPPLEMENT_ID,
  NETWORK_MANUAL_SUPPLEMENT_PROFILE_SCHEMA,
  NETWORK_MANUAL_SUPPLEMENT_REPORTING_SEMANTICS,
} from '../vocabulary.ts'
import {
  parseNetworkCandidatePolicy,
  type NetworkMatrixCandidatePolicy,
} from '../profile.ts'
import {
  parseNetworkMatrixJsonText,
  requireCanonicalEncoding,
  requireExactKeys,
  requireLiteral,
  requireRecord,
  requireSha256,
} from '../contract-support.ts'

export const MANUAL_REAL_NAT_PROFILE_ID = 'manual-real-nat' as const
export const MANUAL_REAL_NAT_AUTHORITY_ID = 'real-nat-external-fixture' as const
export const MANUAL_SUPPLEMENT_AVAILABILITY = 'operator-provisioned' as const

export interface ManualSupplementAuthority {
  readonly authorityId: typeof MANUAL_REAL_NAT_AUTHORITY_ID
  readonly authorityKind: 'external-fixture'
  readonly availabilityExpectation: typeof MANUAL_SUPPLEMENT_AVAILABILITY
  readonly attestationPublicKeySha256: string
}

// This is intentionally not a NetworkTopologyProfile. The distinct type makes
// it impossible to feed operator-run observations into scheduled hard counts.
export interface ManualSupplementProfile {
  readonly schemaVersion: typeof NETWORK_MANUAL_SUPPLEMENT_PROFILE_SCHEMA
  readonly supplementId: typeof NETWORK_MANUAL_SUPPLEMENT_ID
  readonly reportingSemantics: typeof NETWORK_MANUAL_SUPPLEMENT_REPORTING_SEMANTICS
  readonly profileId: typeof MANUAL_REAL_NAT_PROFILE_ID
  readonly authority: ManualSupplementAuthority
  readonly connectivityExpectation: 'connectivity-established'
  readonly candidatePolicy: NetworkMatrixCandidatePolicy
}

export function parseManualSupplementProfile(value: unknown): ManualSupplementProfile {
  const profile = requireRecord(value, 'manual browser network supplement profile')
  requireExactKeys(profile, [
    'schemaVersion',
    'supplementId',
    'reportingSemantics',
    'profileId',
    'authority',
    'connectivityExpectation',
    'candidatePolicy',
  ], 'manual browser network supplement profile')
  const authority = requireRecord(profile.authority, 'manual browser network supplement authority')
  requireExactKeys(authority, [
    'authorityId',
    'authorityKind',
    'availabilityExpectation',
    'attestationPublicKeySha256',
  ], 'manual browser network supplement authority')
  const connectivityExpectation = requireLiteral(
    profile.connectivityExpectation,
    'connectivity-established',
    'manual browser network supplement connectivity expectation',
  )
  return Object.freeze({
    schemaVersion: requireLiteral(
      profile.schemaVersion,
      NETWORK_MANUAL_SUPPLEMENT_PROFILE_SCHEMA,
      'manual browser network supplement schema',
    ),
    supplementId: requireLiteral(
      profile.supplementId,
      NETWORK_MANUAL_SUPPLEMENT_ID,
      'manual browser network supplement ID',
    ),
    reportingSemantics: requireLiteral(
      profile.reportingSemantics,
      NETWORK_MANUAL_SUPPLEMENT_REPORTING_SEMANTICS,
      'manual browser network supplement reporting semantics',
    ),
    profileId: requireLiteral(
      profile.profileId,
      MANUAL_REAL_NAT_PROFILE_ID,
      'manual browser network supplement profile ID',
    ),
    authority: Object.freeze({
      authorityId: requireLiteral(
        authority.authorityId,
        MANUAL_REAL_NAT_AUTHORITY_ID,
        'manual browser network supplement authority ID',
      ),
      authorityKind: requireLiteral(
        authority.authorityKind,
        'external-fixture',
        'manual browser network supplement authority kind',
      ),
      availabilityExpectation: requireLiteral(
        authority.availabilityExpectation,
        MANUAL_SUPPLEMENT_AVAILABILITY,
        'manual browser network supplement availability',
      ),
      attestationPublicKeySha256: requireSha256(
        authority.attestationPublicKeySha256,
        'manual browser network supplement attestation public-key digest',
      ),
    }),
    connectivityExpectation,
    candidatePolicy: parseNetworkCandidatePolicy(
      profile.candidatePolicy,
      connectivityExpectation,
    ),
  })
}

export function parseManualSupplementProfileJson(encoded: string): ManualSupplementProfile {
  return requireCanonicalEncoding(
    encoded,
    parseManualSupplementProfile(parseNetworkMatrixJsonText(
      encoded,
      'manual browser network supplement profile',
    )),
    'manual browser network supplement profile',
  )
}

export function canonicalManualSupplementProfileJson(profile: ManualSupplementProfile): string {
  return `${JSON.stringify(parseManualSupplementProfile(profile))}\n`
}
