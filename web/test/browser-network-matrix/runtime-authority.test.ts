import { describe, expect, it, vi } from 'vitest'

import type { LoadedNetworkMatrixRegistry } from '../../scripts/browser-network-matrix/manifest.ts'
import { completedOwnedOperation } from '../../scripts/browser-network-matrix/owned-operation.ts'
import {
  InjectedNetworkMatrixAuthorityResolver,
  type ExternalFixtureTrustInspectionResult,
  type ExternalFixtureTrustInspector,
  type NetworkMatrixAuthorityPreparationContext,
  type NetworkMatrixExternalFixtureInput,
  type NetworkMatrixRuntimeInputs,
} from '../../scripts/browser-network-matrix/runtime-authority.ts'
import type { NetworkMatrixProfileId } from '../../scripts/browser-network-matrix/vocabulary.ts'
import { loadRegistry } from './fixtures.ts'
import {
  TEST_FIXTURE_ATTESTATION_PUBLIC_KEY_SHA256,
  testExternalFixtureTrustProof,
} from './signed-fixture.ts'

describe('external browser network matrix authorities', () => {
  it.each([
    'scheduled-public-stun',
    'scheduled-restricted-udp',
    'scheduled-coturn',
    'manual-real-nat',
  ] as const)('prepares %s from pinned local trust without inventing a sample authority', async (
    profileId,
  ) => {
    const registry = await loadRegistry()
    const inspector = satisfiedInspector(profileId)
    const context = authorityContext(registry, profileId)
    const prepared = await makeResolver(runtimeInputs(), inspector).prepare(
      context,
    ).result

    expect(inspector.inspect).toHaveBeenCalledWith({
      profileId,
      expectedAttestationPublicKeySha256: TEST_FIXTURE_ATTESTATION_PUBLIC_KEY_SHA256,
      signal: context.signal,
    })
    expect(prepared.attestation).toMatchObject({
      prerequisiteOutcome: 'satisfied',
      proof: {
        proofKind: 'external-fixture-trust',
        externalFixtureTrust: testExternalFixtureTrustProof(),
      },
    })
    expect(prepared.execution).toEqual({
      runtimeKind: 'external-fixture',
      profileId,
    })
  })

  it('does not inspect or schedule an authority that was not provisioned', async () => {
    const registry = await loadRegistry()
    const inspector = satisfiedInspector('scheduled-restricted-udp')
    const inputs = runtimeInputs()
    const { 'scheduled-restricted-udp': omitted, ...provisionedFixtures } =
      inputs.externalFixtures
    expect(omitted).toEqual({ profileId: 'scheduled-restricted-udp' })
    const prepared = await makeResolver({
      externalFixtures: Object.freeze(provisionedFixtures),
    }, inspector).prepare(authorityContext(registry, 'scheduled-restricted-udp')).result

    expect(prepared.attestation).toMatchObject({
      prerequisiteOutcome: 'unavailable',
      proof: null,
      failure: { failureKind: 'unavailable', failureCode: 'authority-not-provisioned' },
    })
    expect(prepared.execution).toBeNull()
    expect(inspector.inspect).not.toHaveBeenCalled()
  })

  it('rejects a trust inspection attributed to a different profile', async () => {
    const registry = await loadRegistry()
    const inspector = satisfiedInspector('scheduled-restricted-udp')
    const prepared = await makeResolver(runtimeInputs(), inspector).prepare(
      authorityContext(registry, 'scheduled-public-stun'),
    ).result

    expect(prepared.attestation).toMatchObject({
      prerequisiteOutcome: 'invalid',
      failure: { failureCode: 'proof-invalid' },
    })
    expect(prepared.execution).toBeNull()
  })

  it('rejects a malformed fixture input before inspecting local trust', async () => {
    const registry = await loadRegistry()
    const inspector = satisfiedInspector('scheduled-public-stun')
    const inputs = runtimeInputs()
    const prepared = await makeResolver({
      externalFixtures: Object.freeze({
        ...inputs.externalFixtures,
        'scheduled-public-stun': Object.freeze({
          profileId: 'scheduled-public-stun',
          staleLiveBinding: 'must-not-be-accepted',
        }) as unknown as NetworkMatrixExternalFixtureInput,
      }),
    }, inspector).prepare(authorityContext(registry, 'scheduled-public-stun')).result

    expect(prepared.attestation).toMatchObject({
      prerequisiteOutcome: 'invalid',
      failure: { failureCode: 'proof-invalid' },
    })
    expect(inspector.inspect).not.toHaveBeenCalled()
  })

  it.each([
    [{ outcome: 'unavailable', failureCode: 'authority-not-provisioned' }, 'unavailable'],
    [{ outcome: 'invalid', failureCode: 'proof-invalid' }, 'invalid'],
    [{ outcome: 'failed', failureCode: 'runtime-check-failed' }, 'failed'],
  ] as const)('propagates a %s local trust verdict', async (inspection, expectedOutcome) => {
    const registry = await loadRegistry()
    const inspector = fixedInspector(inspection)
    const prepared = await makeResolver(runtimeInputs(), inspector).prepare(
      authorityContext(registry, 'scheduled-public-stun'),
    ).result

    expect(prepared.attestation).toMatchObject({
      prerequisiteOutcome: expectedOutcome,
      proof: null,
      failure: {
        failureKind: expectedOutcome,
        failureCode: inspection.failureCode,
      },
    })
    expect(prepared.execution).toBeNull()
  })

  it('rejects local trust that cannot satisfy the runtime attestation contract', async () => {
    const registry = await loadRegistry()
    const inspector = satisfiedInspector('scheduled-public-stun', {
      trust: {
        ...testExternalFixtureTrustProof(),
        controllerOrigin: 'http://fixture.example.test/',
      },
    })
    const prepared = await makeResolver(runtimeInputs(), inspector).prepare(
      authorityContext(registry, 'scheduled-public-stun'),
    ).result

    expect(prepared.attestation).toMatchObject({
      prerequisiteOutcome: 'invalid',
      failure: { failureCode: 'proof-invalid' },
    })
    expect(prepared.execution).toBeNull()
  })

  it('forcibly settles a rejected trust inspection before publishing a failed prerequisite', async () => {
    const registry = await loadRegistry()
    const forceTerminateAndWait = vi.fn().mockResolvedValue(undefined)
    const inspector: ExternalFixtureTrustInspector = {
      inspect: () => ({
        result: Promise.reject(new Error('trust inspection rejected after acquisition')),
        forceTerminateAndWait,
      }),
    }

    const prepared = await makeResolver(runtimeInputs(), inspector).prepare(
      authorityContext(registry, 'scheduled-coturn'),
    ).result

    expect(forceTerminateAndWait).toHaveBeenCalledWith('authority-prepare')
    expect(prepared.attestation).toMatchObject({
      prerequisiteOutcome: 'failed',
      failure: { failureCode: 'runtime-check-failed' },
    })
  })

  it('does not hide failed trust cleanup behind a prerequisite verdict', async () => {
    const registry = await loadRegistry()
    const inspector: ExternalFixtureTrustInspector = {
      inspect: () => ({
        result: Promise.reject(new Error('trust inspection rejected')),
        forceTerminateAndWait: vi.fn().mockRejectedValue(new Error('inspection remained active')),
      }),
    }

    await expect(makeResolver(runtimeInputs(), inspector).prepare(
      authorityContext(registry, 'scheduled-public-stun'),
    ).result).rejects.toMatchObject({
      name: 'NetworkMatrixOwnershipCleanupError',
      operationClass: 'authority-prepare',
    })
  })
})

function makeResolver(
  inputs: NetworkMatrixRuntimeInputs,
  externalFixtureTrust: ExternalFixtureTrustInspector,
): InjectedNetworkMatrixAuthorityResolver {
  return new InjectedNetworkMatrixAuthorityResolver({ inputs, externalFixtureTrust })
}

function runtimeInputs(): NetworkMatrixRuntimeInputs {
  return Object.freeze({
    externalFixtures: Object.freeze({
      'scheduled-public-stun': Object.freeze({ profileId: 'scheduled-public-stun' }),
      'scheduled-restricted-udp': Object.freeze({ profileId: 'scheduled-restricted-udp' }),
      'scheduled-coturn': Object.freeze({ profileId: 'scheduled-coturn' }),
      'manual-real-nat': Object.freeze({ profileId: 'manual-real-nat' }),
    }),
  })
}

function satisfiedInspector(
  profileId: NetworkMatrixProfileId,
  override: Partial<Extract<ExternalFixtureTrustInspectionResult, { outcome: 'satisfied' }>> = {},
): ExternalFixtureTrustInspector & { readonly inspect: ReturnType<typeof vi.fn> } {
  return {
    inspect: vi.fn().mockImplementation(() => completedOwnedOperation({
      outcome: 'satisfied' as const,
      profileId,
      trust: testExternalFixtureTrustProof(),
      ...override,
    })),
  }
}

function fixedInspector(
  result: ExternalFixtureTrustInspectionResult,
): ExternalFixtureTrustInspector {
  return {
    inspect: () => completedOwnedOperation(result),
  }
}

function authorityContext(
  registry: LoadedNetworkMatrixRegistry,
  profileId: NetworkMatrixProfileId,
): NetworkMatrixAuthorityPreparationContext {
  const reference = registry.manifest.profiles.find((profile) => profile.profileId === profileId)
  const profile = registry.profiles.find((profile) => profile.profileId === profileId)
  if (reference === undefined || profile === undefined) throw new Error(`missing profile ${profileId}`)
  return {
    registry,
    reference,
    profile,
    runId: 'authority-runtime-run',
    signal: new AbortController().signal,
  }
}
