import { networkMatrixIdentities } from './manifest.ts'
import type { NetworkRunResult } from './result.ts'
import { NetworkMatrixRunCollector } from './run-collector.ts'
import { requireRunId } from './contract-support.ts'
import { systemDeadlineScheduler } from './owned-operation.ts'
import {
  createNetworkMatrixTraceJournal,
  settleNetworkMatrixTraceJournal,
} from './trace/index.ts'
import {
  NETWORK_MATRIX_MAXIMUM_TRACE_BYTES,
  NETWORK_MATRIX_MAXIMUM_TRACE_EVENTS,
  NETWORK_MATRIX_RUNNER_DEADLINES,
  type NetworkMatrixRunExecution,
  type NetworkMatrixRunnerOptions,
  type NetworkMatrixRunTrace,
  type PreparedProfile,
  type RunnerContext,
  type RunnerState,
} from './runner/contract.ts'
import {
  closeRemainingProfiles,
  executeProfilesSequentially,
} from './runner/profile-lifecycle.ts'
import {
  combinedFailure,
  emitTrace,
  expectedRunnerTraceIdentities,
  opaqueFailureContext,
  runTraceIdentity,
} from './runner/trace-support.ts'

export {
  NETWORK_MATRIX_AUTHORITY_CLOSE_DEADLINE_MS,
  NETWORK_MATRIX_AUTHORITY_PREPARATION_DEADLINE_MS,
  NETWORK_MATRIX_MAXIMUM_TRACE_BYTES,
  NETWORK_MATRIX_MAXIMUM_TRACE_EVENTS,
  NETWORK_MATRIX_RUNNER_DEADLINES,
  NETWORK_MATRIX_SAMPLE_EXECUTION_DEADLINE_MS,
  NetworkMatrixOrchestrationError,
  NetworkMatrixSampleExecutionError,
} from './runner/contract.ts'
export type {
  NetworkMatrixRunCollectorPort,
  NetworkMatrixRunExecution,
  NetworkMatrixRunnerDeadlines,
  NetworkMatrixRunnerOptions,
  NetworkMatrixRunTrace,
  NetworkMatrixSampleExecution,
  NetworkMatrixSampleExecutionContext,
  NetworkMatrixSampleExecutor,
} from './runner/contract.ts'

/**
 * A process-instance identity is unique across the frozen expansion. Together
 * with the owned-operation boundary this makes five samples mean five reaped,
 * isolated browser processes—not repeated assertions in one browser context.
 */
export function startNetworkMatrix(
  options: NetworkMatrixRunnerOptions,
): NetworkMatrixRunExecution {
  const runId = requireRunId(options.runId, 'browser network matrix runner run ID')
  const journal = createNetworkMatrixTraceJournal(
    expectedRunnerTraceIdentities(options, runId),
    NETWORK_MATRIX_MAXIMUM_TRACE_EVENTS,
    NETWORK_MATRIX_MAXIMUM_TRACE_BYTES,
    'browser network matrix lifecycle trace',
  )
  const result = settleNetworkMatrixTraceJournal(
    runNetworkMatrixOperation(options, journal.append),
    journal,
  )
  return Object.freeze({ result, traces: journal.view })
}

async function runNetworkMatrixOperation(
  options: NetworkMatrixRunnerOptions,
  appendTrace: (trace: NetworkMatrixRunTrace) => void,
): Promise<NetworkRunResult> {
  const context = runnerContext(options, appendTrace)
  const state: RunnerState = {
    processInstances: new Set(),
    terminalSampleTraceIds: new Set(),
    orchestrationFailure: null,
  }
  const acquired: PreparedProfile[] = []
  const identity = runTraceIdentity(context.runId)
  let lastMilestone = 'run-started'
  emitTrace(context.appendTrace, identity, lastMilestone, 'started', {
    executionMode: context.options.executionMode,
    expectedSamples: networkMatrixIdentities(
      context.options.registry.manifest,
      context.options.executionMode,
    ).length,
    deadlines: context.deadlines,
  })
  let failure: unknown
  try {
    await executeProfilesSequentially(context, state, acquired)
    lastMilestone = 'run-profiles-settled'
    emitTrace(context.appendTrace, identity, lastMilestone, 'succeeded')
  } catch (cause) {
    failure = cause
    lastMilestone = 'run-profile-execution-failed'
    emitTrace(
      context.appendTrace,
      identity,
      lastMilestone,
      'failed',
      opaqueFailureContext('profile-execution-failed'),
    )
  }
  try {
    await closeRemainingProfiles(context, acquired, state)
    emitTrace(context.appendTrace, identity, 'run-cleanup-settled',
      state.orchestrationFailure === 'containment-cleanup-failed' ? 'failed' : 'succeeded', {
        cleanupOutcome: state.orchestrationFailure === 'containment-cleanup-failed'
          ? 'failed'
          : 'completed',
      })
  } catch (cause) {
    failure = combinedFailure(failure, cause)
    state.orchestrationFailure ??= 'containment-cleanup-failed'
    emitTrace(
      context.appendTrace,
      identity,
      'run-cleanup-failed',
      'failed',
      opaqueFailureContext('run-cleanup-failed'),
    )
  }
  let result: NetworkRunResult | undefined
  if (failure === undefined) {
    try {
      result = context.collector.finalize(state.orchestrationFailure === null
        ? null
        : { failureCode: state.orchestrationFailure })
      lastMilestone = 'run-result-finalized'
      emitTrace(context.appendTrace, identity, lastMilestone, 'succeeded', {
        orchestrationOutcome: result.orchestrationOutcome,
        runOutcome: result.runOutcome,
        observedSamples: result.samples.length,
      })
    } catch (cause) {
      failure = cause
      lastMilestone = 'run-result-finalization-failed'
      emitTrace(
        context.appendTrace,
        identity,
        lastMilestone,
        'failed',
        opaqueFailureContext('result-finalization-failed'),
      )
    }
  }
  const cleanupOutcome = state.orchestrationFailure === 'containment-cleanup-failed'
    ? 'failed'
    : 'completed'
  emitTrace(
    context.appendTrace,
    identity,
    'run-terminal',
    failure === undefined && result?.orchestrationOutcome !== 'failed' ? 'succeeded' : 'failed',
    {
      cleanupOutcome,
      lastMilestone,
      ...(result === undefined
        ? { failure: opaqueFailureContext('run-terminal-failed') }
        : {
            orchestrationOutcome: result.orchestrationOutcome,
            runOutcome: result.runOutcome,
            observedSamples: result.samples.length,
          }),
    },
  )
  if (failure !== undefined) throw failure
  if (result === undefined) throw new Error('network matrix runner did not produce a terminal result')
  return result
}

function runnerContext(
  options: NetworkMatrixRunnerOptions,
  appendTrace: (trace: NetworkMatrixRunTrace) => void,
): RunnerContext {
  const runId = requireRunId(options.runId, 'browser network matrix runner run ID')
  return {
    options,
    runId,
    appendTrace,
    deadlines: options.deadlines ?? NETWORK_MATRIX_RUNNER_DEADLINES,
    scheduler: options.deadlineScheduler ?? systemDeadlineScheduler,
    collector: options.collector ?? new NetworkMatrixRunCollector({
      registry: options.registry,
      runId,
      executionMode: options.executionMode,
    }),
  }
}
