import { readFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'

import { beforeAll, describe, expect, it } from 'vitest'

import {
  aggregateNetworkMatrix,
  canonicalNetworkMatrixAggregateJson,
  parseNetworkMatrixAggregate,
  parseNetworkMatrixAggregateJson,
} from '../../scripts/browser-network-matrix/aggregate.ts'
import {
  evaluateNetworkCandidatePolicy,
  parseNetworkCandidatePath,
} from '../../scripts/browser-network-matrix/candidate.ts'
import {
  sha256,
  type LoadedNetworkMatrixRegistry,
  type NetworkMatrixIdentity,
} from '../../scripts/browser-network-matrix/manifest.ts'
import {
  canonicalNetworkRunResultJson,
  parseNetworkRunResult,
  parseNetworkRunResultJson,
  parseNetworkSampleResultJson,
} from '../../scripts/browser-network-matrix/result.ts'
import {
  cloneJson,
  loadRegistry,
  matchedAttemptEvidence,
  makeRun,
  makeRunRaw,
} from './fixtures.ts'
import {
  TEST_BROWSER_PUBLIC_IP,
  TEST_REMOTE_PEER_PUBLIC_IP,
} from './signed-fixture.ts'

let registry: LoadedNetworkMatrixRegistry

const SHARED_RUN_VECTOR_PATH = fileURLToPath(new URL(
  '../../../testdata/browser-network-matrix/vectors/scheduled-not-executed.run.v2.json',
  import.meta.url,
))
const SHARED_OBSERVED_SAMPLE_VECTOR_PATH = fileURLToPath(new URL(
  '../../../testdata/browser-network-matrix/vectors/public-observed.sample.v2.json',
  import.meta.url,
))
const SHARED_AGGREGATE_VECTOR_PATH = fileURLToPath(new URL(
  '../../../testdata/browser-network-matrix/vectors/scheduled-incomplete.verdict.v2.json',
  import.meta.url,
))

beforeAll(async () => { registry = await loadRegistry() })

describe('browser network matrix candidate-path policy', () => {
  it('emits stable rationale order without treating mismatch as infrastructure failure', () => {
    const profile = registry.profiles[0]!
    expect(evaluateNetworkCandidatePolicy({
      selectedPair: 'absent',
      localCandidateType: null,
      localAddress: null,
      localPort: null,
      remoteCandidateType: null,
      remoteAddress: null,
      remotePort: null,
      protocol: null,
    }, profile)).toEqual({
      candidatePolicyOutcome: 'mismatched',
      rationaleCodes: ['selected-pair-required'],
    })

    expect(evaluateNetworkCandidatePolicy({
      selectedPair: 'present',
      localCandidateType: 'relay',
      localAddress: TEST_BROWSER_PUBLIC_IP,
      localPort: 50_000,
      remoteCandidateType: 'relay',
      remoteAddress: TEST_REMOTE_PEER_PUBLIC_IP,
      remotePort: 40_000,
      protocol: 'udp',
    }, profile)).toEqual({
      candidatePolicyOutcome: 'mismatched',
      rationaleCodes: [
        'local-candidate-type-forbidden',
        'local-candidate-type-required-missing',
        'remote-candidate-type-forbidden',
      ],
    })

    expect(evaluateNetworkCandidatePolicy({
      selectedPair: 'present',
      localCandidateType: 'host',
      localAddress: TEST_BROWSER_PUBLIC_IP,
      localPort: 50_000,
      remoteCandidateType: 'host',
      remoteAddress: TEST_REMOTE_PEER_PUBLIC_IP,
      remotePort: 40_000,
      protocol: 'udp',
    }, registry.profiles[1]!)).toEqual({
      candidatePolicyOutcome: 'mismatched',
      rationaleCodes: ['selected-pair-prohibited'],
    })
  })

  it('rejects an absent pair that smuggles candidate observations', () => {
    expect(() => parseNetworkCandidatePath({
      selectedPair: 'absent',
      localCandidateType: 'host',
      localAddress: null,
      localPort: null,
      remoteCandidateType: null,
      remoteAddress: null,
      remotePort: null,
      protocol: null,
    })).toThrow(/cannot carry/u)
  })
})

describe('browser network matrix run result', () => {
  it('derives a completed run from the exact 45 scheduled identities', () => {
    const scheduled = makeRun(registry, 'scheduled', { runId: 'scheduled-complete-run' })
    expect(scheduled).toMatchObject({ runOutcome: 'completed', orchestrationOutcome: 'healthy' })
    expect([scheduled.expectedIdentities.length, scheduled.samples.length]).toEqual([45, 45])
    expect(scheduled.profileResults).toEqual([
      expect.objectContaining({
        profileId: 'scheduled-public-stun',
        expectedSamples: 15,
        observedSamples: 15,
        sampleInfrastructureFailures: 0,
        profileOutcome: 'completed',
      }),
      expect.objectContaining({ profileId: 'scheduled-restricted-udp', profileOutcome: 'completed' }),
      expect.objectContaining({ profileId: 'scheduled-coturn', profileOutcome: 'completed' }),
    ])
  })

  it('derives partial and not-executed only from whole-profile prerequisites', () => {
    const partial = makeRun(registry, 'scheduled', {
      runId: 'scheduled-partial-run',
      prerequisiteOutcomes: { 'scheduled-restricted-udp': 'unavailable' },
    })
    expect(partial.runOutcome).toBe('partial')
    expect(partial.samples).toHaveLength(30)
    expect(partial.samples.some(({ identity }) =>
      identity.profileId === 'scheduled-restricted-udp')).toBe(false)
    expect(partial.profileResults[1]).toMatchObject({
      profileId: 'scheduled-restricted-udp',
      prerequisiteOutcome: 'unavailable',
      observedSamples: 0,
      profileOutcome: 'not-executed',
    })

    const notExecuted = makeRun(registry, 'scheduled', {
      runId: 'scheduled-not-executed-run',
      prerequisiteOutcomes: {
        'scheduled-public-stun': 'unavailable',
        'scheduled-restricted-udp': 'unavailable',
        'scheduled-coturn': 'unavailable',
      },
    })
    expect(notExecuted.runOutcome).toBe('not-executed')
    expect(notExecuted.samples).toEqual([])
    expect(notExecuted.profileResults.every(({ profileOutcome }) =>
      profileOutcome === 'not-executed')).toBe(true)
  })

  it('rejects healthy per-profile sample loss but preserves real subsets after orchestration failure', () => {
    expect(() => makeRun(registry, 'scheduled', {
      runId: 'healthy-missing-sample-run',
      sampleFilter: ({ sampleOrdinal }) => sampleOrdinal !== 5,
    })).toThrow(/omitted or reordered/u)

    const failed = makeRun(registry, 'scheduled', {
      runId: 'failed-subset-run',
      orchestrationOutcome: 'failed',
      sampleFilter: ({ sampleOrdinal }) => sampleOrdinal === 1,
    })
    expect(failed.runOutcome).toBe('infrastructure-failed')
    expect(failed.samples).toHaveLength(9)
    expect(failed.profileResults.every(({ profileOutcome }) =>
      profileOutcome === 'infrastructure-failed')).toBe(true)

    const failedAfterCompleteProfiles = makeRun(registry, 'scheduled', {
      runId: 'failed-after-complete-profiles-run',
      orchestrationOutcome: 'failed',
    })
    expect(failedAfterCompleteProfiles.runOutcome).toBe('infrastructure-failed')
    expect(failedAfterCompleteProfiles.profileResults.every(({ profileOutcome }) =>
      profileOutcome === 'completed')).toBe(true)
  })

  it('rejects fabricated samples for unsatisfied profiles and noncanonical identity order', () => {
    const partialRaw = makeRunRaw(registry, 'scheduled', {
      runId: 'fabricated-unsatisfied-sample-run',
      prerequisiteOutcomes: { 'scheduled-public-stun': 'unavailable' },
    })
    partialRaw.samples = makeRunRaw(registry, 'scheduled', {
      runId: 'fabricated-unsatisfied-sample-run',
    }).samples
    expect(() => parseNetworkRunResult(partialRaw, registry)).toThrow(/unsatisfied/u)

    const reordered = makeRunRaw(registry, 'scheduled', { runId: 'reordered-sample-run' })
    const reorderedSamples = reordered.samples as Record<string, unknown>[]
    ;[reorderedSamples[0], reorderedSamples[1]] = [reorderedSamples[1]!, reorderedSamples[0]!]
    expect(() => parseNetworkRunResult(reordered, registry)).toThrow(/canonical identity order/u)

    const duplicate = makeRunRaw(registry, 'scheduled', { runId: 'duplicate-sample-run' })
    const duplicateSamples = duplicate.samples as Record<string, unknown>[]
    duplicateSamples[1] = cloneJson(duplicateSamples[0]!)
    expect(() => parseNetworkRunResult(duplicate, registry)).toThrow(/repeat or violate/u)
  })

  it('derives candidate evaluation and attestation binding instead of trusting claims', () => {
    const forgedEvaluation = makeRunRaw(registry, 'scheduled', {
      runId: 'forged-candidate-evaluation-run',
      sampleMutator: (sample, identity) => {
        if (isFirstPublicSample(identity)) replaceBrowserSelectedPair(
          sample,
          mismatchingCandidatePath(),
        )
      },
    })
    expect(() => parseNetworkRunResult(forgedEvaluation, registry))
      .toThrow(/candidate policy outcome/u)

    const reboundAttestation = cloneJson(makeRunRaw(registry, 'scheduled', {
      runId: 'rebound-attestation-run',
    }))
    const attestations = reboundAttestation.runtimeAttestations as Record<string, unknown>[]
    const proof = attestations[0]!.proof as Record<string, unknown>
    const trust = proof.externalFixtureTrust as Record<string, unknown>
    trust.tlsCertificateSha256 = 'f'.repeat(64)
    expect(() => parseNetworkRunResult(reboundAttestation, registry))
      .toThrow(/attestation digest/u)
  })

  it('requires one cross-checked browser/Pion attempt with profile-bound authority', () => {
    const crossed = makeRunRaw(registry, 'scheduled', {
      runId: 'crossed-attempt-evidence-run',
    })
    const crossedEvidence = firstSampleEvidence(crossed)
    const crossedBrowser = crossedEvidence.browserSelectedPair as Record<string, unknown>
    crossedBrowser.remotePort = 40_001
    expect(() => parseNetworkRunResult(crossed, registry)).toThrow(/same attempt/u)

    const wrongAuthority = makeRunRaw(registry, 'scheduled', {
      runId: 'wrong-pion-authority-run',
    })
    ;(firstSampleEvidence(wrongAuthority) as Record<string, unknown>).pionAuthority =
      'fabricated-remote-authority'
    expect(() => parseNetworkRunResult(wrongAuthority, registry)).toThrow(/frozen vocabulary/u)

    const unprovenChallenge = makeRunRaw(registry, 'scheduled', {
      runId: 'unproven-attempt-challenge-run',
    })
    const challenge = firstSampleEvidence(unprovenChallenge).challenge as Record<string, unknown>
    challenge.browserEchoObserved = false
    expect(() => parseNetworkRunResult(unprovenChallenge, registry)).toThrow(/must be true/u)

    const legacyAlias = makeRunRaw(registry, 'scheduled', { runId: 'legacy-candidate-path-run' })
    const legacySample = (legacyAlias.samples as Record<string, unknown>[])[0]!
    legacySample.candidatePath = legacySample.attemptEvidence
    expect(() => parseNetworkRunResult(legacyAlias, registry)).toThrow(/exactly/u)
  })

  it('rejects reuse of process, attempt, and challenge authorities across observed samples', () => {
    const duplicateProcess = makeRunRaw(registry, 'scheduled', {
      runId: 'duplicate-process-authority-run',
    })
    const processSamples = duplicateProcess.samples as Record<string, unknown>[]
    const firstProcessInstanceId = processSamples[0]!.processInstanceId as string
    const secondProcessIdentity = processSamples[1]!.identity as NetworkMatrixIdentity
    processSamples[1]!.processInstanceId = firstProcessInstanceId
    processSamples[1]!.attemptEvidence = cloneJson(matchedAttemptEvidence(
      secondProcessIdentity,
      'duplicate-process-authority-run',
      { processInstanceId: firstProcessInstanceId },
    ))
    expect(() => parseNetworkRunResult(duplicateProcess, registry)).toThrow(/process instance ID/u)

    const duplicateAttempt = makeRunRaw(registry, 'scheduled', {
      runId: 'duplicate-attempt-authority-run',
    })
    const attemptSamples = duplicateAttempt.samples as Record<string, unknown>[]
    const firstAttempt = attemptSamples[0]!.attemptEvidence as Record<string, unknown>
    const firstAttemptAuthority = firstAttempt.attemptAuthority as Record<string, unknown>
    const secondIdentity = attemptSamples[1]!.identity as NetworkMatrixIdentity
    attemptSamples[1]!.attemptEvidence = cloneJson(matchedAttemptEvidence(
      secondIdentity,
      'duplicate-attempt-authority-run',
      { attemptId: firstAttemptAuthority.attemptId as string },
    ))
    expect(() => parseNetworkRunResult(duplicateAttempt, registry)).toThrow(/attempt ID/u)

    const duplicateChallenge = makeRunRaw(registry, 'scheduled', {
      runId: 'duplicate-challenge-authority-run',
    })
    const challengeSamples = duplicateChallenge.samples as Record<string, unknown>[]
    const firstChallenge = (
      challengeSamples[0]!.attemptEvidence as Record<string, unknown>
    ).challenge as Record<string, unknown>
    const secondChallenge = (
      challengeSamples[1]!.attemptEvidence as Record<string, unknown>
    ).challenge as Record<string, unknown>
    secondChallenge.bindingSha256 = firstChallenge.bindingSha256
    expect(() => parseNetworkRunResult(duplicateChallenge, registry))
      .toThrow(/bind the exact signed attempt authority/u)
  })

  it('derives profile ledger outcomes and marks typed sample failures as run infrastructure failure', () => {
    const failedSample = makeRun(registry, 'scheduled', {
      runId: 'profile-ledger-sample-failure-run',
      sampleMutator: applySampleInfrastructureFailure,
    })
    expect(failedSample.runOutcome).toBe('infrastructure-failed')
    expect(failedSample.profileResults[0]).toMatchObject({
      observedSamples: 15,
      sampleInfrastructureFailures: 1,
      profileOutcome: 'infrastructure-failed',
    })

    const forgedProfileResult = makeRunRaw(registry, 'scheduled', {
      runId: 'forged-profile-ledger-run',
    })
    const profileResults = forgedProfileResult.profileResults as Record<string, unknown>[]
    profileResults[0]!.observedSamples = 14
    expect(() => parseNetworkRunResult(forgedProfileResult, registry))
      .toThrow(/observed samples/u)
  })

  it('keeps canonical run bytes exact and rejects unknown or alternate encodings', () => {
    const run = makeRun(registry, 'scheduled', { runId: 'canonical-scheduled-run' })
    const encoded = canonicalNetworkRunResultJson(run, registry)
    expect(parseNetworkRunResultJson(encoded, registry)).toEqual(run)
    expect(() => parseNetworkRunResultJson(`\n${encoded}`, registry))
      .toThrow(/canonical minified JSON/u)

    const unknownField = { ...cloneJson(run), verdict: 'passed' }
    expect(() => parseNetworkRunResult(unknownField, registry)).toThrow(/exactly/u)
  })

  it('accepts the same bounded maximum-authority run size as the Go parser', () => {
    const runId = 'maximum-authority-run'
    let sampleIndex = 0
    const raw = makeRunRaw(registry, 'scheduled', {
      runId,
      sampleMutator: (sample, identity) => {
        const suffix = sampleIndex.toString(36).padStart(2, '0')
        const processInstanceId = `p-${'a'.repeat(90)}-${suffix}`
        sample.processInstanceId = processInstanceId
        sample.attemptEvidence = cloneJson(matchedAttemptEvidence(identity, runId, {
          attemptId: `attempt-${'A'.repeat(117)}-${suffix}`,
          processInstanceId,
        }))
        sampleIndex += 1
      },
    })
    const encoded = `${JSON.stringify(raw)}\n`
    expect(new TextEncoder().encode(encoded).byteLength).toBeGreaterThan(64 * 1024)
    expect(parseNetworkRunResultJson(encoded, registry).samples).toHaveLength(45)
  })
})

describe('browser network matrix scheduled hard verdict', () => {
  it('parses the same canonical execution-mode vectors as the Go contract', async () => {
    const sampleEncoded = await readFile(SHARED_OBSERVED_SAMPLE_VECTOR_PATH, 'utf8')
    expect(sha256(sampleEncoded)).toBe('ab497cc8b0fd9352fc4bf085ffbccf6201c199b02ccd66e2f37a864520012f62')
    const observedRun = makeRun(registry, 'scheduled', {
      runId: 'shared-observed-sample-run',
    })
    const sample = parseNetworkSampleResultJson(sampleEncoded, {
      manifest: registry.manifest,
      manifestSha256: registry.manifestSha256,
      runId: observedRun.runId,
      profiles: registry.profiles,
      attestations: observedRun.runtimeAttestations,
    })
    expect(sample).toMatchObject({
      processInstanceId: 'process-scheduled-public-stun-chromium-1',
      attemptEvidence: {
        pionAuthority: 'external-remote',
        browserSelectedPair: { selectedPair: 'present' },
        pionSelectedPair: { selectedPair: 'present' },
        challenge: { pionChallengeObserved: true, browserEchoObserved: true },
      },
    })

    const runEncoded = await readFile(SHARED_RUN_VECTOR_PATH, 'utf8')
    expect(sha256(runEncoded)).toBe('b80b7b6569385a288097a0a111a581e8cf8b723c8d54a0819a7eb23b3bca3eb5')
    const run = parseNetworkRunResultJson(runEncoded, registry)
    expect(run).toMatchObject({
      executionMode: 'scheduled',
      samples: [],
      profileResults: [
        { profileOutcome: 'not-executed' },
        { profileOutcome: 'not-executed' },
        { profileOutcome: 'not-executed' },
      ],
      runOutcome: 'not-executed',
    })

    const aggregateEncoded = await readFile(SHARED_AGGREGATE_VECTOR_PATH, 'utf8')
    expect(sha256(aggregateEncoded))
      .toBe('879aaa1fa7a35f3a232df4c5f3e5e7f1f956e2950eff94181dc409218e1336e3')
    expect(parseNetworkMatrixAggregateJson(aggregateEncoded, registry, [run])).toMatchObject({
      runs: [{ executionMode: 'scheduled', runId: run.runId }],
      counts: { expectedIdentities: 45, observedSamples: 0 },
      evidenceOutcome: 'incomplete',
    })
  })

  it('publishes exactly one real scheduled ledger without fabricating supplemental evidence', () => {
    const scheduled = makeRun(registry, 'scheduled', { runId: 'scheduled-only-ledger-run' })
    const scheduledAggregate = aggregateNetworkMatrix(registry, [scheduled])
    expect(scheduledAggregate.runs).toEqual([
      expect.objectContaining({ executionMode: 'scheduled', runId: scheduled.runId }),
    ])
    expect(scheduledAggregate.counts).toMatchObject({
      expectedIdentities: 45,
      observedSamples: 45,
      matched: 45,
    })
    expect(scheduledAggregate.evidenceOutcome).toBe('complete')
  })

  it('reports complete evidence only for all 45 matched scheduled identities', () => {
    const runs = completeRuns()
    const aggregate = aggregateNetworkMatrix(registry, runs)
    expect(aggregate).toMatchObject({
      reportingSemantics: 'scheduled-hard-fail-closed',
      counts: {
        expectedIdentities: 45,
        observedSamples: 45,
        matched: 45,
        mismatched: 0,
        notEvaluated: 0,
        sampleInfrastructureFailures: 0,
      },
      evidenceOutcome: 'complete',
    })
    expect(Object.hasOwn(aggregate, 'verdict')).toBe(false)
  })

  it('fails closed on a candidate mismatch while preserving its exact derived count', () => {
    const scheduled = makeRun(registry, 'scheduled', {
      runId: 'scheduled-mismatch-run',
      sampleMutator: applyLegitimateMismatch,
    })
    const aggregate = aggregateNetworkMatrix(registry, [scheduled])
    expect(aggregate.evidenceOutcome).toBe('incomplete')
    expect(aggregate.counts).toMatchObject({ matched: 44, mismatched: 1 })
  })

  it('distinguishes incomplete prerequisites from sample and orchestration infrastructure failure', () => {
    const incomplete = aggregateNetworkMatrix(registry, [
      makeRun(registry, 'scheduled', {
        runId: 'scheduled-unavailable-run',
        prerequisiteOutcomes: { 'scheduled-coturn': 'unavailable' },
      }),
    ])
    expect(incomplete.evidenceOutcome).toBe('incomplete')
    expect(incomplete.counts.observedSamples).toBe(30)

    const sampleFailure = aggregateNetworkMatrix(registry, [
      makeRun(registry, 'scheduled', {
        runId: 'scheduled-sample-failure-run',
        sampleMutator: applySampleInfrastructureFailure,
      }),
    ])
    expect(sampleFailure.evidenceOutcome).toBe('infrastructure-failed')
    expect(sampleFailure.counts).toMatchObject({
      observedSamples: 45,
      notEvaluated: 1,
      sampleInfrastructureFailures: 1,
    })

    const orchestrationFailure = aggregateNetworkMatrix(registry, [
      makeRun(registry, 'scheduled', {
        runId: 'scheduled-orchestration-failure-run',
        orchestrationOutcome: 'failed',
        sampleFilter: ({ sampleOrdinal }) => sampleOrdinal === 1,
      }),
    ])
    expect(orchestrationFailure.evidenceOutcome).toBe('infrastructure-failed')
    expect(orchestrationFailure.counts.observedSamples).toBe(9)
  })

  it('canonicalizes real run order and rejects absent, duplicate, forged, or alternate inputs', () => {
    const runs = completeRuns()
    const aggregate = aggregateNetworkMatrix(registry, runs)
    const forged = cloneJson(aggregate)
    forged.counts.matched = 59
    expect(() => parseNetworkMatrixAggregate(forged, registry, runs)).toThrow(/matched count/u)
    expect(() => aggregateNetworkMatrix(registry, [])).toThrow(/exactly one real/u)

    const duplicateModeRuns = [
      runs[0]!,
      makeRun(registry, 'scheduled', { runId: 'second-scheduled-run' }),
    ] as const
    expect(() => aggregateNetworkMatrix(registry, duplicateModeRuns)).toThrow(/exactly one real/u)

    const encoded = canonicalNetworkMatrixAggregateJson(aggregate, registry, runs)
    expect(parseNetworkMatrixAggregateJson(encoded, registry, runs)).toEqual(aggregate)
    expect(() => parseNetworkMatrixAggregateJson(`${encoded} `, registry, runs))
      .toThrow(/canonical minified JSON/u)
  })
})

function completeRuns() {
  return Object.freeze([
    makeRun(registry, 'scheduled', { runId: 'scheduled-aggregate-run' }),
  ] as const)
}

function isFirstPublicSample(identity: {
  readonly profileId: string
  readonly browser: string
  readonly sampleOrdinal: number
}): boolean {
  return identity.profileId === 'scheduled-public-stun' &&
    identity.browser === 'chromium' && identity.sampleOrdinal === 1
}

function mismatchingCandidatePath() {
  return Object.freeze({
    selectedPair: 'present',
    localCandidateType: 'relay',
    localAddress: TEST_BROWSER_PUBLIC_IP,
    localPort: 50_000,
    remoteCandidateType: 'relay',
    remoteAddress: TEST_REMOTE_PEER_PUBLIC_IP,
    remotePort: 40_000,
    protocol: 'udp',
  })
}

function applyLegitimateMismatch(
  sample: Record<string, unknown>,
  identity: { readonly profileId: string; readonly browser: string; readonly sampleOrdinal: number },
): void {
  if (!isFirstPublicSample(identity)) return
  replaceBrowserSelectedPair(sample, mismatchingCandidatePath())
  sample.candidatePolicyOutcome = 'mismatched'
  sample.rationaleCodes = [
    'local-candidate-type-forbidden',
    'local-candidate-type-required-missing',
    'remote-candidate-type-forbidden',
  ]
}

function applySampleInfrastructureFailure(
  sample: Record<string, unknown>,
  identity: { readonly profileId: string; readonly browser: string; readonly sampleOrdinal: number },
): void {
  if (!isFirstPublicSample(identity)) return
  sample.sampleOutcome = 'infrastructure-failed'
  sample.processInstanceId = null
  sample.attemptEvidence = null
  sample.candidatePolicyOutcome = 'not-evaluated'
  sample.rationaleCodes = []
  sample.failure = { failureCode: 'evidence-collection-failed' }
}

function replaceBrowserSelectedPair(
  sample: Record<string, unknown>,
  browserSelectedPair: ReturnType<typeof mismatchingCandidatePath>,
): void {
  const attemptEvidence = sample.attemptEvidence as Record<string, unknown>
  attemptEvidence.browserSelectedPair = browserSelectedPair
}

function firstSampleEvidence(run: Record<string, unknown>): Record<string, unknown> {
  const samples = run.samples as Record<string, unknown>[]
  return samples[0]!.attemptEvidence as Record<string, unknown>
}
