import {
  aggregateNetworkMatrix,
  canonicalNetworkMatrixAggregateJson,
  parseNetworkMatrixAggregateJson,
  type NetworkMatrixAggregate,
} from '../aggregate.ts'
import { requireEnum, requireRunId } from '../contract-support.ts'
import type { LoadedNetworkMatrixRegistry } from '../manifest.ts'
import {
  settleOwnedOperation,
  deferredOwnedOperation,
  NetworkMatrixOwnershipCleanupError,
  type NetworkMatrixDeadlineScheduler,
  type NetworkMatrixOwnedOperation,
  type NetworkMatrixOwnershipRegistration,
} from '../owned-operation.ts'
import {
  canonicalNetworkRunResultJson,
  parseNetworkRunResult,
  parseNetworkRunResultJson,
  type NetworkRunResult,
} from '../result.ts'
import {
  runNetworkMatrix,
  type NetworkMatrixRunTrace,
  type NetworkMatrixRunTraceSink,
  type NetworkMatrixRunnerDeadlines,
  type NetworkMatrixSampleExecutor,
} from '../runner.ts'
import { NetworkMatrixRunCollector } from '../run-collector.ts'
import {
  NetworkMatrixInvocationOwnershipLedger,
  newNetworkMatrixInvocationId,
} from '../invocation-ownership.ts'
import {
  failedNetworkMatrixAuthorityPreparation,
  type NetworkMatrixAuthorityResolver,
} from '../runtime-authority.ts'
import {
  NETWORK_MATRIX_EXECUTION_MODES,
  type NetworkMatrixExecutionMode,
} from '../vocabulary.ts'
import {
  type NetworkMatrixArtifactPublication,
  type NetworkMatrixArtifactPublisher,
} from './atomic-publication.ts'

export const NETWORK_MATRIX_RUNTIME_BOOTSTRAP_DEADLINE_MS = 30_000 as const
export const NETWORK_MATRIX_RUNTIME_CLOSE_DEADLINE_MS = 90_000 as const

export interface NetworkMatrixRuntimeSettlementReceipt {
  readonly terminal: 'closed'
}

export interface NetworkMatrixExecutionRuntime {
  readonly authorities: NetworkMatrixAuthorityResolver
  readonly samples: NetworkMatrixSampleExecutor
  closeAndWait(): Promise<NetworkMatrixRuntimeSettlementReceipt>
  forceTerminateAndWait(): Promise<NetworkMatrixRuntimeSettlementReceipt>
}

export interface NetworkMatrixRuntimeBootstrapContext {
  readonly registry: LoadedNetworkMatrixRegistry
  readonly invocationId: string
  readonly runId: string
  readonly executionMode: NetworkMatrixExecutionMode
}

export interface NetworkMatrixRuntimeBootstrap {
  bootstrap(
    context: NetworkMatrixRuntimeBootstrapContext,
  ): NetworkMatrixOwnedOperation<NetworkMatrixExecutionRuntime>
}

export interface ExecuteNetworkMatrixOptions {
  readonly registry: LoadedNetworkMatrixRegistry
  readonly runId: string
  readonly executionMode: NetworkMatrixExecutionMode
  readonly outputRoot: string
  readonly runtimeBootstrap: NetworkMatrixRuntimeBootstrap
  readonly publisher: NetworkMatrixArtifactPublisher
  readonly bootstrapDeadlineMs?: number
  readonly bootstrapDeadlineScheduler?: NetworkMatrixDeadlineScheduler
  readonly runtimeCloseDeadlineMs?: number
  readonly runtimeCloseDeadlineScheduler?: NetworkMatrixDeadlineScheduler
  readonly runnerDeadlines?: NetworkMatrixRunnerDeadlines
  readonly runnerDeadlineScheduler?: NetworkMatrixDeadlineScheduler
  readonly trace?: NetworkMatrixRunTraceSink
  readonly invocationId?: string
}

export interface ExecuteNetworkMatrixResult {
  readonly commandOutcome:
    | 'completed'
    | 'runtime-bootstrap-failed'
    | 'collector-failed'
    | 'containment-cleanup-failed'
  readonly run: NetworkRunResult
  readonly aggregate: NetworkMatrixAggregate
  readonly publication: NetworkMatrixArtifactPublication
}

/**
 * This is the only execute transaction: it creates the parent-owned collector,
 * derives one aggregate from the exact staged run bytes, and publishes both
 * contracts together. Browser children never receive a filesystem writer.
 */
export async function executeNetworkMatrix(
  options: ExecuteNetworkMatrixOptions,
): Promise<ExecuteNetworkMatrixResult> {
  const runId = requireRunId(options.runId, 'browser network matrix execute run ID')
  const executionMode = requireEnum(
    options.executionMode,
    NETWORK_MATRIX_EXECUTION_MODES,
    'browser network matrix execution mode',
  )
  const trace = options.trace ?? defaultTraceSink
  const ownership = new NetworkMatrixInvocationOwnershipLedger(
    runId,
    options.invocationId ?? newNetworkMatrixInvocationId(),
  )
  const collector = new NetworkMatrixRunCollector({
    registry: options.registry,
    runId,
    executionMode,
  })
  const bootstrapContext = Object.freeze({
    registry: options.registry,
    runId,
    executionMode,
    invocationId: ownership.binding.invocationId,
  })
  emitTrace(trace, runId, 'runtime-bootstrap-started', { executionMode })
  let runtime: NetworkMatrixExecutionRuntime
  let runtimeOwnership: NetworkMatrixOwnershipRegistration | undefined
  try {
    runtime = await settleOwnedOperation(
      deferredOwnedOperation(() => options.runtimeBootstrap.bootstrap(bootstrapContext)),
      'runtime-bootstrap',
      options.bootstrapDeadlineMs ?? NETWORK_MATRIX_RUNTIME_BOOTSTRAP_DEADLINE_MS,
      options.bootstrapDeadlineScheduler,
      Object.freeze({
        registrar: ownership,
        operationId: `runtime-bootstrap-${ownership.binding.invocationId}`,
        successor: (acquiredRuntime: NetworkMatrixExecutionRuntime) => Object.freeze({
          operationId: `runtime-live-${ownership.binding.invocationId}`,
          operationClass: 'runtime-close' as const,
          forceTerminateAndWait: async () => {
            requireRuntimeSettlementReceipt(await acquiredRuntime.forceTerminateAndWait())
          },
        }),
        onSuccessorRegistered: (registration: NetworkMatrixOwnershipRegistration) => {
          runtimeOwnership = registration
        },
      }),
    )
  } catch (cause) {
    emitTrace(trace, runId, 'runtime-bootstrap-failed', { executionMode })
    const commandOutcome = cause instanceof NetworkMatrixOwnershipCleanupError
      ? 'containment-cleanup-failed'
      : 'runtime-bootstrap-failed'
    const run = finalizeBootstrapFailure(
      options.registry,
      collector,
      runId,
      executionMode,
      commandOutcome,
    )
    return publishRunAndRetainOwnership(options, trace, ownership, run, commandOutcome)
  }
  emitTrace(trace, runId, 'runtime-bootstrap-completed', { executionMode })
  if (runtimeOwnership === undefined) {
    throw new Error('network matrix runtime ownership handoff did not publish its successor')
  }
  let run: NetworkRunResult | undefined
  let terminalFailure: 'collector-failed' | 'containment-cleanup-failed' | undefined
  try {
    run = await runNetworkMatrix({
      registry: options.registry,
      runId,
      executionMode,
      authorities: runtime.authorities,
      samples: runtime.samples,
      collector,
      ...(options.runnerDeadlines === undefined ? {} : { deadlines: options.runnerDeadlines }),
      ...(options.runnerDeadlineScheduler === undefined
        ? {}
        : { deadlineScheduler: options.runnerDeadlineScheduler }),
      trace,
      ownershipRegistrar: ownership,
    })
  } catch (cause) {
    terminalFailure = cause instanceof NetworkMatrixOwnershipCleanupError
      ? 'containment-cleanup-failed'
      : 'collector-failed'
    emitTrace(trace, runId, 'run-execution-failed', {
      executionMode,
      failureCode: terminalFailure,
    })
  }
  const settlementFailure = await settleExecutionRuntime(
    options,
    runtime,
    runtimeOwnership,
    trace,
    runId,
    executionMode,
  )
  if (settlementFailure !== undefined) terminalFailure = 'containment-cleanup-failed'
  if (terminalFailure !== undefined) {
    run = run === undefined
      ? finalizeExecutionFailure(
          options.registry,
          collector,
          runId,
          executionMode,
          terminalFailure,
        )
      : markRunOrchestrationFailure(options.registry, run, terminalFailure)
  }
  if (run === undefined) throw new Error('network matrix execution did not produce a terminal run')
  return publishRunAndRetainOwnership(
    options,
    trace,
    ownership,
    run,
    terminalFailure ?? 'completed',
  )
}

async function settleExecutionRuntime(
  options: ExecuteNetworkMatrixOptions,
  runtime: NetworkMatrixExecutionRuntime,
  runtimeOwnership: NetworkMatrixOwnershipRegistration,
  trace: NetworkMatrixRunTraceSink,
  runId: string,
  executionMode: NetworkMatrixExecutionMode,
): Promise<unknown | undefined> {
  emitTrace(trace, runId, 'runtime-close-started', { executionMode })
  const operation: NetworkMatrixOwnedOperation<NetworkMatrixRuntimeSettlementReceipt> =
    Object.freeze({
      result: Promise.resolve()
        .then(() => runtime.closeAndWait())
        .then(requireRuntimeSettlementReceipt),
      forceTerminateAndWait: async (): Promise<void> => {
        requireRuntimeSettlementReceipt(await runtime.forceTerminateAndWait())
      },
    })
  try {
    await settleOwnedOperation(
      operation,
      'runtime-close',
      options.runtimeCloseDeadlineMs ?? NETWORK_MATRIX_RUNTIME_CLOSE_DEADLINE_MS,
      options.runtimeCloseDeadlineScheduler,
    )
    emitTrace(trace, runId, 'runtime-close-completed', { executionMode })
    runtimeOwnership.normalTerminal()
    return undefined
  } catch (cause) {
    if (!(cause instanceof NetworkMatrixOwnershipCleanupError)) {
      runtimeOwnership.forcedTerminal()
    }
    emitTrace(trace, runId, 'runtime-close-failed', { executionMode })
    return cause
  }
}

function requireRuntimeSettlementReceipt(
  value: unknown,
): NetworkMatrixRuntimeSettlementReceipt {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error('network matrix runtime settlement receipt is invalid')
  }
  const receipt = value as Record<string, unknown>
  const keys = Object.keys(receipt)
  if (keys.length !== 1 || keys[0] !== 'terminal' || receipt.terminal !== 'closed') {
    throw new Error('network matrix runtime settlement receipt is invalid')
  }
  return Object.freeze({ terminal: 'closed' })
}

function finalizeExecutionFailure(
  registry: LoadedNetworkMatrixRegistry,
  existingCollector: NetworkMatrixRunCollector,
  runId: string,
  executionMode: NetworkMatrixExecutionMode,
  orchestrationFailureCode: 'collector-failed' | 'containment-cleanup-failed',
): NetworkRunResult {
  return existingCollector.terminalize({ failureCode: orchestrationFailureCode }, (reference) => {
    const profile = registry.profiles.find(({ profileId }) => profileId === reference.profileId)
    if (profile === undefined || reference.executionMode !== executionMode) {
      throw new Error(`loaded profile ${reference.profileId} is absent or crossed execution mode`)
    }
    return failedNetworkMatrixAuthorityPreparation({
      registry,
      reference,
      profile,
      runId,
      signal: new AbortController().signal,
    }, 'runtime-check-failed').attestation
  })
}

function markRunOrchestrationFailure(
  registry: LoadedNetworkMatrixRegistry,
  run: NetworkRunResult,
  failureCode: 'collector-failed' | 'containment-cleanup-failed',
): NetworkRunResult {
  return parseNetworkRunResult({
    ...run,
    orchestrationOutcome: 'failed',
    orchestrationFailure: { failureCode },
    runOutcome: 'infrastructure-failed',
  }, registry)
}

async function publishRunAndRetainOwnership(
  options: ExecuteNetworkMatrixOptions,
  trace: NetworkMatrixRunTraceSink,
  ownership: NetworkMatrixInvocationOwnershipLedger,
  run: NetworkRunResult,
  commandOutcome: ExecuteNetworkMatrixResult['commandOutcome'],
): Promise<ExecuteNetworkMatrixResult> {
  const publication = publishRun(options, trace, run, commandOutcome).then(
    (published) => Object.freeze({ outcome: 'published' as const, published }),
    (cause: unknown) => Object.freeze({ outcome: 'failed' as const, cause }),
  )
  // Evidence publication and retained-owner recovery are independent children of
  // the invocation. Starting both prevents a blocked publisher from starving the
  // exact force retry that still owns a runtime, lease, or process subtree.
  const retainedSettlement = ownership.retainUntilEmpty(undefined, (_cause, retainedOperationIds) => {
    emitTrace(trace, run.runId, 'ownership-retry-failed', {
      executionMode: run.executionMode,
      invocationId: ownership.binding.invocationId,
      retainedOperationIds,
    })
  })
  const [publicationOutcome] = await Promise.all([publication, retainedSettlement])
  if (publicationOutcome.outcome === 'failed') throw publicationOutcome.cause
  return publicationOutcome.published
}

async function publishRun(
  options: ExecuteNetworkMatrixOptions,
  trace: NetworkMatrixRunTraceSink,
  run: NetworkRunResult,
  commandOutcome: ExecuteNetworkMatrixResult['commandOutcome'],
): Promise<ExecuteNetworkMatrixResult> {
  const { runId, executionMode } = run
  const runJson = canonicalNetworkRunResultJson(run, options.registry)
  emitTrace(trace, runId, 'artifact-publication-started', {
    executionMode,
    observedSamples: run.samples.length,
  })
  let publication: NetworkMatrixArtifactPublication
  try {
    publication = await options.publisher.publish({
      outputRoot: options.outputRoot,
      runJson,
      deriveAggregateJson: (stagedRunJson) => {
        const stagedRun = parseNetworkRunResultJson(stagedRunJson, options.registry)
        const aggregate = aggregateNetworkMatrix(options.registry, [stagedRun])
        return canonicalNetworkMatrixAggregateJson(aggregate, options.registry, [stagedRun])
      },
    })
  } catch (cause) {
    emitTrace(trace, runId, 'artifact-publication-failed', { executionMode })
    throw cause
  }
  if (publication.runJson !== runJson) {
    throw new Error('network matrix publisher changed the canonical run bytes')
  }
  const publishedRun = parseNetworkRunResultJson(publication.runJson, options.registry)
  const aggregate = parseNetworkMatrixAggregateJson(
    publication.aggregateJson,
    options.registry,
    [publishedRun],
  )
  emitTrace(trace, runId, 'artifact-publication-completed', {
    executionMode,
    evidenceOutcome: aggregate.evidenceOutcome,
  })
  return Object.freeze({ commandOutcome, run: publishedRun, aggregate, publication })
}

function finalizeBootstrapFailure(
  registry: LoadedNetworkMatrixRegistry,
  collector: NetworkMatrixRunCollector,
  runId: string,
  executionMode: NetworkMatrixExecutionMode,
  orchestrationFailureCode: 'runtime-bootstrap-failed' | 'containment-cleanup-failed',
): NetworkRunResult {
  const references = registry.manifest.profiles.filter(
    (reference) => reference.executionMode === executionMode,
  )
  for (const reference of references) {
    const profile = registry.profiles.find(({ profileId }) => profileId === reference.profileId)
    if (profile === undefined) throw new Error(`loaded profile ${reference.profileId} is absent`)
    const prepared = failedNetworkMatrixAuthorityPreparation({
      registry,
      reference,
      profile,
      runId,
      signal: new AbortController().signal,
    }, 'runtime-bootstrap-failed')
    collector.recordAttestation(prepared.attestation)
  }
  return collector.finalize({ failureCode: orchestrationFailureCode })
}

function emitTrace(
  sink: NetworkMatrixRunTraceSink,
  runId: string,
  milestone: string,
  context: Readonly<Record<string, unknown>>,
): void {
  const event: NetworkMatrixRunTrace = Object.freeze({
    operationId: runId,
    milestone,
    runId,
    context: Object.freeze(context),
  })
  try {
    sink(event)
  } catch {
    // Observability cannot acquire authority over evidence execution.
  }
}

function defaultTraceSink(trace: NetworkMatrixRunTrace): void {
  process.stderr.write(`${JSON.stringify({ component: 'browser-network-matrix-execute', ...trace })}\n`)
}
