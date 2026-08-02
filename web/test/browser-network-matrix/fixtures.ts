import { fileURLToPath } from 'node:url'

import {
  canonicalNetworkRuntimeAttestationJson,
  parseNetworkRuntimeAttestation,
  type NetworkRuntimeAttestation,
} from '../../scripts/browser-network-matrix/attestation.ts'
import {
  evaluateNetworkCandidatePolicy,
} from '../../scripts/browser-network-matrix/candidate.ts'
import type {
  NetworkMatrixAttemptEvidence,
} from '../../scripts/browser-network-matrix/attempt-evidence.ts'
import {
  loadNetworkMatrixRegistry,
  networkMatrixIdentities,
  sha256,
  type LoadedNetworkMatrixRegistry,
  type NetworkMatrixIdentity,
} from '../../scripts/browser-network-matrix/manifest.ts'
import { parseNetworkRunResult, type NetworkRunResult } from '../../scripts/browser-network-matrix/result.ts'
import type {
  NetworkMatrixExecutionMode,
  NetworkMatrixPrerequisiteOutcome,
  NetworkMatrixProfileId,
} from '../../scripts/browser-network-matrix/vocabulary.ts'
import {
  NETWORK_MATRIX_IDENTITIES_PER_PROFILE,
  NETWORK_RUN_RESULT_SCHEMA,
  NETWORK_RUNTIME_ATTESTATION_SCHEMA,
  NETWORK_SAMPLE_RESULT_SCHEMA,
} from '../../scripts/browser-network-matrix/vocabulary.ts'
import {
  testExternalFixtureTrustProof,
  testNetworkMatrixAttemptEvidence,
  type TestNetworkMatrixAttemptOptions,
} from './signed-fixture.ts'

export const MANIFEST_PATH = fileURLToPath(new URL(
  '../../../testdata/browser-network-matrix/scheduled-hard.manifest.v2.json',
  import.meta.url,
))

export function loadRegistry(): Promise<LoadedNetworkMatrixRegistry> {
  return loadNetworkMatrixRegistry(MANIFEST_PATH)
}

export interface RunFixtureOptions {
  readonly runId?: string
  readonly prerequisiteOutcomes?: Readonly<Partial<Record<
    NetworkMatrixProfileId,
    NetworkMatrixPrerequisiteOutcome
  >>>
  readonly orchestrationOutcome?: 'healthy' | 'failed'
  readonly sampleFilter?: (identity: NetworkMatrixIdentity) => boolean
  readonly sampleMutator?: (
    sample: Record<string, unknown>,
    identity: NetworkMatrixIdentity,
  ) => void
}

export function makeRunRaw(
  registry: LoadedNetworkMatrixRegistry,
  mode: NetworkMatrixExecutionMode,
  options: RunFixtureOptions = {},
): Record<string, unknown> {
  const runId = options.runId ?? `${mode}-network-run`
  const context = Object.freeze({
    manifest: registry.manifest,
    manifestSha256: registry.manifestSha256,
    runId,
  })
  const references = registry.manifest.profiles.filter(
    (profile) => profile.executionMode === mode,
  )
  const rawAttestations = references.map((reference) => rawAttestation(
    registry,
    runId,
    reference.profileId,
    options.prerequisiteOutcomes?.[reference.profileId] ?? 'satisfied',
  ))
  const attestations = rawAttestations.map((attestation) =>
    parseNetworkRuntimeAttestation(attestation, context))
  const attestationByProfile = new Map(attestations.map((attestation) => [
    attestation.profileId,
    attestation,
  ]))
  const expectedIdentities = networkMatrixIdentities(registry.manifest, mode)
  const samples = expectedIdentities.flatMap((identity) => {
    const attestation = attestationByProfile.get(identity.profileId)
    if (
      attestation?.prerequisiteOutcome !== 'satisfied' ||
      options.sampleFilter?.(identity) === false
    ) return []
    const sample = rawSample(registry, context, attestation, identity)
    options.sampleMutator?.(sample, identity)
    return [sample]
  })
  const orchestrationOutcome = options.orchestrationOutcome ?? 'healthy'
  const satisfiedCount = attestations.filter(
    ({ prerequisiteOutcome }) => prerequisiteOutcome === 'satisfied',
  ).length
  const runOutcome = fixtureRunOutcome(
    orchestrationOutcome,
    satisfiedCount,
    attestations.length,
    samples.filter(({ sampleOutcome }) => sampleOutcome === 'infrastructure-failed').length,
  )
  const profileResults = attestations.map((attestation) => {
    const profileSamples = samples.filter((sample) => {
      const identity = sample.identity as Record<string, unknown>
      return identity.profileId === attestation.profileId
    })
    const sampleInfrastructureFailures = profileSamples.filter(
      ({ sampleOutcome }) => sampleOutcome === 'infrastructure-failed',
    ).length
    return {
      profileId: attestation.profileId,
      prerequisiteOutcome: attestation.prerequisiteOutcome,
      expectedSamples: NETWORK_MATRIX_IDENTITIES_PER_PROFILE,
      observedSamples: profileSamples.length,
      sampleInfrastructureFailures,
      profileOutcome: fixtureProfileOutcome(
        attestation.prerequisiteOutcome,
        profileSamples.length,
        sampleInfrastructureFailures,
      ),
    }
  })
  return {
    schemaVersion: NETWORK_RUN_RESULT_SCHEMA,
    runId,
    manifestSha256: registry.manifestSha256,
    executionMode: mode,
    orchestrationOutcome,
    orchestrationFailure: orchestrationOutcome === 'failed'
      ? { failureCode: 'collector-failed' }
      : null,
    expectedIdentities,
    runtimeAttestations: rawAttestations,
    samples,
    profileResults,
    runOutcome,
  }
}

export function makeRun(
  registry: LoadedNetworkMatrixRegistry,
  mode: NetworkMatrixExecutionMode,
  options: RunFixtureOptions = {},
): NetworkRunResult {
  return parseNetworkRunResult(makeRunRaw(registry, mode, options), registry)
}

export function rawAttestation(
  registry: LoadedNetworkMatrixRegistry,
  runId: string,
  profileId: NetworkMatrixProfileId,
  prerequisiteOutcome: NetworkMatrixPrerequisiteOutcome,
): Record<string, unknown> {
  const reference = registry.manifest.profiles.find((profile) => profile.profileId === profileId)
  if (reference === undefined) throw new Error(`test fixture lacks profile ${profileId}`)
  return {
    schemaVersion: NETWORK_RUNTIME_ATTESTATION_SCHEMA,
    runId,
    manifestSha256: registry.manifestSha256,
    profileId,
    profileSha256: reference.profileSha256,
    authorityId: reference.authorityId,
    authorityKind: reference.authorityKind,
    prerequisiteOutcome,
    proof: prerequisiteOutcome === 'satisfied'
      ? authorityProof()
      : null,
    failure: prerequisiteOutcome === 'satisfied' ? null : prerequisiteFailure(prerequisiteOutcome),
  }
}

function rawSample(
  registry: LoadedNetworkMatrixRegistry,
  context: { readonly manifest: LoadedNetworkMatrixRegistry['manifest']; readonly manifestSha256: string; readonly runId: string },
  attestation: NetworkRuntimeAttestation,
  identity: NetworkMatrixIdentity,
): Record<string, unknown> {
  const reference = registry.manifest.profiles.find((profile) => profile.profileId === identity.profileId)
  const profile = registry.profiles.find((profile) => profile.profileId === identity.profileId)
  if (reference === undefined || profile === undefined) throw new Error('test sample lacks profile')
  const attemptEvidence = cloneJson(matchedAttemptEvidence(identity, context.runId))
  const evaluation = evaluateNetworkCandidatePolicy(attemptEvidence.browserSelectedPair, profile)
  return {
    schemaVersion: NETWORK_SAMPLE_RESULT_SCHEMA,
    runId: context.runId,
    manifestSha256: context.manifestSha256,
    identity,
    profileSha256: reference.profileSha256,
    attestationSha256: sha256(canonicalNetworkRuntimeAttestationJson(attestation, context)),
    sampleOutcome: 'observed',
    processInstanceId: fixtureProcessInstanceId(identity),
    attemptEvidence,
    candidatePolicyOutcome: evaluation.candidatePolicyOutcome,
    rationaleCodes: evaluation.rationaleCodes,
    failure: null,
  }
}

export function fixtureProcessInstanceId(identity: NetworkMatrixIdentity): string {
  return `process-${identity.profileId}-${identity.browser}-${identity.sampleOrdinal}`
}

export function matchedAttemptEvidence(
  identity: NetworkMatrixIdentity,
  runId = 'fixture-network-run',
  options: TestNetworkMatrixAttemptOptions = {},
): NetworkMatrixAttemptEvidence {
  return testNetworkMatrixAttemptEvidence(runId, identity, options)
}

function authorityProof(): Record<string, unknown> {
  return {
    proofKind: 'external-fixture-trust',
    externalFixtureTrust: testExternalFixtureTrustProof(),
  }
}

function prerequisiteFailure(
  outcome: Exclude<NetworkMatrixPrerequisiteOutcome, 'satisfied'>,
): Record<string, unknown> {
  return {
    failureKind: outcome,
    failureCode: fixtureFailureCode(outcome),
  }
}

function fixtureRunOutcome(
  orchestrationOutcome: 'healthy' | 'failed',
  satisfiedCount: number,
  attestationCount: number,
  sampleInfrastructureFailures: number,
): 'completed' | 'partial' | 'not-executed' | 'infrastructure-failed' {
  if (orchestrationOutcome === 'failed') return 'infrastructure-failed'
  if (sampleInfrastructureFailures > 0) return 'infrastructure-failed'
  if (satisfiedCount === 0) return 'not-executed'
  if (satisfiedCount === attestationCount) return 'completed'
  return 'partial'
}

function fixtureProfileOutcome(
  prerequisiteOutcome: NetworkMatrixPrerequisiteOutcome,
  observedSamples: number,
  sampleInfrastructureFailures: number,
): 'completed' | 'not-executed' | 'infrastructure-failed' {
  if (prerequisiteOutcome !== 'satisfied') return 'not-executed'
  if (
    observedSamples !== NETWORK_MATRIX_IDENTITIES_PER_PROFILE ||
    sampleInfrastructureFailures > 0
  ) {
    return 'infrastructure-failed'
  }
  return 'completed'
}

function fixtureFailureCode(
  outcome: Exclude<NetworkMatrixPrerequisiteOutcome, 'satisfied'>,
): 'authority-not-provisioned' | 'proof-invalid' | 'runtime-check-failed' {
  if (outcome === 'unavailable') return 'authority-not-provisioned'
  if (outcome === 'invalid') return 'proof-invalid'
  return 'runtime-check-failed'
}

export type MutableJson<T> = T extends readonly (infer Entry)[]
  ? MutableJson<Entry>[]
  : T extends object
    ? { -readonly [Key in keyof T]: MutableJson<T[Key]> }
    : T

export function cloneJson<T>(value: T): MutableJson<T> {
  return JSON.parse(JSON.stringify(value)) as MutableJson<T>
}
