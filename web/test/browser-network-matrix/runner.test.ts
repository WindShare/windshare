import { describe, expect, it, vi } from 'vitest'

import { parseNetworkRuntimeAttestation } from '../../scripts/browser-network-matrix/attestation.ts'
import type { NetworkMatrixAttemptEvidence } from '../../scripts/browser-network-matrix/attempt-evidence.ts'
import { networkMatrixIdentities, type NetworkMatrixIdentity } from '../../scripts/browser-network-matrix/manifest.ts'
import {
  completedOwnedOperation,
  type NetworkMatrixDeadlineScheduler,
  type NetworkMatrixOwnershipInput,
  type NetworkMatrixOwnershipRegistrar,
  type NetworkMatrixOwnershipRegistration,
} from '../../scripts/browser-network-matrix/owned-operation.ts'
import { NetworkMatrixRunCollector } from '../../scripts/browser-network-matrix/run-collector.ts'
import { networkMatrixSampleOperationId } from '../../scripts/browser-network-matrix/sample-authority.ts'
import {
  NetworkMatrixOrchestrationError,
  NetworkMatrixSampleExecutionError,
  startNetworkMatrix,
  type NetworkMatrixRunCollectorPort,
  type NetworkMatrixRunnerOptions,
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

async function runNetworkMatrix(options: NetworkMatrixRunnerOptions) {
  const execution = startNetworkMatrix(options)
  const result = await execution.result
  const snapshot = execution.traces.snapshot()
  if (
    snapshot.failure !== null ||
    !snapshot.completed ||
    snapshot.truncated ||
    snapshot.observedEvents !== snapshot.capturedEvents ||
    snapshot.observedBytes !== snapshot.capturedBytes
  ) throw new Error('test discarded incomplete network matrix runner trace evidence')
  return result
}

describe('browser network matrix runner', () => {
  it('executes the exact scheduled expansion at the maximum run ID boundary', async () => {
    const registry = await loadRegistry()
    const runId = 'r'.repeat(96)
    const resolver = fakeResolver()
    const ownedOperationIds: string[] = []
    const observed: NetworkMatrixIdentity[] = []
    const samples = sampleExecutor((identity, call, runId) => {
      observed.push(identity)
      return successfulExecution(identity, call, runId)
    })

    const execution = startNetworkMatrix({
      registry,
      runId,
      executionMode: 'scheduled',
      authorities: resolver.authorities,
      samples,
      ownershipRegistrar: recordingOwnershipRegistrar(ownedOperationIds),
    })
    const result = await execution.result
    const traceSnapshot = execution.traces.snapshot()
    expect(traceSnapshot).toMatchObject({
      completed: true,
      truncated: false,
      failure: null,
      observedEvents: traceSnapshot.capturedEvents,
    })
    const traces = traceSnapshot.events

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
      schemaVersion: 'windshare.browser-network-matrix-trace/v1',
      component: 'browser-network-matrix-runner',
      scenario: 'network-matrix-sample',
      outcome: 'started',
      profileId: 'scheduled-public-stun',
      browser: 'chromium',
      sampleOrdinal: 1,
    })
    expect(traces.filter(({ milestone }) => milestone === 'run-started')).toHaveLength(1)
    expect(traces.filter(({ milestone }) => milestone === 'run-terminal')).toHaveLength(1)
    expect(traces.filter(({ milestone }) => milestone === 'profile-started')).toHaveLength(3)
    expect(traces.filter(({ milestone }) => milestone === 'profile-terminal')).toHaveLength(3)
    expect(traces.filter(({ milestone }) => milestone === 'sample-terminal')).toHaveLength(45)
    expect(traces.every(({ operationId }) => Buffer.byteLength(operationId, 'utf8') <= 128)).toBe(true)
    expect(ownedOperationIds).not.toHaveLength(0)
    expect(ownedOperationIds.every((operationId) =>
      Buffer.byteLength(operationId, 'utf8') <= 128 &&
      /^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$/u.test(operationId))).toBe(true)
    expect(traces.find(({ milestone }) => milestone === 'run-terminal')).toMatchObject({
      component: 'browser-network-matrix-runner',
      scenario: 'network-matrix-run',
      outcome: 'succeeded',
      context: { cleanupOutcome: 'completed', lastMilestone: 'run-result-finalized' },
    })
    expect(traces.every(({ schemaVersion, component, scenario, outcome }) =>
      schemaVersion === 'windshare.browser-network-matrix-trace/v1' &&
      component === 'browser-network-matrix-runner' &&
      ['network-matrix-run', 'network-matrix-profile', 'network-matrix-sample'].includes(scenario) &&
      ['started', 'succeeded', 'failed', 'skipped'].includes(outcome))).toBe(true)
    expect([...resolver.closes.values()].every((close) => close.mock.calls.length === 1)).toBe(true)
  })

  it('fails a sample terminal when observed evidence violates the candidate hard oracle', async () => {
    const registry = await loadRegistry()
    const resolver = fakeResolver()
    const samples = sampleExecutor((identity, call, runId) => {
      const execution = successfulExecution(identity, call, runId)
      if (call !== 1) return execution
      return {
        ...execution,
        observation: {
          sampleOutcome: 'observed' as const,
          attemptEvidence: matchedAttemptEvidence(identity, runId, {
            candidatePolicyMismatch: true,
            processInstanceId: execution.processInstanceId,
          }),
        },
      }
    })
    const execution = startNetworkMatrix({
      registry,
      runId: 'candidate-hard-oracle-run',
      executionMode: 'scheduled',
      authorities: resolver.authorities,
      samples,
    })

    const result = await execution.result
    const snapshot = execution.traces.snapshot()
    const firstOperationId = networkMatrixSampleOperationId(
      'candidate-hard-oracle-run',
      networkMatrixIdentities(registry.manifest, 'scheduled')[0]!,
    )
    const firstTerminal = snapshot.events.find(({ operationId, milestone }) =>
      operationId === firstOperationId && milestone === 'sample-terminal')

    expect(result.samples[0]).toMatchObject({
      sampleOutcome: 'observed',
      candidatePolicyOutcome: 'mismatched',
    })
    expect(firstTerminal).toMatchObject({
      outcome: 'failed',
      context: {
        cleanupOutcome: 'completed',
        lastMilestone: 'sample-execution-completed',
        sampleOutcome: 'observed',
        candidatePolicyOutcome: 'mismatched',
      },
    })
    expect(snapshot.failure).toBeNull()
  })

  it('binds a sample deadline to one failed terminal event after successful cleanup', async () => {
    const registry = await loadRegistry()
    const resolver = fakeResolver()
    const execution = startNetworkMatrix({
      registry,
      runId: 'deadline-trace-run',
      executionMode: 'scheduled',
      authorities: resolver.authorities,
      samples: sampleExecutor((identity, call, runId) =>
        successfulExecution(identity, call, runId)),
      deadlineScheduler: secondOperationDeadlineScheduler(),
    })
    const stalledConsumer = execution.traces[Symbol.asyncIterator]()
    expect(await stalledConsumer.next()).toMatchObject({
      done: false,
      value: { milestone: 'run-started' },
    })
    const result = await execution.result
    const traceSnapshot = execution.traces.snapshot()
    expect(traceSnapshot).toMatchObject({
      completed: true,
      truncated: false,
      failure: null,
      observedEvents: traceSnapshot.capturedEvents,
    })
    const traces = traceSnapshot.events

    expect(result.samples[0]).toMatchObject({
      sampleOutcome: 'infrastructure-failed',
      failure: { failureCode: 'sample-deadline-exceeded' },
    })
    const operationId = networkMatrixSampleOperationId(
      'deadline-trace-run',
      networkMatrixIdentities(registry.manifest, 'scheduled')[0]!,
    )
    const lifecycle = traces.filter((trace) => trace.operationId === operationId)
    expect(lifecycle.filter(({ milestone }) => milestone === 'sample-started')).toHaveLength(1)
    expect(lifecycle.filter(({ milestone }) => milestone === 'sample-terminal')).toHaveLength(1)
    expect(lifecycle.find(({ milestone }) => milestone === 'sample-terminal')).toMatchObject({
      outcome: 'failed',
      context: {
        cleanupOutcome: 'completed',
        lastMilestone: 'sample-execution-deadline-exceeded',
        failureCode: 'sample-deadline-exceeded',
      },
    })
  })

  it('preserves the deadline milestone when cleanup also fails', async () => {
    const registry = await loadRegistry()
    const resolver = fakeResolver()
    const forceTerminateAndWait = vi.fn().mockRejectedValue(new Error('subtree remained live'))
    const samples: NetworkMatrixSampleExecutor = {
      execute: vi.fn().mockReturnValue({
        result: new Promise(() => undefined),
        forceTerminateAndWait,
      }),
    }

    const execution = startNetworkMatrix({
      registry,
      runId: 'deadline-cleanup-failure-run',
      executionMode: 'scheduled',
      authorities: resolver.authorities,
      samples,
      deadlineScheduler: secondOperationDeadlineScheduler(),
    })
    const result = await execution.result
    const snapshot = execution.traces.snapshot()
    const operationId = networkMatrixSampleOperationId(
      'deadline-cleanup-failure-run',
      networkMatrixIdentities(registry.manifest, 'scheduled')[0]!,
    )
    const lifecycle = snapshot.events.filter((trace) => trace.operationId === operationId)

    expect(forceTerminateAndWait).toHaveBeenCalledWith('sample-execute')
    expect(result).toMatchObject({
      orchestrationOutcome: 'failed',
      orchestrationFailure: { failureCode: 'containment-cleanup-failed' },
    })
    expect(lifecycle.find(({ milestone }) => milestone === 'sample-cleanup-failed'))
      .toMatchObject({ outcome: 'failed' })
    expect(lifecycle.find(({ milestone }) => milestone === 'sample-terminal')).toMatchObject({
      outcome: 'failed',
      context: {
        cleanupOutcome: 'failed',
        lastMilestone: 'sample-execution-deadline-exceeded',
        failureCode: 'containment-cleanup-failed',
      },
    })
    expect(snapshot).toMatchObject({
      completed: true,
      truncated: false,
      observedEvents: snapshot.capturedEvents,
      observedBytes: snapshot.capturedBytes,
    })
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
})

describe('browser network matrix runner suppression and hostile failures', () => {
  it('reports not-executed without inventing samples when every prerequisite is unsatisfied', async () => {
    const registry = await loadRegistry()
    const resolver = fakeResolver({
      'scheduled-public-stun': 'unavailable',
      'scheduled-restricted-udp': 'failed',
      'scheduled-coturn': 'invalid',
    })
    const samples = sampleExecutor((identity, call, runId) =>
      successfulExecution(identity, call, runId))

    const execution = startNetworkMatrix({
      registry,
      runId: 'not-executed-run',
      executionMode: 'scheduled',
      authorities: resolver.authorities,
      samples,
    })
    const result = await execution.result
    const snapshot = execution.traces.snapshot()
    const sampleStarts = snapshot.events.filter(({ milestone }) => milestone === 'sample-started')
    const sampleSkips = snapshot.events.filter(
      ({ milestone }) => milestone === 'sample-execution-skipped',
    )
    const sampleTerminals = snapshot.events.filter(({ milestone }) => milestone === 'sample-terminal')

    expect(result.runOutcome).toBe('not-executed')
    expect(result.samples).toEqual([])
    expect(samples.execute).not.toHaveBeenCalled()
    expect(sampleStarts).toHaveLength(45)
    expect(sampleSkips).toHaveLength(45)
    expect(sampleTerminals).toHaveLength(45)
    expect(new Set(sampleStarts.map(({ operationId }) => operationId))).toHaveLength(45)
    expect(sampleTerminals.every(({ outcome, context }) =>
      outcome === 'skipped' &&
      context?.cleanupOutcome === 'not-required' &&
      context.lastMilestone === 'sample-execution-skipped')).toBe(true)
    expect(snapshot).toMatchObject({
      completed: true,
      failure: null,
      truncated: false,
      observedEvents: snapshot.capturedEvents,
      observedBytes: snapshot.capturedBytes,
    })
  })

  it.each([
    ['Error accessors', () => {
      let traps = 0
      const failure = new Error('opaque dependency failure')
      Object.defineProperty(failure, 'message', {
        configurable: true,
        get: () => {
          traps += 1
          throw new Error('message getter must remain opaque')
        },
      })
      Object.defineProperty(failure, 'toString', {
        configurable: true,
        value: () => {
          traps += 1
          throw new Error('toString must remain opaque')
        },
      })
      return { failure, observedTraps: () => traps }
    }],
    ['Proxy traps', () => {
      let traps = 0
      const failure = new Proxy(new Error('opaque proxy failure'), {
        get: () => {
          traps += 1
          throw new Error('get trap must remain opaque')
        },
        getPrototypeOf: () => {
          traps += 1
          throw new Error('prototype trap must remain opaque')
        },
      })
      return { failure, observedTraps: () => traps }
    }],
  ])('settles sample cleanup and terminal evidence without inspecting hostile %s', async (
    _label,
    createFailure,
  ) => {
    const registry = await loadRegistry()
    const resolver = fakeResolver()
    const { failure, observedTraps } = createFailure()
    const forceTerminateAndWait = vi.fn().mockResolvedValue(undefined)
    let calls = 0
    const samples: NetworkMatrixSampleExecutor = {
      execute(context) {
        calls += 1
        if (calls === 1) {
          return Object.freeze({
            result: Promise.reject(failure),
            forceTerminateAndWait,
          })
        }
        return completedOwnedOperation(successfulExecution(context.identity, calls, context.runId))
      },
    }
    const execution = startNetworkMatrix({
      registry,
      runId: `hostile-sample-cause-${_label.replaceAll(' ', '-').toLowerCase()}`,
      executionMode: 'scheduled',
      authorities: resolver.authorities,
      samples,
    })

    const result = await execution.result
    const snapshot = execution.traces.snapshot()

    expect(forceTerminateAndWait).toHaveBeenCalledOnce()
    expect(forceTerminateAndWait).toHaveBeenCalledWith('sample-execute')
    expect(result.samples[0]).toMatchObject({
      sampleOutcome: 'infrastructure-failed',
      failure: { failureCode: 'sample-runner-failed' },
    })
    expect(snapshot).toMatchObject({
      completed: true,
      failure: null,
      truncated: false,
      observedEvents: snapshot.capturedEvents,
      observedBytes: snapshot.capturedBytes,
    })
    expect(snapshot.events.find(({ milestone }) => milestone === 'sample-execution-failed'))
      .toMatchObject({
        outcome: 'failed',
        context: { failureCode: 'sample-execution-failed' },
      })
    expect(observedTraps()).toBe(0)
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
})

describe('browser network matrix runner evidence resilience', () => {
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

function recordingOwnershipRegistrar(
  operationIds: string[],
): NetworkMatrixOwnershipRegistrar {
  const registration = (): NetworkMatrixOwnershipRegistration => Object.freeze({
    normalTerminal: () => undefined,
    forcedTerminal: () => undefined,
    handoff: (input: NetworkMatrixOwnershipInput) => {
      operationIds.push(input.operationId)
      return registration()
    },
  })
  return Object.freeze({
    register: (input: NetworkMatrixOwnershipInput) => {
      operationIds.push(input.operationId)
      return registration()
    },
  })
}

function secondOperationDeadlineScheduler(): NetworkMatrixDeadlineScheduler {
  let operations = 0
  return Object.freeze({
    schedule() {
      operations += 1
      return Object.freeze({
        elapsed: operations === 2 ? Promise.resolve() : new Promise<void>(() => undefined),
        cancel: () => undefined,
      })
    },
  })
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
