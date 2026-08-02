import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { parseNetworkMatrixAggregateJson } from '../../scripts/browser-network-matrix/aggregate.ts'
import { parseNetworkRuntimeAttestation } from '../../scripts/browser-network-matrix/attestation.ts'
import {
  startNetworkMatrixExecution,
  type ExecuteNetworkMatrixOptions,
  type ExecuteNetworkMatrixResult,
  type NetworkMatrixRunnerStarter,
  type NetworkMatrixRuntimeBootstrap,
} from '../../scripts/browser-network-matrix/cli/execute.ts'
import type { NetworkMatrixArtifactPublisher } from '../../scripts/browser-network-matrix/cli/atomic-publication.ts'
import { sha256 } from '../../scripts/browser-network-matrix/manifest.ts'
import {
  completedOwnedOperation,
  type NetworkMatrixDeadlineScheduler,
  type NetworkMatrixOwnedOperation,
} from '../../scripts/browser-network-matrix/owned-operation.ts'
import { parseNetworkRunResultJson } from '../../scripts/browser-network-matrix/result.ts'
import {
  startNetworkMatrix,
  type NetworkMatrixSampleExecutionContext,
  type NetworkMatrixSampleExecutor,
} from '../../scripts/browser-network-matrix/runner.ts'
import type {
  NetworkMatrixAuthorityPreparationContext,
  NetworkMatrixAuthorityResolver,
  PreparedNetworkMatrixAuthority,
} from '../../scripts/browser-network-matrix/runtime-authority.ts'
import { loadRegistry, matchedAttemptEvidence, rawAttestation } from './fixtures.ts'

const temporaryRoots: string[] = []

async function executeNetworkMatrix(
  options: ExecuteNetworkMatrixOptions,
): Promise<ExecuteNetworkMatrixResult> {
  const execution = startNetworkMatrixExecution(options)
  const result = await execution.result
  const snapshot = execution.traces.snapshot()
  if (
    snapshot.failure !== null ||
    !snapshot.completed ||
    snapshot.truncated ||
    snapshot.observedEvents !== snapshot.capturedEvents ||
    snapshot.observedBytes !== snapshot.capturedBytes
  ) throw new Error('test discarded incomplete network matrix execution trace evidence')
  return result
}

afterEach(async () => {
  await Promise.all(temporaryRoots.splice(0).map((root) => rm(root, { recursive: true, force: true })))
})

describe('local browser network matrix execute transaction', () => {
  it('owns collection and publishes an aggregate derived from the exact staged run', async () => {
    const registry = await loadRegistry()
    const parent = await temporaryRoot()
    const outputRoot = join(parent, 'scheduled-attempt')
    const bootstrap = successfulBootstrap()

    const execution = startNetworkMatrixExecution({
      registry,
      runId: 'local-scheduled-attempt',
      executionMode: 'scheduled',
      outputRoot,
      runtimeBootstrap: bootstrap,
      publisher: testArtifactPublisher(),
    })
    const stalledConsumer = execution.traces[Symbol.asyncIterator]()
    expect(await stalledConsumer.next()).toMatchObject({
      done: false,
      value: { milestone: 'execution-started' },
    })
    const result = await execution.result
    expect(execution.traces.snapshot()).toMatchObject({
      completed: true,
      truncated: false,
      failure: null,
      observedEvents: execution.traces.snapshot().capturedEvents,
    })

    expect(result.run).toMatchObject({
      executionMode: 'scheduled',
      runOutcome: 'completed',
    })
    expect(result.commandOutcome).toBe('completed')
    expect(execution.traces.snapshot().events.find(
      ({ milestone }) => milestone === 'execution-terminal',
    )).toMatchObject({
      outcome: 'succeeded',
      context: {
        aggregateHardOracleAccepted: true,
        cleanupOutcome: 'completed',
        evidenceOutcome: 'complete',
        lastMilestone: 'artifact-publication-completed',
      },
    })
    expect(result.run.samples).toHaveLength(45)
    expect(result.aggregate).toMatchObject({
      evidenceOutcome: 'complete',
      runs: [{ executionMode: 'scheduled', runId: 'local-scheduled-attempt' }],
    })
    const runJson = await readFile(join(outputRoot, 'run.json'), 'utf8')
    const aggregateJson = await readFile(join(outputRoot, 'aggregate.json'), 'utf8')
    const stagedRun = parseNetworkRunResultJson(runJson, registry)
    const stagedAggregate = parseNetworkMatrixAggregateJson(aggregateJson, registry, [stagedRun])
    expect(stagedAggregate.runs[0]?.runSha256).toBe(sha256(runJson))
    expect(bootstrap.bootstrap).toHaveBeenCalledWith({
      registry,
      invocationId: expect.stringMatching(/^invocation-[a-f0-9]{32}$/u),
      runId: 'local-scheduled-attempt',
      executionMode: 'scheduled',
    })
  })

  it('force-settles an interrupted bootstrap before publishing its terminal ledger', async () => {
    const registry = await loadRegistry()
    const parent = await temporaryRoot()
    const outputRoot = join(parent, 'interrupted-attempt')
    const forceTerminateAndWait = vi.fn().mockResolvedValue(undefined)
    const bootstrap: NetworkMatrixRuntimeBootstrap = {
      bootstrap: vi.fn().mockReturnValue({
        result: new Promise(() => undefined),
        forceTerminateAndWait,
      } satisfies NetworkMatrixOwnedOperation<never>),
    }

    const result = await executeNetworkMatrix({
      registry,
      runId: 'interrupted-bootstrap-attempt',
      executionMode: 'scheduled',
      outputRoot,
      runtimeBootstrap: bootstrap,
      publisher: testArtifactPublisher(),
      bootstrapDeadlineMs: 1,
      bootstrapDeadlineScheduler: immediateDeadlineScheduler(),
    })

    expect(forceTerminateAndWait).toHaveBeenCalledOnce()
    expect(forceTerminateAndWait).toHaveBeenCalledWith('runtime-bootstrap')
    expect(result).toMatchObject({
      commandOutcome: 'runtime-bootstrap-failed',
      run: {
        orchestrationOutcome: 'failed',
        orchestrationFailure: { failureCode: 'runtime-bootstrap-failed' },
        samples: [],
        runOutcome: 'infrastructure-failed',
      },
      aggregate: { evidenceOutcome: 'infrastructure-failed' },
    })
    expect(result.run.runtimeAttestations).toHaveLength(3)
    expect(result.run.runtimeAttestations[0]).toMatchObject({
      prerequisiteOutcome: 'failed',
      failure: { failureCode: 'runtime-bootstrap-failed' },
    })
    expect(parseNetworkRunResultJson(await readFile(join(outputRoot, 'run.json'), 'utf8'), registry))
      .toEqual(result.run)
  })

  it('force-settles a rejected bootstrap and publishes zero-sample failure evidence', async () => {
    const registry = await loadRegistry()
    const parent = await temporaryRoot()
    const outputRoot = join(parent, 'rejected-attempt')
    const forceTerminateAndWait = vi.fn().mockResolvedValue(undefined)
    const bootstrapFailure = new Error('injected bootstrap rejection')
    const bootstrap: NetworkMatrixRuntimeBootstrap = {
      bootstrap: vi.fn().mockReturnValue({
        result: Promise.reject(bootstrapFailure),
        forceTerminateAndWait,
      }),
    }

    const result = await executeNetworkMatrix({
      registry,
      runId: 'rejected-bootstrap-attempt',
      executionMode: 'scheduled',
      outputRoot,
      runtimeBootstrap: bootstrap,
      publisher: testArtifactPublisher(),
    })

    expect(forceTerminateAndWait).toHaveBeenCalledWith('runtime-bootstrap')
    expect(result.commandOutcome).toBe('runtime-bootstrap-failed')
    expect(result.run.orchestrationFailure?.failureCode).toBe('runtime-bootstrap-failed')
    expect(await readFile(join(outputRoot, 'aggregate.json'), 'utf8')).toBe(result.publication.aggregateJson)
  })

  it.each([
    ['error-accessors', () => {
      let traps = 0
      const failure = new Error('opaque bootstrap failure')
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
    ['proxy-traps', () => {
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
  ])('publishes cleanup and terminal evidence without inspecting hostile bootstrap %s', async (
    label,
    createFailure,
  ) => {
    const registry = await loadRegistry()
    const parent = await temporaryRoot()
    const outputRoot = join(parent, label)
    const { failure, observedTraps } = createFailure()
    const forceTerminateAndWait = vi.fn().mockResolvedValue(undefined)
    const bootstrap: NetworkMatrixRuntimeBootstrap = {
      bootstrap: () => Object.freeze({
        result: Promise.reject(failure),
        forceTerminateAndWait,
      }),
    }
    const execution = startNetworkMatrixExecution({
      registry,
      runId: `hostile-bootstrap-${label}`,
      executionMode: 'scheduled',
      outputRoot,
      runtimeBootstrap: bootstrap,
      publisher: testArtifactPublisher(),
    })

    const result = await execution.result
    const snapshot = execution.traces.snapshot()
    const bootstrapFailure = snapshot.events.find(
      ({ milestone }) => milestone === 'runtime-bootstrap-failed',
    )

    expect(forceTerminateAndWait).toHaveBeenCalledWith('runtime-bootstrap')
    expect(result).toMatchObject({
      commandOutcome: 'runtime-bootstrap-failed',
      runtimeCleanupOutcome: 'completed',
      runnerTraces: null,
    })
    expect(bootstrapFailure).toMatchObject({
      outcome: 'failed',
      context: {
        failureCode: 'runtime-bootstrap-failed',
      },
    })
    expect(snapshot).toMatchObject({
      completed: true,
      failure: null,
      truncated: false,
      observedEvents: snapshot.capturedEvents,
      observedBytes: snapshot.capturedBytes,
    })
    expect(observedTraps()).toBe(0)
  })

  it('terminalizes every scheduled profile when composition fails before authority preparation', async () => {
    const registry = await loadRegistry()
    const parent = await temporaryRoot()
    const outputRoot = join(parent, 'scheduled-bootstrap-failure')
    const bootstrap: NetworkMatrixRuntimeBootstrap = {
      bootstrap: vi.fn().mockReturnValue({
        result: Promise.reject(new Error('scheduled composition failed')),
        forceTerminateAndWait: vi.fn().mockResolvedValue(undefined),
      }),
    }

    const result = await executeNetworkMatrix({
      registry,
      runId: 'scheduled-bootstrap-failure-attempt',
      executionMode: 'scheduled',
      outputRoot,
      runtimeBootstrap: bootstrap,
      publisher: testArtifactPublisher(),
    })

    expect(result.run.samples).toEqual([])
    expect(result.run.runtimeAttestations.map(({ profileId, prerequisiteOutcome, failure }) => ({
      profileId,
      prerequisiteOutcome,
      failureCode: failure?.failureCode,
    }))).toEqual([
      {
        profileId: 'scheduled-public-stun',
        prerequisiteOutcome: 'failed',
        failureCode: 'runtime-bootstrap-failed',
      },
      {
        profileId: 'scheduled-restricted-udp',
        prerequisiteOutcome: 'failed',
        failureCode: 'runtime-bootstrap-failed',
      },
      {
        profileId: 'scheduled-coturn',
        prerequisiteOutcome: 'failed',
        failureCode: 'runtime-bootstrap-failed',
      },
    ])
    expect(result.run.profileResults.every(({ profileOutcome }) => profileOutcome === 'not-executed'))
      .toBe(true)
    expect(result.aggregate.counts.observedSamples).toBe(0)
  })

  it('publishes containment loss instead of downgrading failed bootstrap cleanup', async () => {
    const registry = await loadRegistry()
    const parent = await temporaryRoot()
    const outputRoot = join(parent, 'cleanup-loss-attempt')
    let markRetryStarted: (() => void) | undefined
    const retryStarted = new Promise<void>((resolve) => { markRetryStarted = resolve })
    let releaseRetry: (() => void) | undefined
    const retryGate = new Promise<void>((resolve) => { releaseRetry = resolve })
    const forceTerminateAndWait = vi.fn()
      .mockRejectedValueOnce(new Error('cleanup proof lost'))
      .mockImplementationOnce(async () => {
        markRetryStarted?.()
        await retryGate
      })
    const bootstrap: NetworkMatrixRuntimeBootstrap = {
      bootstrap: vi.fn().mockReturnValue({
        result: Promise.reject(new Error('bootstrap failed')),
        forceTerminateAndWait,
      }),
    }

    const execution = executeNetworkMatrix({
      registry,
      runId: 'bootstrap-cleanup-loss-attempt',
      executionMode: 'scheduled',
      outputRoot,
      runtimeBootstrap: bootstrap,
      publisher: testArtifactPublisher(),
    })
    let settled = false
    const settlementObservation = execution.finally(() => { settled = true })
    await retryStarted

    expect(settled).toBe(false)
    expect(forceTerminateAndWait).toHaveBeenCalledTimes(2)
    await vi.waitFor(async () => {
      await expect(readFile(join(outputRoot, 'run.json'), 'utf8')).resolves.toContain(
        'containment-cleanup-failed',
      )
    })
    releaseRetry?.()
    const result = await execution
    await settlementObservation

    expect(result.commandOutcome).toBe('containment-cleanup-failed')
    expect(result.run.orchestrationFailure?.failureCode).toBe('containment-cleanup-failed')
    expect(result.run.runtimeAttestations[0]?.failure?.failureCode)
      .toBe('runtime-bootstrap-failed')
  })

  it('closes and drains the runtime before terminalizing malformed runner trace evidence', async () => {
    const registry = await loadRegistry()
    const parent = await temporaryRoot()
    const closeAndWait = vi.fn().mockResolvedValue(Object.freeze({ terminal: 'closed' as const }))
    const forceTerminateAndWait = vi.fn().mockResolvedValue(
      Object.freeze({ terminal: 'closed' as const }),
    )
    const runtime = Object.freeze({
      authorities: scheduledAuthorityResolver(),
      samples: scheduledSampleExecutor(),
      closeAndWait,
      forceTerminateAndWait,
    })
    const bootstrap: NetworkMatrixRuntimeBootstrap = {
      bootstrap: () => completedOwnedOperation(runtime),
    }
    const runner: NetworkMatrixRunnerStarter = {
      start(options) {
        const execution = startNetworkMatrix(options)
        return Object.freeze({
          result: execution.result,
          traces: Object.freeze({
            snapshot: () => Object.freeze({
              ...execution.traces.snapshot(),
              completed: false,
            }),
            [Symbol.asyncIterator]: () => execution.traces[Symbol.asyncIterator](),
          }),
        })
      },
    }

    const result = await executeNetworkMatrix({
      registry,
      runId: 'malformed-runner-trace-cleanup',
      executionMode: 'scheduled',
      outputRoot: join(parent, 'malformed-runner-trace'),
      runtimeBootstrap: bootstrap,
      publisher: testArtifactPublisher(),
      runner,
    })

    expect(closeAndWait).toHaveBeenCalledOnce()
    expect(forceTerminateAndWait).not.toHaveBeenCalled()
    expect(result).toMatchObject({
      commandOutcome: 'collector-failed',
      runtimeCleanupOutcome: 'completed',
      runnerTraces: { completed: false },
      run: {
        orchestrationOutcome: 'failed',
        orchestrationFailure: { failureCode: 'collector-failed' },
      },
    })
  })

  it('reports completed runtime cleanup when only post-cleanup publication fails', async () => {
    const registry = await loadRegistry()
    const parent = await temporaryRoot()
    const closeAndWait = vi.fn().mockResolvedValue(Object.freeze({ terminal: 'closed' as const }))
    const forceTerminateAndWait = vi.fn().mockResolvedValue(
      Object.freeze({ terminal: 'closed' as const }),
    )
    const runtime = Object.freeze({
      authorities: scheduledAuthorityResolver(),
      samples: scheduledSampleExecutor(),
      closeAndWait,
      forceTerminateAndWait,
    })
    const bootstrap: NetworkMatrixRuntimeBootstrap = {
      bootstrap: () => completedOwnedOperation(runtime),
    }
    const publicationFailure = new Error('publication failed after cleanup')
    const execution = startNetworkMatrixExecution({
      registry,
      runId: 'post-cleanup-publication-failure',
      executionMode: 'scheduled',
      outputRoot: join(parent, 'publication-failure'),
      runtimeBootstrap: bootstrap,
      publisher: {
        publish: vi.fn().mockRejectedValue(publicationFailure),
      },
    })

    let observed: unknown
    try {
      await execution.result
    } catch (cause) {
      observed = cause
    }
    const snapshot = execution.traces.snapshot()
    const runnerTraces = await execution.runnerTraces
    const terminal = snapshot.events.find(({ milestone }) => milestone === 'execution-terminal')

    expect(observed).toBe(publicationFailure)
    expect(runnerTraces).toMatchObject({
      completed: true,
      failure: null,
      truncated: false,
      observedEvents: runnerTraces?.capturedEvents,
      observedBytes: runnerTraces?.capturedBytes,
    })
    expect(closeAndWait).toHaveBeenCalledOnce()
    expect(forceTerminateAndWait).not.toHaveBeenCalled()
    expect(terminal).toMatchObject({
      outcome: 'failed',
      context: {
        cleanupOutcome: 'completed',
        failureCode: 'execution-operation-failed',
        lastMilestone: 'execution-failed',
      },
    })
    expect(snapshot).toMatchObject({
      completed: true,
      failure: null,
      truncated: false,
      observedEvents: snapshot.capturedEvents,
      observedBytes: snapshot.capturedBytes,
    })
  })

  it('retries retained cleanup while artifact publication is still pending', async () => {
    const registry = await loadRegistry()
    const parent = await temporaryRoot()
    const outputRoot = join(parent, 'concurrent-publication-cleanup')
    let markRetryStarted: (() => void) | undefined
    const retryStarted = new Promise<void>((resolve) => { markRetryStarted = resolve })
    let releaseRetry: (() => void) | undefined
    const retryGate = new Promise<void>((resolve) => { releaseRetry = resolve })
    const forceTerminateAndWait = vi.fn()
      .mockRejectedValueOnce(new Error('first cleanup proof lost'))
      .mockImplementationOnce(async () => {
        markRetryStarted?.()
        await retryGate
      })
    const bootstrap: NetworkMatrixRuntimeBootstrap = {
      bootstrap: () => ({
        result: Promise.reject(new Error('bootstrap rejected')),
        forceTerminateAndWait,
      }),
    }
    let markPublicationStarted: (() => void) | undefined
    const publicationStarted = new Promise<void>((resolve) => { markPublicationStarted = resolve })
    let releasePublication: (() => void) | undefined
    const publicationGate = new Promise<void>((resolve) => { releasePublication = resolve })
    const basePublisher = testArtifactPublisher()
    const publisher: NetworkMatrixArtifactPublisher = {
      async publish(input) {
        markPublicationStarted?.()
        await publicationGate
        return basePublisher.publish(input)
      },
    }

    const execution = executeNetworkMatrix({
      registry,
      runId: 'concurrent-publication-cleanup',
      executionMode: 'scheduled',
      outputRoot,
      runtimeBootstrap: bootstrap,
      publisher,
    })
    await Promise.all([publicationStarted, retryStarted])

    expect(forceTerminateAndWait).toHaveBeenCalledTimes(2)
    let settled = false
    const settlementObservation = execution.then(
      () => { settled = true },
      () => { settled = true },
    )
    await Promise.resolve()
    expect(settled).toBe(false)

    releaseRetry?.()
    releasePublication?.()
    await expect(execution).resolves.toMatchObject({
      commandOutcome: 'containment-cleanup-failed',
    })
    await settlementObservation
  })
})

function successfulBootstrap(): NetworkMatrixRuntimeBootstrap & {
  readonly bootstrap: ReturnType<typeof vi.fn>
} {
  const runtime = Object.freeze({
    authorities: scheduledAuthorityResolver(),
    samples: scheduledSampleExecutor(),
    closeAndWait: async () => Object.freeze({ terminal: 'closed' as const }),
    forceTerminateAndWait: async () => Object.freeze({ terminal: 'closed' as const }),
  })
  return {
    bootstrap: vi.fn().mockReturnValue(completedOwnedOperation(runtime)),
  }
}

function scheduledAuthorityResolver(): NetworkMatrixAuthorityResolver {
  return {
    prepare(context: NetworkMatrixAuthorityPreparationContext) {
      const attestation = parseNetworkRuntimeAttestation(
        rawAttestation(context.registry, context.runId, context.profile.profileId, 'satisfied'),
        {
          manifest: context.registry.manifest,
          manifestSha256: context.registry.manifestSha256,
          runId: context.runId,
        },
      )
      const prepared: PreparedNetworkMatrixAuthority = Object.freeze({
        attestation,
        execution: Object.freeze({
          profileId: context.profile.profileId,
          runtimeKind: 'external-fixture',
        }),
        close: () => completedOwnedOperation(undefined),
        forceTerminateAndWait: () => Promise.resolve(),
      })
      return completedOwnedOperation(prepared)
    },
  }
}

function scheduledSampleExecutor(): NetworkMatrixSampleExecutor {
  return {
    execute(context: NetworkMatrixSampleExecutionContext) {
      const processInstanceId = [
        'sample',
        context.identity.profileId,
        context.identity.browser,
        context.identity.sampleOrdinal,
      ].join('-')
      return completedOwnedOperation({
        processInstanceId,
        observation: {
          sampleOutcome: 'observed',
          attemptEvidence: matchedAttemptEvidence(context.identity, context.runId, {
            processInstanceId,
          }),
        },
      })
    },
  }
}

function immediateDeadlineScheduler(): NetworkMatrixDeadlineScheduler {
  return {
    schedule: () => ({ elapsed: Promise.resolve(), cancel: () => undefined }),
  }
}

async function temporaryRoot(): Promise<string> {
  const root = await mkdtemp(join(tmpdir(), 'windshare-network-matrix-execute-'))
  temporaryRoots.push(root)
  return root
}

function testArtifactPublisher(): NetworkMatrixArtifactPublisher {
  return {
    async publish(input) {
      const aggregateJson = input.deriveAggregateJson(input.runJson)
      const runPath = join(input.outputRoot, 'run.json')
      const aggregatePath = join(input.outputRoot, 'aggregate.json')
      await mkdir(input.outputRoot)
      await Promise.all([
        writeFile(runPath, input.runJson, { encoding: 'utf8', flag: 'wx' }),
        writeFile(aggregatePath, aggregateJson, { encoding: 'utf8', flag: 'wx' }),
      ])
      return Object.freeze({
        outputRoot: input.outputRoot,
        runPath,
        aggregatePath,
        runJson: input.runJson,
        aggregateJson,
      })
    },
  }
}
