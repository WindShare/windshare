export const NETWORK_MATRIX_MANIFEST_SCHEMA =
  'windshare.browser-network-matrix.manifest/v2' as const
export const NETWORK_TOPOLOGY_PROFILE_SCHEMA =
  'windshare.browser-network-matrix.scheduled-profile/v2' as const
export const NETWORK_RUNTIME_ATTESTATION_SCHEMA =
  'windshare.browser-network-matrix.runtime-attestation/v3' as const
export const NETWORK_SAMPLE_RESULT_SCHEMA =
  'windshare.browser-network-matrix.scheduled-sample/v2' as const
export const NETWORK_RUN_RESULT_SCHEMA =
  'windshare.browser-network-matrix.scheduled-run/v2' as const
export const NETWORK_MATRIX_AGGREGATE_SCHEMA =
  'windshare.browser-network-matrix.scheduled-verdict/v2' as const
export const NETWORK_MANUAL_SUPPLEMENT_PROFILE_SCHEMA =
  'windshare.browser-network-matrix.manual-supplement-profile/v1' as const

export const NETWORK_MATRIX_ID = 'scheduled-hard-browser-network-v1' as const
export const NETWORK_MATRIX_REPORTING_SEMANTICS = 'scheduled-hard-fail-closed' as const
export const NETWORK_MANUAL_SUPPLEMENT_ID = 'manual-real-nat-supplement-v1' as const
export const NETWORK_MANUAL_SUPPLEMENT_REPORTING_SEMANTICS =
  'manual-supplemental-non-authoritative' as const

export const NETWORK_MATRIX_BROWSERS = Object.freeze([
  'chromium',
  'firefox',
  'webkit',
] as const)
export const NETWORK_MATRIX_SAMPLE_ORDINALS = Object.freeze([1, 2, 3, 4, 5] as const)
export const NETWORK_MATRIX_EXECUTION_MODES = Object.freeze(['scheduled'] as const)
export const NETWORK_MATRIX_PROFILE_IDS = Object.freeze([
  'scheduled-public-stun',
  'scheduled-restricted-udp',
  'scheduled-coturn',
] as const)
export const NETWORK_MATRIX_AUTHORITY_IDS = Object.freeze([
  'public-stun-external-fixture',
  'restricted-udp-external-fixture',
  'coturn-external-fixture',
] as const)
export const NETWORK_MATRIX_AUTHORITY_KINDS = Object.freeze([
  'external-fixture',
] as const)
export const NETWORK_MATRIX_AVAILABILITY_EXPECTATIONS = Object.freeze(['required'] as const)
export const NETWORK_MATRIX_CONNECTIVITY_EXPECTATIONS = Object.freeze([
  'connectivity-established',
  'connectivity-blocked',
] as const)
export const NETWORK_MATRIX_SELECTED_PAIR_REQUIREMENTS = Object.freeze([
  'required',
  'prohibited',
] as const)
export const NETWORK_MATRIX_CANDIDATE_TYPES = Object.freeze([
  'host',
  'srflx',
  'prflx',
  'relay',
] as const)
export const NETWORK_MATRIX_PROTOCOLS = Object.freeze(['udp', 'tcp'] as const)
export const NETWORK_MATRIX_PION_AUTHORITIES = Object.freeze([
  'external-remote',
] as const)
export const NETWORK_MATRIX_ADDRESS_FAMILIES = Object.freeze(['ipv4', 'ipv6'] as const)
export const NETWORK_MATRIX_PREREQUISITE_OUTCOMES = Object.freeze([
  'satisfied',
  'unavailable',
  'invalid',
  'failed',
] as const)
export const NETWORK_MATRIX_PROOF_KINDS = Object.freeze([
  'external-fixture-trust',
] as const)
export const NETWORK_MATRIX_UNAVAILABLE_FAILURE_CODES = Object.freeze([
  'authority-attestation-expired',
  'authority-key-rotation-required',
  'authority-not-provisioned',
  'authority-unreachable',
] as const)
export const NETWORK_MATRIX_INVALID_FAILURE_CODES = Object.freeze([
  'authority-binding-mismatch',
  'proof-invalid',
] as const)
export const NETWORK_MATRIX_FAILED_FAILURE_CODES = Object.freeze([
  'authority-probe-failed',
  'runtime-check-failed',
  'runtime-bootstrap-failed',
] as const)
export const NETWORK_MATRIX_SAMPLE_OUTCOMES = Object.freeze([
  'observed',
  'infrastructure-failed',
] as const)
export const NETWORK_MATRIX_CANDIDATE_POLICY_OUTCOMES = Object.freeze([
  'matched',
  'mismatched',
  'not-evaluated',
] as const)
export const NETWORK_MATRIX_SELECTED_PAIR_OBSERVATIONS = Object.freeze(['present', 'absent'] as const)
export const NETWORK_MATRIX_SAMPLE_FAILURE_CODES = Object.freeze([
  'sample-runner-failed',
  'sample-deadline-exceeded',
  'evidence-collection-failed',
] as const)
export const NETWORK_MATRIX_RATIONALE_CODES = Object.freeze([
  'selected-pair-required',
  'selected-pair-prohibited',
  'local-candidate-type-forbidden',
  'local-candidate-type-not-allowed',
  'local-candidate-type-required-missing',
  'remote-candidate-type-forbidden',
  'remote-candidate-type-not-allowed',
  'remote-candidate-type-required-missing',
  'protocol-forbidden',
  'protocol-not-allowed',
  'protocol-required-missing',
] as const)
export const NETWORK_MATRIX_ORCHESTRATION_OUTCOMES = Object.freeze(['healthy', 'failed'] as const)
export const NETWORK_MATRIX_ORCHESTRATION_FAILURE_CODES = Object.freeze([
  'runtime-bootstrap-failed',
  'orchestrator-deadline-exceeded',
  'collector-failed',
  'containment-cleanup-failed',
] as const)
export const NETWORK_MATRIX_RUN_OUTCOMES = Object.freeze([
  'completed',
  'partial',
  'not-executed',
  'infrastructure-failed',
] as const)
export const NETWORK_MATRIX_PROFILE_RUN_OUTCOMES = Object.freeze([
  'completed',
  'not-executed',
  'infrastructure-failed',
] as const)
export const NETWORK_MATRIX_EVIDENCE_OUTCOMES = Object.freeze([
  'complete',
  'incomplete',
  'infrastructure-failed',
] as const)

export const NETWORK_MATRIX_IDENTITY_COUNTS = Object.freeze({
  total: 45,
  scheduled: 45,
} as const)
export const NETWORK_MATRIX_IDENTITIES_PER_PROFILE = 15 as const
export const NETWORK_MATRIX_SAMPLE_RETRY_COUNT = 0 as const

export const NETWORK_MATRIX_PROFILE_REGISTRY = Object.freeze([
  Object.freeze({
    profileId: 'scheduled-public-stun',
    profileKind: 'scheduled-public-stun',
    executionMode: 'scheduled',
    authorityId: 'public-stun-external-fixture',
    authorityKind: 'external-fixture',
    profilePath: 'profiles/scheduled-public-stun.v2.json',
  }),
  Object.freeze({
    profileId: 'scheduled-restricted-udp',
    profileKind: 'scheduled-restricted-udp',
    executionMode: 'scheduled',
    authorityId: 'restricted-udp-external-fixture',
    authorityKind: 'external-fixture',
    profilePath: 'profiles/scheduled-restricted-udp.v2.json',
  }),
  Object.freeze({
    profileId: 'scheduled-coturn',
    profileKind: 'scheduled-coturn',
    executionMode: 'scheduled',
    authorityId: 'coturn-external-fixture',
    authorityKind: 'external-fixture',
    profilePath: 'profiles/scheduled-coturn.v2.json',
  }),
] as const)

export type NetworkMatrixBrowser = (typeof NETWORK_MATRIX_BROWSERS)[number]
export type NetworkMatrixSampleOrdinal = (typeof NETWORK_MATRIX_SAMPLE_ORDINALS)[number]
export type NetworkMatrixExecutionMode = (typeof NETWORK_MATRIX_EXECUTION_MODES)[number]
export type NetworkMatrixProfileId = (typeof NETWORK_MATRIX_PROFILE_IDS)[number]
export type NetworkMatrixAuthorityId = (typeof NETWORK_MATRIX_AUTHORITY_IDS)[number]
export type NetworkMatrixAuthorityKind = (typeof NETWORK_MATRIX_AUTHORITY_KINDS)[number]
export type NetworkMatrixConnectivityExpectation =
  (typeof NETWORK_MATRIX_CONNECTIVITY_EXPECTATIONS)[number]
export type NetworkMatrixSelectedPairRequirement =
  (typeof NETWORK_MATRIX_SELECTED_PAIR_REQUIREMENTS)[number]
export type NetworkMatrixCandidateType = (typeof NETWORK_MATRIX_CANDIDATE_TYPES)[number]
export type NetworkMatrixProtocol = (typeof NETWORK_MATRIX_PROTOCOLS)[number]
export type NetworkMatrixPionAuthority = (typeof NETWORK_MATRIX_PION_AUTHORITIES)[number]
export type NetworkMatrixAddressFamily = (typeof NETWORK_MATRIX_ADDRESS_FAMILIES)[number]
export type NetworkMatrixPrerequisiteOutcome =
  (typeof NETWORK_MATRIX_PREREQUISITE_OUTCOMES)[number]
export type NetworkMatrixProofKind = (typeof NETWORK_MATRIX_PROOF_KINDS)[number]
export type NetworkMatrixSampleOutcome = (typeof NETWORK_MATRIX_SAMPLE_OUTCOMES)[number]
export type NetworkMatrixCandidatePolicyOutcome =
  (typeof NETWORK_MATRIX_CANDIDATE_POLICY_OUTCOMES)[number]
export type NetworkMatrixRationaleCode = (typeof NETWORK_MATRIX_RATIONALE_CODES)[number]
export type NetworkMatrixRunOutcome = (typeof NETWORK_MATRIX_RUN_OUTCOMES)[number]
export type NetworkMatrixProfileRunOutcome = (typeof NETWORK_MATRIX_PROFILE_RUN_OUTCOMES)[number]
