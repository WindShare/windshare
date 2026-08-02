import { isProxy } from 'node:util/types'

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
  startNetworkMatrix,
  type NetworkMatrixRunExecution,
  type NetworkMatrixRunTrace,
  type NetworkMatrixRunnerDeadlines,
  type NetworkMatrixRunnerOptions,
  type NetworkMatrixSampleExecutor,
} from '../runner.ts'
import {
  createNetworkMatrixTraceJournal,
  networkMatrixTrace,
  settleNetworkMatrixTraceJournal,
  type NetworkMatrixTraceChannel,
  type NetworkMatrixTraceSnapshot,
} from '../trace/index.ts'
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
export const NETWORK_MATRIX_EXECUTION_MAXIMUM_TRACE_EVENTS = 1_024 as const
export const NETWORK_MATRIX_EXECUTION_MAXIMUM_TRACE_BYTES = 8_388_608 as const

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

export interface NetworkMatrixRunnerStarter {
  start(options: NetworkMatrixRunnerOptions): NetworkMatrixRunExecution
}

const SYSTEM_NETWORK_MATRIX_RUNNER: NetworkMatrixRunnerStarter = Object.freeze({
  start: startNetworkMatrix,
})

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
  readonly runner?: NetworkMatrixRunnerStarter
  readonly runnerDeadlines?: NetworkMatrixRunnerDeadlines
  readonly runnerDeadlineScheduler?: NetworkMatrixDeadlineScheduler
  readonly invocationId?: string
}

export interface ExecuteNetworkMatrixExecution {
  readonly result: Promise<ExecuteNetworkMatrixResult>
  readonly runnerTraces: Promise<NetworkMatrixTraceSnapshot | null>
  readonly traces: NetworkMatrixTraceChannel
}

interface ExecutionSettlementState {
  runtimeCleanupOutcome: ExecuteNetworkMatrixResult['runtimeCleanupOutcome']
  runnerTraces: NetworkMatrixTraceSnapshot | null
}

export interface ExecuteNetworkMatrixResult {
  readonly commandOutcome:
    | 'completed'
    | 'runtime-bootstrap-failed'
    | 'collector-failed'
    | 'containment-cleanup-failed'
  readonly runtimeCleanupOutcome: 'completed' | 'failed'
  readonly runnerTraces: NetworkMatrixTraceSnapshot | null
  readonly run: NetworkRunResult
  readonly aggregate: NetworkMatrixAggregate
  readonly publication: NetworkMatrixArtifactPublication
}

/**
 * This is the only execute transaction: it creates the parent-owned collector,
 * derives one aggregate from the exact staged run bytes, and publishes both
 * contracts together. Browser children never receive a filesystem writer.
 */
export function startNetworkMatrixExecution(
  options: ExecuteNetworkMatrixOptions,
): ExecuteNetworkMatrixExecution {
  const runId = requireRunId(options.runId, 'browser network matrix execute run ID')
  const identity = executionTraceIdentity(runId)
  const journal = createNetworkMatrixTraceJournal(
    Object.freeze([identity]),
    NETWORK_MATRIX_EXECUTION_MAXIMUM_TRACE_EVENTS,
    NETWORK_MATRIX_EXECUTION_MAXIMUM_TRACE_BYTES,
    'browser network matrix execution lifecycle trace',
  )
  journal.append(networkMatrixTrace(identity, 'execution-started', 'started', {
    executionMode: options.executionMode,
  }))
  const settlementState: ExecutionSettlementState = {
    runtimeCleanupOutcome: 'completed',
    runnerTraces: null,
  }
  const operation = executeNetworkMatrixOperation(options, journal.append, settlementState).then(
    (settled) => {
      settlementState.runtimeCleanupOutcome = settled.runtimeCleanupOutcome
      const acceptedByHardOracle = settled.commandOutcome === 'completed' &&
        settled.aggregate.evidenceOutcome === 'complete'
      emitTrace(
        journal.append,
        settled.run.runId,
        'execution-terminal',
        acceptedByHardOracle ? 'succeeded' : 'failed',
        {
          commandOutcome: settled.commandOutcome,
          evidenceOutcome: settled.aggregate.evidenceOutcome,
          aggregateHardOracleAccepted: acceptedByHardOracle,
          cleanupOutcome: settled.runtimeCleanupOutcome,
          lastMilestone: 'artifact-publication-completed',
        },
      )
      return settled
    },
    (cause: unknown) => {
      emitTrace(
        journal.append,
        runId,
        'execution-failed',
        'failed',
        opaqueFailureContext('execution-operation-failed'),
      )
      emitTrace(journal.append, runId, 'execution-terminal', 'failed', {
        cleanupOutcome: settlementState.runtimeCleanupOutcome,
        lastMilestone: 'execution-failed',
        ...opaqueFailureContext('execution-operation-failed'),
      })
      throw cause
    },
  )
  const result = settleNetworkMatrixTraceJournal(operation, journal)
  const runnerTraces = result.then(
    (settled) => settled.runnerTraces,
    () => settlementState.runnerTraces,
  )
  return Object.freeze({ result, runnerTraces, traces: journal.view })
}

async function executeNetworkMatrixOperation(
  options: ExecuteNetworkMatrixOptions,
  appendTrace: (trace: NetworkMatrixRunTrace) => void,
  settlementState: ExecutionSettlementState,
): Promise<ExecuteNetworkMatrixResult> {
  const runId = requireRunId(options.runId, 'browser network matrix execute run ID')
  const executionMode = requireEnum(
    options.executionMode,
    NETWORK_MATRIX_EXECUTION_MODES,
    'browser network matrix execution mode',
  )
  const ownership = new NetworkMatrixInvocationOwnershipLedger(
    runId,
    options.invocationId ?? newNetworkMatrixInvocationId(),
  )
  try {
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
  emitTrace(appendTrace, runId, 'runtime-bootstrap-started', 'started', { executionMode })
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
    emitTrace(appendTrace, runId, 'runtime-bootstrap-failed', 'failed', {
      executionMode,
      ...opaqueFailureContext('runtime-bootstrap-failed'),
    })
    const commandOutcome = containsCleanupFailure(cause)
      ? 'containment-cleanup-failed'
      : 'runtime-bootstrap-failed'
    settlementState.runtimeCleanupOutcome = commandOutcome === 'containment-cleanup-failed'
      ? 'failed'
      : 'completed'
    const run = finalizeBootstrapFailure(
      options.registry,
      collector,
      runId,
      executionMode,
      commandOutcome,
    )
    return publishRunAndRetainOwnership(
      options,
      appendTrace,
      ownership,
      run,
      commandOutcome,
      settlementState.runtimeCleanupOutcome,
      null,
    )
  }
  emitTrace(appendTrace, runId, 'runtime-bootstrap-completed', 'succeeded', { executionMode })

  let run: NetworkRunResult | undefined
  let runnerTraces: NetworkMatrixTraceSnapshot | null = null
  let workflowFailure: unknown
  let terminalFailure: 'collector-failed' | 'containment-cleanup-failed' | undefined
  let settlementFailure: unknown
  try {
    if (runtimeOwnership === undefined) {
      workflowFailure = new Error(
        'network matrix runtime ownership handoff did not publish its successor',
      )
    } else {
      const runnerExecution = (options.runner ?? SYSTEM_NETWORK_MATRIX_RUNNER).start({
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
        ownershipRegistrar: ownership,
      })
      let runnerFailure: unknown
      try {
        run = await runnerExecution.result
      } catch (cause) {
        runnerFailure = cause
      }
      runnerTraces = runnerExecution.traces.snapshot()
      settlementState.runnerTraces = runnerTraces
      let traceFailure: unknown
      try {
        requireCompleteRunnerTraceSnapshot(runnerTraces)
      } catch (cause) {
        traceFailure = cause
      }
      workflowFailure = combineFailures(
        runnerFailure,
        traceFailure,
        'network matrix runner and trace evidence both failed',
      )
    }
  } finally {
    settlementFailure = await settleExecutionRuntime(
      options,
      runtime,
      runtimeOwnership,
      appendTrace,
      runId,
      executionMode,
    )
    settlementState.runtimeCleanupOutcome = settlementFailure === undefined
      ? 'completed'
      : 'failed'
  }

  workflowFailure = combineFailures(
    workflowFailure,
    settlementFailure,
    'network matrix execution and runtime cleanup both failed',
  )
  if (workflowFailure !== undefined) {
    terminalFailure = containsCleanupFailure(workflowFailure) || settlementFailure !== undefined
      ? 'containment-cleanup-failed'
      : 'collector-failed'
    emitTrace(appendTrace, runId, 'run-execution-failed', 'failed', {
      executionMode,
      failureCode: terminalFailure,
      ...opaqueFailureContext('run-execution-failed'),
    })
  }
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
      appendTrace,
      ownership,
      run,
      terminalFailure ?? 'completed',
      settlementState.runtimeCleanupOutcome,
      runnerTraces,
    )
  } finally {
    await ownership.retainUntilEmpty(undefined, (_cause, retainedOperationIds) => {
      emitTrace(appendTrace, runId, 'ownership-retry-failed', 'failed', {
        executionMode,
        invocationId: ownership.binding.invocationId,
        retainedOperationIds,
      })
    })
  }
}

async function settleExecutionRuntime(
  options: ExecuteNetworkMatrixOptions,
  runtime: NetworkMatrixExecutionRuntime,
  runtimeOwnership: NetworkMatrixOwnershipRegistration | undefined,
  appendTrace: (trace: NetworkMatrixRunTrace) => void,
  runId: string,
  executionMode: NetworkMatrixExecutionMode,
): Promise<unknown | undefined> {
  emitTrace(appendTrace, runId, 'runtime-close-started', 'started', { executionMode })
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
    emitTrace(appendTrace, runId, 'runtime-close-completed', 'succeeded', { executionMode })
    runtimeOwnership?.normalTerminal()
    return undefined
  } catch (cause) {
    if (!isOwnershipCleanupError(cause)) {
      runtimeOwnership?.forcedTerminal()
    }
    emitTrace(appendTrace, runId, 'runtime-close-failed', 'failed', { executionMode })
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
  appendTrace: (trace: NetworkMatrixRunTrace) => void,
  ownership: NetworkMatrixInvocationOwnershipLedger,
  run: NetworkRunResult,
  commandOutcome: ExecuteNetworkMatrixResult['commandOutcome'],
  runtimeCleanupOutcome: ExecuteNetworkMatrixResult['runtimeCleanupOutcome'],
  runnerTraces: NetworkMatrixTraceSnapshot | null,
): Promise<ExecuteNetworkMatrixResult> {
  const publication = publishRun(
    options,
    appendTrace,
    run,
    commandOutcome,
    runtimeCleanupOutcome,
    runnerTraces,
  ).then(
    (published) => Object.freeze({ outcome: 'published' as const, published }),
    (cause: unknown) => Object.freeze({ outcome: 'failed' as const, cause }),
  )
  // Evidence publication and retained-owner recovery are independent children of
  // the invocation. Starting both prevents a blocked publisher from starving the
  // exact force retry that still owns a runtime, lease, or process subtree.
  const retainedSettlement = ownership.retainUntilEmpty(undefined, (_cause, retainedOperationIds) => {
    emitTrace(appendTrace, run.runId, 'ownership-retry-failed', 'failed', {
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
  appendTrace: (trace: NetworkMatrixRunTrace) => void,
  run: NetworkRunResult,
  commandOutcome: ExecuteNetworkMatrixResult['commandOutcome'],
  runtimeCleanupOutcome: ExecuteNetworkMatrixResult['runtimeCleanupOutcome'],
  runnerTraces: NetworkMatrixTraceSnapshot | null,
): Promise<ExecuteNetworkMatrixResult> {
  const { runId, executionMode } = run
  const runJson = canonicalNetworkRunResultJson(run, options.registry)
  emitTrace(appendTrace, runId, 'artifact-publication-started', 'started', {
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
    emitTrace(appendTrace, runId, 'artifact-publication-failed', 'failed', { executionMode })
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
  emitTrace(appendTrace, runId, 'artifact-publication-completed', 'succeeded', {
    executionMode,
    evidenceOutcome: aggregate.evidenceOutcome,
  })
  return Object.freeze({
    commandOutcome,
    runtimeCleanupOutcome,
    runnerTraces,
    run: publishedRun,
    aggregate,
    publication,
  })
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

function requireCompleteRunnerTraceSnapshot(
  snapshot: NetworkMatrixTraceSnapshot,
): void {
  if (!snapshot.completed) {
    throw new Error('network matrix runner trace journal is incomplete after runner settlement')
  }
  if (
    snapshot.failure !== null ||
    snapshot.truncated ||
    snapshot.observedEvents !== snapshot.capturedEvents ||
    snapshot.observedBytes !== snapshot.capturedBytes
  ) {
    throw new Error('network matrix runner trace journal exceeded or violated its evidence authority')
  }
}

function emitTrace(
  appendTrace: (trace: NetworkMatrixRunTrace) => void,
  runId: string,
  milestone: string,
  outcome: NetworkMatrixRunTrace['outcome'],
  context: Readonly<Record<string, unknown>>,
): void {
  appendTrace(networkMatrixTrace(executionTraceIdentity(runId), milestone, outcome, context))
}

function executionTraceIdentity(runId: string) {
  return Object.freeze({
    component: 'browser-network-matrix-execute' as const,
    scenario: 'network-matrix-execution' as const,
    operationId: `execution-${runId}`,
    runId,
  })
}

function combineFailures(
  first: unknown,
  second: unknown,
  message: string,
): unknown {
  if (first === undefined) return second
  if (second === undefined) return first
  return new AggregateError([first, second], message, { cause: first })
}

function containsCleanupFailure(value: unknown, visited = new Set<unknown>()): boolean {
  if (
    typeof value !== 'object' ||
    value === null ||
    isProxy(value) ||
    visited.has(value)
  ) return false
  visited.add(value)
  if (isOwnershipCleanupError(value)) return true
  if (!(value instanceof AggregateError)) return false
  const errors = Object.getOwnPropertyDescriptor(value, 'errors')
  if (
    errors === undefined ||
    !('value' in errors) ||
    isProxy(errors.value) ||
    !Array.isArray(errors.value)
  ) return false
  return errors.value.some((entry: unknown) => containsCleanupFailure(entry, visited))
}

function isOwnershipCleanupError(
  value: unknown,
): value is NetworkMatrixOwnershipCleanupError {
  return typeof value === 'object' &&
    value !== null &&
    !isProxy(value) &&
    value instanceof NetworkMatrixOwnershipCleanupError
}

type ExecutionTraceFailureCode =
  | 'execution-operation-failed'
  | 'run-execution-failed'
  | 'runtime-bootstrap-failed'

/**
 * Lifecycle evidence records a closed phase code and never introspects a thrown
 * dependency value; cleanup and terminal publication therefore survive hostile
 * Error accessors, toString implementations, and Proxy traps.
 */
function opaqueFailureContext(
  failureCode: ExecutionTraceFailureCode,
): Readonly<Record<string, unknown>> {
  return Object.freeze({ failureCode })
}
