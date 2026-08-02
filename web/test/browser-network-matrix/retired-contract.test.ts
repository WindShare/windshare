import { describe, expect, it } from 'vitest'
import {
  aggregateNetworkMatrix,
  parseNetworkMatrixAggregate,
} from '../../scripts/browser-network-matrix/aggregate.ts'
import { parseNetworkRuntimeAttestation } from '../../scripts/browser-network-matrix/attestation.ts'
import { parseNetworkMatrixManifest } from '../../scripts/browser-network-matrix/manifest.ts'
import { parseNetworkTopologyProfile } from '../../scripts/browser-network-matrix/profile.ts'
import { parseNetworkRunResult } from '../../scripts/browser-network-matrix/result.ts'
import {
  cloneJson,
  loadRegistry,
  makeRun,
  makeRunRaw,
  rawAttestation,
} from './fixtures.ts'

const RETIRED_EXECUTION_KEY = ['execution', 'Trigger'].join('')
const RETIRED_AUTHORITY_LABEL_KEY = ['required', 'Runner', 'Labels'].join('')
const RETIRED_PROOF_KEY = ['self', 'Hosted', 'Runner'].join('')
const RETIRED_TERMS = Object.freeze([
  RETIRED_EXECUTION_KEY,
  RETIRED_AUTHORITY_LABEL_KEY,
  ['runner', 'Context'].join(''),
  ['scheduler', 'Required', 'Labels'].join(''),
  RETIRED_PROOF_KEY,
  ['manual', 'self', 'hosted', 'real', 'nat'].join('-'),
  ['self', 'hosted', 'runner'].join('-'),
  ['real', 'nat', 'self', 'hosted', 'runner'].join('-'),
  ['runner', 'setup', 'failed'].join('-'),
])

describe('scheduler-agnostic browser network matrix contracts', () => {
  it('emits no retired event, label, context, profile, or proof vocabulary', async () => {
    const registry = await loadRegistry()
    const run = makeRun(registry, 'scheduled')
    const attestation = rawAttestation(registry, run.runId, 'scheduled-public-stun', 'satisfied')
    const aggregate = aggregateNetworkMatrix(registry, [run])
    const canonicalProjection = JSON.stringify({
      manifest: registry.manifest,
      profiles: registry.profiles,
      run,
      aggregate,
      attestation,
    })

    for (const retired of RETIRED_TERMS) expect(canonicalProjection).not.toContain(retired)
  })

  it('rejects the retired execution key in manifest, profile, run, and aggregate objects', async () => {
    const registry = await loadRegistry()
    const manifest = cloneJson(registry.manifest) as unknown as Record<string, unknown>
    replaceExecutionMode(manifestProfile(manifest))
    expect(() => parseNetworkMatrixManifest(manifest)).toThrow(/exactly/u)

    const profile = cloneJson(registry.profiles[0]) as unknown as Record<string, unknown>
    replaceExecutionMode(profile)
    expect(() => parseNetworkTopologyProfile(profile)).toThrow(/exactly/u)

    const run = makeRunRaw(registry, 'scheduled')
    replaceExecutionMode(run)
    expect(() => parseNetworkRunResult(run, registry)).toThrow(/exactly/u)

    const canonicalRun = makeRun(registry, 'scheduled')
    const aggregate = cloneJson(aggregateNetworkMatrix(registry, [canonicalRun])) as unknown as Record<
      string,
      unknown
    >
    const references = aggregate.runs as Record<string, unknown>[]
    replaceExecutionMode(references[0] as Record<string, unknown>)
    expect(() => parseNetworkMatrixAggregate(aggregate, registry, [canonicalRun])).toThrow(/exactly/u)
  })

  it('rejects retired authority-label and proof-union members without aliases', async () => {
    const registry = await loadRegistry()
    const profile = cloneJson(
      registry.profiles.find(({ profileId }) => profileId === 'scheduled-public-stun'),
    ) as unknown as Record<string, unknown>
    const authority = profile.authority as Record<string, unknown>
    authority[RETIRED_AUTHORITY_LABEL_KEY] = []
    expect(() => parseNetworkTopologyProfile(profile)).toThrow(/exactly/u)

    const runId = 'retired-proof-member-run'
    const attestation = rawAttestation(registry, runId, 'scheduled-public-stun', 'satisfied')
    const proof = attestation.proof as Record<string, unknown>
    proof[RETIRED_PROOF_KEY] = {}
    expect(() => parseNetworkRuntimeAttestation(attestation, {
      manifest: registry.manifest,
      manifestSha256: registry.manifestSha256,
      runId,
    })).toThrow(/exactly/u)
  })

  it('rejects the retired platform setup failure code in both failure vocabularies', async () => {
    const registry = await loadRegistry()
    const retiredFailureCode = ['runner', 'setup', 'failed'].join('-')
    const run = makeRunRaw(registry, 'scheduled', { orchestrationOutcome: 'failed' })
    const failure = run.orchestrationFailure as Record<string, unknown>
    failure.failureCode = retiredFailureCode
    expect(() => parseNetworkRunResult(run, registry)).toThrow(/frozen vocabulary/u)

    const prerequisiteRun = makeRunRaw(registry, 'scheduled', {
      prerequisiteOutcomes: { 'scheduled-public-stun': 'failed' },
    })
    const attestations = prerequisiteRun.runtimeAttestations as Record<string, unknown>[]
    const prerequisiteFailure = attestations[0]?.failure as Record<string, unknown>
    prerequisiteFailure.failureCode = retiredFailureCode
    expect(() => parseNetworkRunResult(prerequisiteRun, registry)).toThrow(/frozen vocabulary/u)
  })
})

function replaceExecutionMode(record: Record<string, unknown>): void {
  record[RETIRED_EXECUTION_KEY] = record.executionMode
  delete record.executionMode
}

function manifestProfile(manifest: Record<string, unknown>): Record<string, unknown> {
  const profiles = manifest.profiles as Record<string, unknown>[]
  return profiles[0] as Record<string, unknown>
}
