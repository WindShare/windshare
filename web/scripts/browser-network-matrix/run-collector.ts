import {
  NETWORK_MATRIX_IDENTITIES_PER_PROFILE,
  NETWORK_RUN_RESULT_SCHEMA,
  NETWORK_SAMPLE_RESULT_SCHEMA,
  type NetworkMatrixExecutionMode,
} from './vocabulary.ts'
import {
  networkMatrixIdentities,
  networkMatrixIdentityKey,
  sha256,
  type LoadedNetworkMatrixRegistry,
  type NetworkMatrixIdentity,
} from './manifest.ts'
import {
  canonicalNetworkRuntimeAttestationJson,
  parseNetworkRuntimeAttestation,
  type NetworkRuntimeAttestation,
} from './attestation.ts'
import { evaluateNetworkCandidatePolicy } from './candidate.ts'
import {
  parseNetworkMatrixAttemptEvidence,
  type NetworkMatrixAttemptEvidence,
} from './attempt-evidence.ts'
import {
  parseNetworkRunResult,
  parseNetworkSampleResult,
  type NetworkOrchestrationFailure,
  type NetworkProfileRunResult,
  type NetworkRunResult,
  type NetworkSampleFailure,
  type NetworkSampleResult,
} from './result.ts'
import {
  networkMatrixError,
  requireEnum,
  requireRunId,
} from './contract-support.ts'
import { NETWORK_MATRIX_SAMPLE_FAILURE_CODES } from './vocabulary.ts'

export type NetworkMatrixSampleObservation =
  | {
      readonly sampleOutcome: 'observed'
      readonly attemptEvidence: NetworkMatrixAttemptEvidence
    }
  | {
      readonly sampleOutcome: 'infrastructure-failed'
      readonly failureCode: NetworkSampleFailure['failureCode']
    }

export interface NetworkMatrixRunCollectorOptions {
  readonly registry: LoadedNetworkMatrixRegistry
  readonly runId: string
  readonly executionMode: NetworkMatrixExecutionMode
}

export type MissingNetworkRuntimeAttestationFactory = (
  reference: LoadedNetworkMatrixRegistry['manifest']['profiles'][number],
) => NetworkRuntimeAttestation

/**
 * The collector owns ordering and all derived claims. Runtime adapters report
 * only prerequisite and two-ended attempt facts; they cannot self-award a policy
 * match or a completed run.
 */
export class NetworkMatrixRunCollector {
  readonly #registry: LoadedNetworkMatrixRegistry
  readonly #runId: string
  readonly #executionMode: NetworkMatrixExecutionMode
  readonly #expectedIdentities: readonly NetworkMatrixIdentity[]
  readonly #expectedProfiles: LoadedNetworkMatrixRegistry['manifest']['profiles']
  readonly #attestations: NetworkRuntimeAttestation[] = []
  readonly #samples: NetworkSampleResult[] = []
  readonly #processInstanceIds = new Set<string>()
  readonly #attemptIds = new Set<string>()
  readonly #challengeBindings = new Set<string>()
  #lastSamplePosition = -1
  #finalized = false

  constructor(options: NetworkMatrixRunCollectorOptions) {
    this.#registry = options.registry
    this.#runId = requireRunId(options.runId, 'browser network matrix collector run ID')
    this.#executionMode = options.executionMode
    this.#expectedIdentities = networkMatrixIdentities(
      options.registry.manifest,
      options.executionMode,
    )
    this.#expectedProfiles = Object.freeze(options.registry.manifest.profiles.filter(
      ({ executionMode }) => executionMode === options.executionMode,
    ))
  }

  recordAttestation(attestation: NetworkRuntimeAttestation): NetworkRuntimeAttestation {
    this.#requireMutable()
    const expected = this.#expectedProfiles[this.#attestations.length]
    if (expected === undefined || expected.profileId !== attestation.profileId) {
      networkMatrixError('runtime attestation repeats or violates execution-mode profile order')
    }
    const parsed = parseNetworkRuntimeAttestation(attestation, this.#attestationContext())
    this.#attestations.push(parsed)
    return parsed
  }

  recordSample(
    identity: NetworkMatrixIdentity,
    processInstanceId: string | null,
    observation: NetworkMatrixSampleObservation,
  ): NetworkSampleResult {
    this.#requireMutable()
    const position = this.#expectedIdentities.findIndex(
      (candidate) => networkMatrixIdentityKey(candidate) === networkMatrixIdentityKey(identity),
    )
    if (position < 0 || position <= this.#lastSamplePosition) {
      networkMatrixError('network sample repeats or violates canonical execution-mode identity order')
    }
    const reference = this.#registry.manifest.profiles.find(
      ({ profileId }) => profileId === identity.profileId,
    )
    const profile = this.#registry.profiles.find(({ profileId }) => profileId === identity.profileId)
    const attestation = this.#attestations.find(({ profileId }) => profileId === identity.profileId)
    if (reference === undefined || profile === undefined || attestation === undefined) {
      networkMatrixError('network sample lacks its profile or runtime attestation')
    }
    if (attestation.prerequisiteOutcome !== 'satisfied') {
      networkMatrixError('network sample cannot be recorded for an unsatisfied prerequisite')
    }
    const attestationSha256 = sha256(canonicalNetworkRuntimeAttestationJson(
      attestation,
      this.#attestationContext(),
    ))
    const raw = observation.sampleOutcome === 'observed'
      ? observedSample(
          this.#runId,
          this.#registry.manifestSha256,
          identity,
          reference.profileSha256,
          attestationSha256,
          requireRunId(
            processInstanceId,
            'network matrix collected browser process instance ID',
          ),
          observation.attemptEvidence,
          profile,
        )
      : failedSample(
          this.#runId,
          this.#registry.manifestSha256,
          identity,
          reference.profileSha256,
          attestationSha256,
          requireNullProcessInstance(processInstanceId),
          observation.failureCode,
        )
    const parsed = parseNetworkSampleResult(raw, {
      ...this.#attestationContext(),
      profiles: this.#registry.profiles,
      attestations: this.#attestations,
    })
    this.#requireUniqueObservedAuthorities(parsed)
    this.#lastSamplePosition = position
    this.#samples.push(parsed)
    return parsed
  }

  finalize(orchestrationFailure: NetworkOrchestrationFailure | null): NetworkRunResult {
    this.#requireMutable()
    this.#requireAttestationsComplete()
    this.#finalized = true
    const orchestrationOutcome = orchestrationFailure === null ? 'healthy' : 'failed'
    const profileResults = deriveProfileResults(
      this.#expectedIdentities,
      this.#attestations,
      this.#samples,
    )
    return parseNetworkRunResult({
      schemaVersion: NETWORK_RUN_RESULT_SCHEMA,
      runId: this.#runId,
      manifestSha256: this.#registry.manifestSha256,
      executionMode: this.#executionMode,
      orchestrationOutcome,
      orchestrationFailure,
      expectedIdentities: this.#expectedIdentities,
      runtimeAttestations: this.#attestations,
      samples: this.#samples,
      profileResults,
      runOutcome: deriveRunOutcome(
        this.#attestations,
        this.#samples,
        orchestrationOutcome,
      ),
    }, this.#registry)
  }

  /**
   * Failure terminalization retains every already-authenticated observation and
   * fills only the trailing profile attestations the interrupted runner never
   * reached. Replacing the collector would erase valid evidence from the same run.
   */
  terminalize(
    orchestrationFailure: NetworkOrchestrationFailure,
    missingAttestation: MissingNetworkRuntimeAttestationFactory,
  ): NetworkRunResult {
    this.#requireMutable()
    while (this.#attestations.length < this.#expectedProfiles.length) {
      const reference = this.#expectedProfiles[this.#attestations.length]
      if (reference === undefined) {
        networkMatrixError('network matrix collector profile terminalization lost registry order')
      }
      this.recordAttestation(missingAttestation(reference))
    }
    return this.finalize(orchestrationFailure)
  }

  #attestationContext() {
    return Object.freeze({
      manifest: this.#registry.manifest,
      manifestSha256: this.#registry.manifestSha256,
      runId: this.#runId,
    })
  }

  #requireAttestationsComplete(): void {
    if (this.#attestations.length !== this.#expectedProfiles.length) {
      networkMatrixError('runtime attestations differ from the exact execution-mode profile registry')
    }
  }

  #requireMutable(): void {
    if (this.#finalized) networkMatrixError('network matrix run collector is already finalized')
  }

  #requireUniqueObservedAuthorities(sample: NetworkSampleResult): void {
    if (sample.sampleOutcome !== 'observed') return
    if (sample.processInstanceId === null || sample.attemptEvidence === null) {
      networkMatrixError('observed sample lacks its process or attempt authority')
    }
    const challengeBinding = sample.attemptEvidence.challenge?.bindingSha256 ?? null
    if (
      this.#processInstanceIds.has(sample.processInstanceId) ||
      this.#attemptIds.has(sample.attemptEvidence.attemptAuthority.attemptId) ||
      (challengeBinding !== null && this.#challengeBindings.has(challengeBinding))
    ) {
      networkMatrixError('network matrix reuses an observed process, attempt, or challenge authority')
    }
    this.#processInstanceIds.add(sample.processInstanceId)
    this.#attemptIds.add(sample.attemptEvidence.attemptAuthority.attemptId)
    if (challengeBinding !== null) this.#challengeBindings.add(challengeBinding)
  }
}

function observedSample(
  runId: string,
  manifestSha256: string,
  identity: NetworkMatrixIdentity,
  profileSha256: string,
  attestationSha256: string,
  processInstanceId: string,
  attemptEvidenceValue: NetworkMatrixAttemptEvidence,
  profile: LoadedNetworkMatrixRegistry['profiles'][number],
): unknown {
  const attemptEvidence = parseNetworkMatrixAttemptEvidence(
    attemptEvidenceValue,
    identity.profileId,
  )
  const evaluation = evaluateNetworkCandidatePolicy(attemptEvidence.browserSelectedPair, profile)
  return {
    schemaVersion: NETWORK_SAMPLE_RESULT_SCHEMA,
    runId,
    manifestSha256,
    identity,
    profileSha256,
    attestationSha256,
    sampleOutcome: 'observed',
    processInstanceId,
    attemptEvidence,
    candidatePolicyOutcome: evaluation.candidatePolicyOutcome,
    rationaleCodes: evaluation.rationaleCodes,
    failure: null,
  }
}

function failedSample(
  runId: string,
  manifestSha256: string,
  identity: NetworkMatrixIdentity,
  profileSha256: string,
  attestationSha256: string,
  processInstanceId: null,
  failureCodeValue: NetworkSampleFailure['failureCode'],
): unknown {
  const failureCode = requireEnum(
    failureCodeValue,
    NETWORK_MATRIX_SAMPLE_FAILURE_CODES,
    'network matrix collected sample failure code',
  )
  return {
    schemaVersion: NETWORK_SAMPLE_RESULT_SCHEMA,
    runId,
    manifestSha256,
    identity,
    profileSha256,
    attestationSha256,
    sampleOutcome: 'infrastructure-failed',
    processInstanceId,
    attemptEvidence: null,
    candidatePolicyOutcome: 'not-evaluated',
    rationaleCodes: [],
    failure: { failureCode },
  }
}

function requireNullProcessInstance(value: string | null): null {
  if (value !== null) {
    networkMatrixError('infrastructure-failed sample cannot retain a browser process instance ID')
  }
  return null
}

function deriveProfileResults(
  expectedIdentities: readonly NetworkMatrixIdentity[],
  attestations: readonly NetworkRuntimeAttestation[],
  samples: readonly NetworkSampleResult[],
): readonly NetworkProfileRunResult[] {
  return Object.freeze(attestations.map((attestation) => {
    const expectedSamples = expectedIdentities.filter(
      ({ profileId }) => profileId === attestation.profileId,
    ).length
    if (expectedSamples !== NETWORK_MATRIX_IDENTITIES_PER_PROFILE) {
      networkMatrixError('network matrix profile does not expand to exactly 15 identities')
    }
    const profileSamples = samples.filter(
      ({ identity }) => identity.profileId === attestation.profileId,
    )
    const sampleInfrastructureFailures = profileSamples.filter(
      ({ sampleOutcome }) => sampleOutcome === 'infrastructure-failed',
    ).length
    return Object.freeze({
      profileId: attestation.profileId,
      prerequisiteOutcome: attestation.prerequisiteOutcome,
      expectedSamples: NETWORK_MATRIX_IDENTITIES_PER_PROFILE,
      observedSamples: profileSamples.length,
      sampleInfrastructureFailures,
      profileOutcome: deriveProfileOutcome(
        attestation.prerequisiteOutcome,
        profileSamples.length,
        sampleInfrastructureFailures,
      ),
    })
  }))
}

function deriveProfileOutcome(
  prerequisiteOutcome: NetworkRuntimeAttestation['prerequisiteOutcome'],
  observedSamples: number,
  sampleInfrastructureFailures: number,
): NetworkProfileRunResult['profileOutcome'] {
  if (prerequisiteOutcome !== 'satisfied') return 'not-executed'
  if (
    observedSamples !== NETWORK_MATRIX_IDENTITIES_PER_PROFILE ||
    sampleInfrastructureFailures > 0
  ) return 'infrastructure-failed'
  return 'completed'
}

function deriveRunOutcome(
  attestations: readonly NetworkRuntimeAttestation[],
  samples: readonly NetworkSampleResult[],
  orchestrationOutcome: 'healthy' | 'failed',
): NetworkRunResult['runOutcome'] {
  if (
    orchestrationOutcome === 'failed' ||
    samples.some(({ sampleOutcome }) => sampleOutcome === 'infrastructure-failed')
  ) return 'infrastructure-failed'
  const satisfied = attestations.filter(
    ({ prerequisiteOutcome }) => prerequisiteOutcome === 'satisfied',
  ).length
  if (satisfied === 0) return 'not-executed'
  return satisfied === attestations.length ? 'completed' : 'partial'
}
