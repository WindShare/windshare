import type { NetworkRuntimeAttestation } from '../attestation.ts'
import type { NetworkMatrixAttemptEvidence } from '../attempt-evidence.ts'
import type { NetworkMatrixIdentity } from '../manifest.ts'
import {
  NETWORK_MATRIX_IDENTITIES_PER_PROFILE,
  NETWORK_MATRIX_ORCHESTRATION_FAILURE_CODES,
  NETWORK_MATRIX_ORCHESTRATION_OUTCOMES,
  NETWORK_MATRIX_SAMPLE_FAILURE_CODES,
  NETWORK_RUN_RESULT_SCHEMA,
  NETWORK_SAMPLE_RESULT_SCHEMA,
  type NetworkMatrixCandidatePolicyOutcome,
  type NetworkMatrixExecutionMode,
  type NetworkMatrixPrerequisiteOutcome,
  type NetworkMatrixProfileId,
  type NetworkMatrixProfileRunOutcome,
  type NetworkMatrixRationaleCode,
  type NetworkMatrixRunOutcome,
  type NetworkMatrixSampleOutcome,
} from '../vocabulary.ts'

export interface NetworkSampleFailure {
  readonly failureCode: (typeof NETWORK_MATRIX_SAMPLE_FAILURE_CODES)[number]
}

export interface NetworkSampleResult {
  readonly schemaVersion: typeof NETWORK_SAMPLE_RESULT_SCHEMA
  readonly runId: string
  readonly manifestSha256: string
  readonly identity: NetworkMatrixIdentity
  readonly profileSha256: string
  readonly attestationSha256: string
  readonly sampleOutcome: NetworkMatrixSampleOutcome
  readonly processInstanceId: string | null
  readonly attemptEvidence: NetworkMatrixAttemptEvidence | null
  readonly candidatePolicyOutcome: NetworkMatrixCandidatePolicyOutcome
  readonly rationaleCodes: readonly NetworkMatrixRationaleCode[]
  readonly failure: NetworkSampleFailure | null
}

export interface NetworkOrchestrationFailure {
  readonly failureCode: (typeof NETWORK_MATRIX_ORCHESTRATION_FAILURE_CODES)[number]
}

export interface NetworkProfileRunResult {
  readonly profileId: NetworkMatrixProfileId
  readonly prerequisiteOutcome: NetworkMatrixPrerequisiteOutcome
  readonly expectedSamples: typeof NETWORK_MATRIX_IDENTITIES_PER_PROFILE
  readonly observedSamples: number
  readonly sampleInfrastructureFailures: number
  readonly profileOutcome: NetworkMatrixProfileRunOutcome
}

export interface NetworkRunResult {
  readonly schemaVersion: typeof NETWORK_RUN_RESULT_SCHEMA
  readonly runId: string
  readonly manifestSha256: string
  readonly executionMode: NetworkMatrixExecutionMode
  readonly orchestrationOutcome: (typeof NETWORK_MATRIX_ORCHESTRATION_OUTCOMES)[number]
  readonly orchestrationFailure: NetworkOrchestrationFailure | null
  readonly expectedIdentities: readonly NetworkMatrixIdentity[]
  readonly runtimeAttestations: readonly NetworkRuntimeAttestation[]
  readonly samples: readonly NetworkSampleResult[]
  readonly profileResults: readonly NetworkProfileRunResult[]
  readonly runOutcome: NetworkMatrixRunOutcome
}
