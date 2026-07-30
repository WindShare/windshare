import { describe, expect, it, vi } from 'vitest'

import { parseNetworkRuntimeAttestation } from '../../scripts/browser-network-matrix/attestation.ts'
import type { NetworkMatrixAttemptEvidence } from '../../scripts/browser-network-matrix/attempt-evidence.ts'
import { networkMatrixIdentities, type NetworkMatrixIdentity } from '../../scripts/browser-network-matrix/manifest.ts'
import { completedOwnedOperation } from '../../scripts/browser-network-matrix/owned-operation.ts'
import { NetworkMatrixRunCollector } from '../../scripts/browser-network-matrix/run-collector.ts'
import {
  NetworkMatrixOrchestrationError,
  NetworkMatrixSampleExecutionError,
  runNetworkMatrix,
  type NetworkMatrixRunCollectorPort,
  type NetworkMatrixRunTrace,
  type NetworkMatrixSampleExecutor,
} from '../../scripts/browser-network-matrix/runner.ts'
import type {
  NetworkMatrixAuthorityPreparationContext,
  NetworkMatrixAuthorityResolver,
  PreparedNetworkMatrixAuthority,
} from '../../scripts/browser-network-matrix/runtime-authority.ts'
import type {
  NetworkMatrixPrerequisiteOutcome,
  NetworkMatrixProfileId,
} from '../../scripts/browser-network-matrix/vocabulary.ts'
import { loadRegistry, matchedAttemptEvidence, rawAttestation } from './fixtures.ts'

describe('browser network matrix runner', () => {
  it('executes the exact scheduled topology × browser × five identity expansion', async () => {
    const registry = await loadRegistry()
    const resolver = fakeResolver()
    const observed: NetworkMatrixIdentity[] = []
    const traces: NetworkMatrixRunTrace[] = []
    const samples = sampleExecutor((identity, call, runId) => {
      observed.push(identity)
      return successfulExecution(identity, call, runId)
    })

    const result = await runNetworkMatrix({
      registry,
      runId: 'scheduled-runner-run',
      executionMode: 'scheduled',
      authorities: resolver.authorities,
      samples,
      trace: (trace) => traces.push(trace),
    })

    expect(observed).toEqual(networkMatrixIdentities(registry.manifest, 'scheduled'))
    expect(result.samples).toHaveLength(45)
    expect(result.profileResults.map((profile) => profile.observedSamples)).toEqual([15, 15, 15])
    expect(result.profileResults.map((profile) => profile.profileOutcome)).toEqual([
      'completed',
      'completed',
      'completed',
    ])
    expect(result.runOutcome).toBe('completed')
    expect(traces.filter(({ milestone }) => milestone === 'sample-started')).toHaveLength(45)
    expect(traces.find(({ milestone }) => milestone === 'sample-started')).toMatchObject({
      profileId: 'scheduled-public-stun',
      browser: 'chromium',
      sampleOrdinal: 1,
    })
    expect([...resolver.closes.values()].every((close) => close.mock.calls.length === 1)).toBe(true)
  })

  it('emits an unavailable topology attestation and zero samples for that whole topology', async () => {
    const registry = await loadRegistry()
    const resolver = fakeResolver({
      'scheduled-public-stun': 'unavailable',
      'scheduled-coturn': 'unavailable',
    })
    const samples = sampleExecutor((identity, call, runId) =>
      successfulExecution(identity, call, runId))

    const result = await runNetworkMatrix({
      registry,
      runId: 'partial-runner-run',
      executionMode: 'scheduled',
      authorities: resolver.authorities,
      samples,
      trace: () => undefined,
    })

    expect(result.runOutcome).toBe('partial')
    expect(result.samples).toHaveLength(15)
    expect(new Set(result.samples.map(({ identity }) => identity.profileId))).toEqual(
      new Set(['scheduled-restricted-udp']),
    )
    expect(result.profileResults).toMatchObject([
      { prerequisiteOutcome: 'unavailable', observedSamples: 0, profileOutcome: 'not-executed' },
      { prerequisiteOutcome: 'satisfied', observedSamples: 15, profileOutcome: 'completed' },
      { prerequisiteOutcome: 'unavailable', observedSamples: 0, profileOutcome: 'not-executed' },
    ])
    expect(samples.execute).toHaveBeenCalledTimes(15)
  })

  it('reports not-executed without inventing samples when every prerequisite is unsatisfied', async () => {
    const registry = await loadRegistry()
    const resolver = fakeResolver({
      'scheduled-public-stun': 'unavailable',
      'scheduled-restricted-udp': 'failed',
      'scheduled-coturn': 'invalid',
    })
    const samples = sampleExecutor((identity, call, runId) =>
      successfulExecution(identity, call, runId))

    const result = await runNetworkMatrix({
      registry,
      runId: 'not-executed-run',
      executionMode: 'scheduled',
      authorities: resolver.authorities,
      samples,
      trace: () => undefined,
    })

    expect(result.runOutcome).toBe('not-executed')
    expect(result.samples).toEqual([])
    expect(samples.execute).not.toHaveBeenCalled()
  })

  it('keeps all real sample attempts and elevates an individual infrastructure failure', async () => {
    const registry = await loadRegistry()
    const resolver = fakeResolver()
    const samples = sampleExecutor((identity, call, runId) => {
      if (call === 1) {
        throw new NetworkMatrixSampleExecutionError(
          'sample-deadline-exceeded',
          'fake named sample deadline',
        )
      }
      return successfulExecution(identity, call, runId)
    })

    const result = await runNetworkMatrix({
      registry,
      runId: 'sample-infrastructure-run',
      executionMode: 'scheduled',
      authorities: resolver.authorities,
      samples,
      trace: () => undefined,
    })

    expect(samples.execute).toHaveBeenCalledTimes(45)
    expect(result.samples).toHaveLength(45)
    expect(result.samples[0]).toMatchObject({
      sampleOutcome: 'infrastructure-failed',
      processInstanceId: null,
      attemptEvidence: null,
      failure: { failureCode: 'sample-deadline-exceeded' },
    })
    expect(result.runOutcome).toBe('infrastructure-failed')
    expect(result.profileResults[0]).toMatchObject({
      sampleInfrastructureFailures: 1,
      profileOutcome: 'infrastructure-failed',
    })
  })

  it('rejects browser process reuse as sample infrastructure evidence', async () => {
    const registry = await loadRegistry()
    const resolver = fakeResolver()
    const samples = sampleExecutor((identity, call, runId) => successfulExecution(
      identity,
      call,
      runId,
      call <= 2 ? 'reused-browser-process' : `browser-process-${call}`,
    ))

    const result = await runNetworkMatrix({
      registry,
      runId: 'process-isolation-run',
      executionMode: 'scheduled',
      authorities: resolver.authorities,
      samples,
      trace: () => undefined,
    })

    expect(result.samples).toHaveLength(45)
    expect(result.samples[1]).toMatchObject({
      sampleOutcome: 'infrastructure-failed',
      failure: { failureCode: 'sample-runner-failed' },
    })
  })

  it('contains reused attempt and challenge authorities without poisoning later samples', async () => {
    const registry = await loadRegistry()
    const resolver = fakeResolver()
    let firstAttemptId = ''
    let thirdChallengeBinding = ''
    const samples = sampleExecutor((identity, call, runId) => {
      const execution = successfulExecution(identity, call, runId)
      const evidence = execution.observation.attemptEvidence
      if (evidence.challenge === null) throw new Error('public fixture lacks challenge proof')
      if (call === 1) firstAttemptId = evidence.attemptAuthority.attemptId
      if (call === 3) thirdChallengeBinding = evidence.challenge.bindingSha256
      let attemptEvidence: NetworkMatrixAttemptEvidence = evidence
      if (call === 2) {
        attemptEvidence = Object.freeze({
          ...evidence,
          attemptAuthority: Object.freeze({
            ...evidence.attemptAuthority,
            attemptId: firstAttemptId,
          }),
        })
      } else if (call === 4) {
        attemptEvidence = Object.freeze({
          ...evidence,
          challenge: Object.freeze({
            ...evidence.challenge,
            bindingSha256: thirdChallengeBinding,
          }),
        })
      }
      return {
        ...execution,
        observation: Object.freeze({ sampleOutcome: 'observed' as const, attemptEvidence }),
      }
    })

    const result = await runNetworkMatrix({
      registry,
      runId: 'attempt-authority-isolation-run',
      executionMode: 'scheduled',
      authorities: resolver.authorities,
      samples,
      trace: () => undefined,
    })

    expect(result.samples[1]).toMatchObject({
      sampleOutcome: 'infrastructure-failed',
      processInstanceId: null,
      attemptEvidence: null,
      failure: { failureCode: 'evidence-collection-failed' },
    })
    expect(result.samples[3]).toMatchObject({
      sampleOutcome: 'infrastructure-failed',
      failure: { failureCode: 'evidence-collection-failed' },
    })
    expect(result.samples[2]?.sampleOutcome).toBe('observed')
    expect(result.samples[4]?.sampleOutcome).toBe('observed')
  })

  it('turns malformed candidate evidence into evidence-collection failure without aborting peers', async () => {
    const registry = await loadRegistry()
    const resolver = fakeResolver()
    const samples = sampleExecutor((identity, call, runId) => call === 3
      ? {
          processInstanceId: `browser-process-${call}`,
          observation: {
            sampleOutcome: 'observed',
            attemptEvidence: {
              ...matchedAttemptEvidence(identity, runId),
              browserSelectedPair: {
                selectedPair: 'present',
                localCandidateType: 'fabricated',
                remoteCandidateType: 'host',
                protocol: 'udp',
              },
            } as unknown as NetworkMatrixAttemptEvidence,
          },
        }
      : successfulExecution(identity, call, runId))

    const result = await runNetworkMatrix({
      registry,
      runId: 'malformed-evidence-run',
      executionMode: 'scheduled',
      authorities: resolver.authorities,
      samples,
      trace: () => undefined,
    })

    expect(samples.execute).toHaveBeenCalledTimes(45)
    expect(result.samples[2]).toMatchObject({
      sampleOutcome: 'infrastructure-failed',
      failure: { failureCode: 'evidence-collection-failed' },
    })
  })

  it('preserves the collected subset when orchestration terminates', async () => {
    const registry = await loadRegistry()
    const resolver = fakeResolver()
    const samples = sampleExecutor((identity, call, runId) => {
      if (call === 17) {
        throw new NetworkMatrixOrchestrationError(
          'orchestrator-deadline-exceeded',
          'fake orchestration deadline',
        )
      }
      return successfulExecution(identity, call, runId)
    })

    const result = await runNetworkMatrix({
      registry,
      runId: 'orchestration-failure-run',
      executionMode: 'scheduled',
      authorities: resolver.authorities,
      samples,
      trace: () => undefined,
    })

    expect(result.samples).toHaveLength(16)
    expect(result.orchestrationFailure?.failureCode).toBe('orchestrator-deadline-exceeded')
    expect(result.runOutcome).toBe('infrastructure-failed')
    expect(result.profileResults.map(({ profileOutcome }) => profileOutcome)).toEqual([
      'completed',
      'infrastructure-failed',
      'not-executed',
    ])
    expect([...resolver.closes.values()].every((close) => close.mock.calls.length === 1)).toBe(true)
  })

  it('contains a throwing authority resolver as a failed whole-profile prerequisite', async () => {
    const registry = await loadRegistry()
    const resolver = fakeResolver({}, new Set(['scheduled-public-stun']))
    const samples = sampleExecutor((identity, call, runId) =>
      successfulExecution(identity, call, runId))

    const result = await runNetworkMatrix({
      registry,
      runId: 'authority-throw-run',
      executionMode: 'scheduled',
      authorities: resolver.authorities,
      samples,
      trace: () => undefined,
    })

    expect(result.runtimeAttestations[0]).toMatchObject({
      prerequisiteOutcome: 'failed',
      failure: { failureCode: 'runtime-check-failed' },
    })
    expect(result.samples).toHaveLength(30)
    expect(result.runOutcome).toBe('partial')
  })

  it('elevates lost preparation containment proof and still finalizes a canonical ledger', async () => {
    const registry = await loadRegistry()
    const fallback = fakeResolver()
    let preparations = 0
    const authorities: NetworkMatrixAuthorityResolver = {
      prepare(context) {
        preparations += 1
        if (preparations !== 1) return fallback.authorities.prepare(context)
        return {
          result: Promise.reject(new Error('fixture rejected after launch')),
          forceTerminateAndWait: vi.fn().mockRejectedValue(new Error('fixture was not reaped')),
        }
      },
    }
    const samples = sampleExecutor((identity, call, runId) =>
      successfulExecution(identity, call, runId))

    const result = await runNetworkMatrix({
      registry,
      runId: 'prepare-containment-loss-run',
      executionMode: 'scheduled',
      authorities,
      samples,
      trace: () => undefined,
    })

    expect(result).toMatchObject({
      orchestrationOutcome: 'failed',
      orchestrationFailure: { failureCode: 'containment-cleanup-failed' },
      runOutcome: 'infrastructure-failed',
    })
    expect(result.samples).toEqual([])
    expect(result.runtimeAttestations[0]?.prerequisiteOutcome).toBe('failed')
    expect(samples.execute).not.toHaveBeenCalled()
  })

  it('elevates lost sample containment proof while preserving the real subset ledger', async () => {
    const registry = await loadRegistry()
    const resolver = fakeResolver()
    let calls = 0
    const samples: NetworkMatrixSampleExecutor = {
      execute(context) {
        calls += 1
        if (calls === 3) {
          return {
            result: Promise.reject(new Error('browser process rejected')),
            forceTerminateAndWait: vi.fn().mockRejectedValue(new Error('browser subtree not reaped')),
          }
        }
        return completedOwnedOperation(successfulExecution(context.identity, calls, context.runId))
      },
    }

    const result = await runNetworkMatrix({
      registry,
      runId: 'sample-containment-loss-run',
      executionMode: 'scheduled',
      authorities: resolver.authorities,
      samples,
      trace: () => undefined,
    })

    expect(result.samples).toHaveLength(2)
    expect(result).toMatchObject({
      orchestrationOutcome: 'failed',
      orchestrationFailure: { failureCode: 'containment-cleanup-failed' },
      runOutcome: 'infrastructure-failed',
    })
    expect(result.profileResults[0]).toMatchObject({
      observedSamples: 2,
      profileOutcome: 'infrastructure-failed',
    })
  })
})

describe('browser network matrix runner lifecycle containment', () => {
  it('reaps each profile authority before preparing the next profile', async () => {
    const registry = await loadRegistry()
    const resolver = fakeResolver()
    const lifecycle: string[] = []
    const authorities: NetworkMatrixAuthorityResolver = {
      prepare(context) {
        const profileId = context.profile.profileId
        lifecycle.push(`prepare:${profileId}`)
        const preparation = resolver.authorities.prepare(context)
        return {
          result: preparation.result.then((prepared) => ({
            ...prepared,
            close() {
              const close = prepared.close()
              return {
                result: close.result.then(() => {
                  lifecycle.push(`closed:${profileId}`)
                }),
                forceTerminateAndWait: close.forceTerminateAndWait,
              }
            },
          })),
          forceTerminateAndWait: preparation.forceTerminateAndWait,
        }
      },
    }

    await runNetworkMatrix({
      registry,
      runId: 'sequential-authority-lifecycle-run',
      executionMode: 'scheduled',
      authorities,
      samples: sampleExecutor((identity, call, runId) =>
        successfulExecution(identity, call, runId)),
      trace: () => undefined,
    })

    expect(lifecycle).toEqual([
      'prepare:scheduled-public-stun',
      'closed:scheduled-public-stun',
      'prepare:scheduled-restricted-udp',
      'closed:scheduled-restricted-udp',
      'prepare:scheduled-coturn',
      'closed:scheduled-coturn',
    ])
  })

  it('closes a just-acquired authority when collector attestation storage throws', async () => {
    const registry = await loadRegistry()
    const resolver = fakeResolver()
    const backingCollector = new NetworkMatrixRunCollector({
      registry,
      runId: 'collector-throw-cleanup-run',
      executionMode: 'scheduled',
    })
    const collector: NetworkMatrixRunCollectorPort = {
      recordAttestation: vi.fn().mockImplementation(() => {
        throw new Error('collector storage failed')
      }),
      recordSample: (identity, processInstanceId, observation) =>
        backingCollector.recordSample(identity, processInstanceId, observation),
      finalize: (failure) => backingCollector.finalize(failure),
    }

    await expect(runNetworkMatrix({
      registry,
      runId: 'collector-throw-cleanup-run',
      executionMode: 'scheduled',
      authorities: resolver.authorities,
      samples: sampleExecutor((identity, call, runId) =>
        successfulExecution(identity, call, runId)),
      collector,
      trace: () => undefined,
    })).rejects.toThrow('collector storage failed')

    expect(resolver.closes.get('scheduled-public-stun')).toHaveBeenCalledOnce()
    expect(resolver.closes.size).toBe(1)
  })

  it('awaits the retained fallback and reports containment failure when close cannot start', async () => {
    const registry = await loadRegistry()
    const resolver = fakeResolver()
    let signalFallbackStarted = (): void => undefined
    let releaseFallback = (): void => undefined
    const fallbackStarted = new Promise<void>((resolve) => {
      signalFallbackStarted = resolve
    })
    const fallbackReleased = new Promise<void>((resolve) => {
      releaseFallback = resolve
    })
    const retainedFallback = vi.fn().mockImplementation(() => {
      signalFallbackStarted()
      return fallbackReleased
    })
    const authorities: NetworkMatrixAuthorityResolver = {
      prepare(context) {
        const preparation = resolver.authorities.prepare(context)
        return {
          result: preparation.result.then((prepared) => context.profile.profileId === 'scheduled-public-stun'
            ? {
                ...prepared,
                close(): never {
                  throw new Error('close factory failed synchronously')
                },
                forceTerminateAndWait: retainedFallback,
              }
            : prepared),
          forceTerminateAndWait: preparation.forceTerminateAndWait,
        }
      },
    }
    const run = runNetworkMatrix({
      registry,
      runId: 'sync-close-fallback-run',
      executionMode: 'scheduled',
      authorities,
      samples: sampleExecutor((identity, call, runId) =>
        successfulExecution(identity, call, runId)),
      trace: () => undefined,
    })

    await fallbackStarted
    let runSettled = false
    const settlementObservation = run.then(
      () => { runSettled = true },
      () => { runSettled = true },
    )
    await Promise.resolve()
    expect(runSettled).toBe(false)
    releaseFallback()
    const result = await run
    await settlementObservation

    expect(retainedFallback).toHaveBeenCalledWith('authority-close')
    expect(result).toMatchObject({
      orchestrationOutcome: 'failed',
      orchestrationFailure: { failureCode: 'containment-cleanup-failed' },
      runOutcome: 'infrastructure-failed',
    })
    expect(result.samples).toHaveLength(15)
  })

  it('settles invalid preparation through the producer owner before its value can escape', async () => {
    const registry = await loadRegistry()
    const resolver = fakeResolver()
    const invalidValueFallback = vi.fn().mockResolvedValue(undefined)
    const producerFallback = vi.fn().mockResolvedValue(undefined)
    const authorities: NetworkMatrixAuthorityResolver = {
      prepare(context) {
        const preparation = resolver.authorities.prepare(context)
        return {
          result: preparation.result.then((prepared) => context.profile.profileId === 'scheduled-public-stun'
            ? {
                ...prepared,
                close: undefined,
                forceTerminateAndWait: invalidValueFallback,
              } as unknown as PreparedNetworkMatrixAuthority
            : prepared),
          forceTerminateAndWait: producerFallback,
        }
      },
    }

    const result = await runNetworkMatrix({
      registry,
      runId: 'invalid-preparation-retained-fallback-run',
      executionMode: 'scheduled',
      authorities,
      samples: sampleExecutor((identity, call, runId) =>
        successfulExecution(identity, call, runId)),
      trace: () => undefined,
    })

    expect(producerFallback).toHaveBeenCalledWith('authority-prepare')
    expect(invalidValueFallback).not.toHaveBeenCalled()
    expect(result.runtimeAttestations[0]).toMatchObject({
      prerequisiteOutcome: 'failed',
      failure: { failureCode: 'runtime-check-failed' },
    })
    expect(result.samples).toHaveLength(30)
  })

  it('does not fabricate containment loss after the producer settles an invalid value', async () => {
    const registry = await loadRegistry()
    const resolver = fakeResolver()
    const authorities: NetworkMatrixAuthorityResolver = {
      prepare(context) {
        const preparation = resolver.authorities.prepare(context)
        return {
          result: preparation.result.then((prepared) => context.profile.profileId === 'scheduled-public-stun'
            ? {
                ...prepared,
                close: undefined,
                forceTerminateAndWait: undefined,
              } as unknown as PreparedNetworkMatrixAuthority
            : prepared),
          forceTerminateAndWait: preparation.forceTerminateAndWait,
        }
      },
    }

    const result = await runNetworkMatrix({
      registry,
      runId: 'invalid-preparation-containment-run',
      executionMode: 'scheduled',
      authorities,
      samples: sampleExecutor((identity, call, runId) =>
        successfulExecution(identity, call, runId)),
      trace: () => undefined,
    })

    expect(result).toMatchObject({
      orchestrationOutcome: 'healthy',
      orchestrationFailure: null,
      runOutcome: 'partial',
    })
    expect(result.samples).toHaveLength(30)
  })
})

function sampleExecutor(
  run: (
    identity: NetworkMatrixIdentity,
    call: number,
    runId: string,
  ) => ReturnType<typeof successfulExecution>,
): NetworkMatrixSampleExecutor & { readonly execute: ReturnType<typeof vi.fn> } {
  let calls = 0
  return {
    execute: vi.fn().mockImplementation((context: {
      readonly identity: NetworkMatrixIdentity
      readonly runId: string
    }) => {
      calls += 1
      return {
        result: Promise.resolve().then(() => run(context.identity, calls, context.runId)),
        forceTerminateAndWait: () => Promise.resolve(),
      }
    }),
  }
}

function successfulExecution(
  identity: NetworkMatrixIdentity,
  call: number,
  runId: string,
  processInstanceId = `browser-process-${call}`,
) {
  return {
    processInstanceId,
    observation: {
      sampleOutcome: 'observed' as const,
      attemptEvidence: matchedAttemptEvidence(identity, runId, { processInstanceId }),
    },
  }
}

function fakeResolver(
  outcomes: Readonly<Partial<Record<NetworkMatrixProfileId, NetworkMatrixPrerequisiteOutcome>>> = {},
  throwing: ReadonlySet<NetworkMatrixProfileId> = new Set(),
): {
  readonly authorities: NetworkMatrixAuthorityResolver
  readonly closes: ReadonlyMap<NetworkMatrixProfileId, ReturnType<typeof vi.fn>>
} {
  const closes = new Map<NetworkMatrixProfileId, ReturnType<typeof vi.fn>>()
  return {
    authorities: {
      prepare: vi.fn().mockImplementation((context: NetworkMatrixAuthorityPreparationContext) => {
        const profileId = context.profile.profileId
        if (throwing.has(profileId)) throw new Error('fake authority failure')
        const outcome = outcomes[profileId] ?? 'satisfied'
        const close = vi.fn().mockReturnValue(completedOwnedOperation(undefined))
        closes.set(profileId, close)
        const attestation = parseNetworkRuntimeAttestation(
          rawAttestation(context.registry, context.runId, profileId, outcome),
          {
            manifest: context.registry.manifest,
            manifestSha256: context.registry.manifestSha256,
            runId: context.runId,
          },
        )
        const prepared: PreparedNetworkMatrixAuthority = {
          attestation,
          execution: outcome === 'satisfied'
             ? {
                 profileId,
                 runtimeKind: 'external-fixture',
               }
            : null,
          close,
          forceTerminateAndWait: vi.fn().mockResolvedValue(undefined),
        }
        return completedOwnedOperation(prepared)
      }),
    },
    closes,
  }
}
