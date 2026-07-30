import {
  NETWORK_MATRIX_AVAILABILITY_EXPECTATIONS,
  NETWORK_MATRIX_CANDIDATE_TYPES,
  NETWORK_MATRIX_PROFILE_REGISTRY,
  NETWORK_MATRIX_PROTOCOLS,
  NETWORK_TOPOLOGY_PROFILE_SCHEMA,
  type NetworkMatrixAuthorityId,
  type NetworkMatrixAuthorityKind,
  type NetworkMatrixCandidateType,
  type NetworkMatrixConnectivityExpectation,
  type NetworkMatrixExecutionMode,
  type NetworkMatrixProfileId,
  type NetworkMatrixProtocol,
  type NetworkMatrixSelectedPairRequirement,
} from './vocabulary.ts'
import {
  networkMatrixError,
  parseNetworkMatrixJsonText,
  requireCanonicalEncoding,
  requireCanonicalStringSet,
  requireExactKeys,
  requireLiteral,
  requireRecord,
  requireSha256,
} from './contract-support.ts'

export interface NetworkMatrixAuthorityRequirement {
  readonly authorityId: NetworkMatrixAuthorityId
  readonly authorityKind: NetworkMatrixAuthorityKind
  readonly availabilityExpectation: 'not-assumed'
  readonly attestationPublicKeySha256: string
}

export interface NetworkMatrixVocabularyConstraint<T extends string> {
  readonly allowed: readonly T[]
  readonly required: readonly T[]
  readonly forbidden: readonly T[]
}

export interface NetworkMatrixCandidatePolicy {
  readonly selectedPair: NetworkMatrixSelectedPairRequirement
  readonly localCandidateTypes: NetworkMatrixVocabularyConstraint<NetworkMatrixCandidateType>
  readonly remoteCandidateTypes: NetworkMatrixVocabularyConstraint<NetworkMatrixCandidateType>
  readonly protocols: NetworkMatrixVocabularyConstraint<NetworkMatrixProtocol>
}

export interface NetworkTopologyProfile {
  readonly schemaVersion: typeof NETWORK_TOPOLOGY_PROFILE_SCHEMA
  readonly profileId: NetworkMatrixProfileId
  readonly profileKind: NetworkMatrixProfileId
  readonly executionMode: NetworkMatrixExecutionMode
  readonly authority: NetworkMatrixAuthorityRequirement
  readonly connectivityExpectation: NetworkMatrixConnectivityExpectation
  readonly candidatePolicy: NetworkMatrixCandidatePolicy
}

export function parseNetworkTopologyProfile(value: unknown): NetworkTopologyProfile {
  const record = requireRecord(value, 'network topology profile')
  requireExactKeys(record, [
    'schemaVersion',
    'profileId',
    'profileKind',
    'executionMode',
    'authority',
    'connectivityExpectation',
    'candidatePolicy',
  ], 'network topology profile')
  const profileId = profileIdFrom(record.profileId)
  const registry = NETWORK_MATRIX_PROFILE_REGISTRY.find((entry) => entry.profileId === profileId)
  if (registry === undefined) networkMatrixError('network topology profile is absent from the registry')
  const authority = parseNetworkMatrixAuthorityRequirement(record.authority, registry)
  const connectivityExpectation = requireLiteral(
    record.connectivityExpectation,
    registry.profileId === 'scheduled-restricted-udp'
      ? 'connectivity-blocked'
      : 'connectivity-established',
    'network topology connectivity expectation',
  )
  const candidatePolicy = parseCandidatePolicy(record.candidatePolicy, connectivityExpectation)
  return Object.freeze({
    schemaVersion: requireLiteral(
      record.schemaVersion,
      NETWORK_TOPOLOGY_PROFILE_SCHEMA,
      'network topology profile schema',
    ),
    profileId,
    profileKind: requireLiteral(record.profileKind, registry.profileKind, 'network topology kind'),
    executionMode: requireLiteral(
      record.executionMode,
      registry.executionMode,
      'network topology execution mode',
    ),
    authority,
    connectivityExpectation,
    candidatePolicy,
  })
}

export function parseNetworkTopologyProfileJson(encoded: string): NetworkTopologyProfile {
  return requireCanonicalEncoding(
    encoded,
    parseNetworkTopologyProfile(parseNetworkMatrixJsonText(encoded, 'network topology profile')),
    'network topology profile',
  )
}

export function parseNetworkMatrixAuthorityRequirement(
  value: unknown,
  registry: (typeof NETWORK_MATRIX_PROFILE_REGISTRY)[number],
): NetworkMatrixAuthorityRequirement {
  const authority = requireRecord(value, 'network topology authority requirement')
  requireExactKeys(authority, [
    'authorityId',
    'authorityKind',
    'availabilityExpectation',
    'attestationPublicKeySha256',
  ], 'network topology authority requirement')
  const authorityKind = requireLiteral(
    authority.authorityKind,
    registry.authorityKind,
    'network topology authority kind',
  )
  return Object.freeze({
    authorityId: requireLiteral(
      authority.authorityId,
      registry.authorityId,
      'network topology authority ID',
    ),
    authorityKind,
    availabilityExpectation: requireLiteral(
      authority.availabilityExpectation,
      NETWORK_MATRIX_AVAILABILITY_EXPECTATIONS[0],
      'network topology availability expectation',
    ),
    attestationPublicKeySha256: requireSha256(
      authority.attestationPublicKeySha256,
      'network topology attestation public-key digest',
    ),
  })
}

function parseCandidatePolicy(
  value: unknown,
  connectivityExpectation: NetworkMatrixConnectivityExpectation,
): NetworkMatrixCandidatePolicy {
  const policy = requireRecord(value, 'network topology candidate policy')
  requireExactKeys(policy, [
    'selectedPair',
    'localCandidateTypes',
    'remoteCandidateTypes',
    'protocols',
  ], 'network topology candidate policy')
  const selectedPair = requireLiteral(
    policy.selectedPair,
    connectivityExpectation === 'connectivity-established' ? 'required' : 'prohibited',
    'network topology selected-pair requirement',
  )
  const localCandidateTypes = candidateConstraint(
    policy.localCandidateTypes,
    NETWORK_MATRIX_CANDIDATE_TYPES,
    'local candidate types',
  )
  const remoteCandidateTypes = candidateConstraint(
    policy.remoteCandidateTypes,
    NETWORK_MATRIX_CANDIDATE_TYPES,
    'remote candidate types',
  )
  const protocols = candidateConstraint(
    policy.protocols,
    NETWORK_MATRIX_PROTOCOLS,
    'candidate protocols',
  )
  const constraints = [localCandidateTypes, remoteCandidateTypes, protocols]
  if (selectedPair === 'prohibited') {
    if (constraints.some(({ allowed, required, forbidden }) =>
      allowed.length !== 0 || required.length !== 0 || forbidden.length !== 0)) {
      networkMatrixError('a prohibited selected pair cannot carry candidate path constraints')
    }
  } else if (constraints.some(({ allowed }) => allowed.length === 0)) {
    networkMatrixError('a required selected pair needs non-empty candidate and protocol policies')
  }
  return Object.freeze({
    selectedPair,
    localCandidateTypes,
    remoteCandidateTypes,
    protocols,
  })
}

function candidateConstraint<T extends string>(
  value: unknown,
  vocabulary: readonly T[],
  label: string,
): NetworkMatrixVocabularyConstraint<T> {
  const constraint = requireRecord(value, label)
  requireExactKeys(constraint, ['allowed', 'required', 'forbidden'], label)
  const allowed = requireCanonicalStringSet(constraint.allowed, vocabulary, `${label} allowed`)
  const required = requireCanonicalStringSet(constraint.required, vocabulary, `${label} required`)
  const forbidden = requireCanonicalStringSet(constraint.forbidden, vocabulary, `${label} forbidden`)
  if (required.length > 1) {
    networkMatrixError(`${label} cannot require multiple values from one observed selected-pair field`)
  }
  if (required.some((entry) => !allowed.includes(entry))) {
    networkMatrixError(`${label} required vocabulary is not a subset of allowed vocabulary`)
  }
  if (allowed.some((entry) => forbidden.includes(entry))) {
    networkMatrixError(`${label} allowed and forbidden vocabularies overlap`)
  }
  if (
    allowed.length !== 0 &&
    vocabulary.some((entry) => !allowed.includes(entry) && !forbidden.includes(entry))
  ) {
    networkMatrixError(`${label} policy does not classify every frozen vocabulary value`)
  }
  return Object.freeze({ allowed, required, forbidden })
}

function profileIdFrom(value: unknown): NetworkMatrixProfileId {
  if (typeof value !== 'string') networkMatrixError('network topology profile ID must be a string')
  const registry = NETWORK_MATRIX_PROFILE_REGISTRY.find(({ profileId }) => profileId === value)
  if (registry === undefined) networkMatrixError('network topology profile ID is outside the frozen registry')
  return registry.profileId
}
