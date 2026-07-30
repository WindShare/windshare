import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { parseNetworkMatrixAggregateJson } from '../../scripts/browser-network-matrix/aggregate.ts'
import { parseNetworkRuntimeAttestation } from '../../scripts/browser-network-matrix/attestation.ts'
import {
  executeNetworkMatrix,
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
import type {
  NetworkMatrixSampleExecutionContext,
  NetworkMatrixSampleExecutor,
} from '../../scripts/browser-network-matrix/runner.ts'
import type {
  NetworkMatrixAuthorityPreparationContext,
  NetworkMatrixAuthorityResolver,
  PreparedNetworkMatrixAuthority,
} from '../../scripts/browser-network-matrix/runtime-authority.ts'
import { loadRegistry, matchedAttemptEvidence, rawAttestation } from './fixtures.ts'

const temporaryRoots: string[] = []

afterEach(async () => {
  await Promise.all(temporaryRoots.splice(0).map((root) => rm(root, { recursive: true, force: true })))
})

describe('local browser network matrix execute transaction', () => {
  it('owns collection and publishes an aggregate derived from the exact staged run', async () => {
    const registry = await loadRegistry()
    const parent = await temporaryRoot()
    const outputRoot = join(parent, 'manual-attempt')
    const bootstrap = successfulBootstrap()

    const result = await executeNetworkMatrix({
      registry,
      runId: 'local-manual-attempt',
      executionMode: 'manual',
      outputRoot,
      runtimeBootstrap: bootstrap,
      publisher: testArtifactPublisher(),
      trace: () => undefined,
    })

    expect(result.run).toMatchObject({
      executionMode: 'manual',
      runOutcome: 'completed',
    })
    expect(result.commandOutcome).toBe('completed')
    expect(result.run.samples).toHaveLength(15)
    expect(result.aggregate).toMatchObject({
      evidenceOutcome: 'incomplete',
      runs: [{ executionMode: 'manual', runId: 'local-manual-attempt' }],
    })
    const runJson = await readFile(join(outputRoot, 'run.json'), 'utf8')
    const aggregateJson = await readFile(join(outputRoot, 'aggregate.json'), 'utf8')
    const stagedRun = parseNetworkRunResultJson(runJson, registry)
    const stagedAggregate = parseNetworkMatrixAggregateJson(aggregateJson, registry, [stagedRun])
    expect(stagedAggregate.runs[0]?.runSha256).toBe(sha256(runJson))
    expect(bootstrap.bootstrap).toHaveBeenCalledWith({
      registry,
      invocationId: expect.stringMatching(/^invocation-[a-f0-9]{32}$/u),
      runId: 'local-manual-attempt',
      executionMode: 'manual',
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
      executionMode: 'manual',
      outputRoot,
      runtimeBootstrap: bootstrap,
      publisher: testArtifactPublisher(),
      bootstrapDeadlineMs: 1,
      bootstrapDeadlineScheduler: immediateDeadlineScheduler(),
      trace: () => undefined,
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
    expect(result.run.runtimeAttestations).toHaveLength(1)
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
      executionMode: 'manual',
      outputRoot,
      runtimeBootstrap: bootstrap,
      publisher: testArtifactPublisher(),
      trace: () => undefined,
    })

    expect(forceTerminateAndWait).toHaveBeenCalledWith('runtime-bootstrap')
    expect(result.commandOutcome).toBe('runtime-bootstrap-failed')
    expect(result.run.orchestrationFailure?.failureCode).toBe('runtime-bootstrap-failed')
    expect(await readFile(join(outputRoot, 'aggregate.json'), 'utf8')).toBe(result.publication.aggregateJson)
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
      trace: () => undefined,
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
      executionMode: 'manual',
      outputRoot,
      runtimeBootstrap: bootstrap,
      publisher: testArtifactPublisher(),
      trace: () => undefined,
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
      executionMode: 'manual',
      outputRoot,
      runtimeBootstrap: bootstrap,
      publisher,
      trace: () => undefined,
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
    authorities: manualAuthorityResolver(),
    samples: manualSampleExecutor(),
    closeAndWait: async () => Object.freeze({ terminal: 'closed' as const }),
    forceTerminateAndWait: async () => Object.freeze({ terminal: 'closed' as const }),
  })
  return {
    bootstrap: vi.fn().mockReturnValue(completedOwnedOperation(runtime)),
  }
}

function manualAuthorityResolver(): NetworkMatrixAuthorityResolver {
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

function manualSampleExecutor(): NetworkMatrixSampleExecutor {
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
