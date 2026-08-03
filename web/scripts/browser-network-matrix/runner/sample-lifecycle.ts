import type { NetworkMatrixProfileId } from '../vocabulary.ts'
import {
  networkMatrixIdentities,
  type NetworkMatrixIdentity,
} from '../manifest.ts'
import { networkMatrixSampleOperationId } from '../sample-authority.ts'
import type {
  NetworkSampleFailure,
  NetworkSampleResult,
} from '../result.ts'
import {
  networkMatrixError,
  requireRunId,
} from '../contract-support.ts'
import {
  deferredOwnedOperation,
  settleOwnedOperation,
} from '../owned-operation.ts'
import type {
  NetworkMatrixTraceIdentity,
  NetworkMatrixTraceOutcome,
} from '../trace/index.ts'
import {
  NetworkMatrixSampleExecutionError,
  type NetworkMatrixRunCollectorPort,
  type NetworkMatrixSampleExecution,
  type OrchestrationFailureCode,
  type PreparedProfile,
  type RunnerCleanupOutcome,
  type RunnerContext,
  type RunnerState,
} from './contract.ts'
import {
  emitTrace,
  isDeadlineExceeded,
  isOrchestrationError,
  isOwnershipCleanupError,
  isSampleExecutionError,
  opaqueFailureContext,
  sampleTraceIdentity,
} from './trace-support.ts'

export async function executeProfileSamples(
  context: RunnerContext,
  item: PreparedProfile,
  state: RunnerState,
  initialSuppression: OrchestrationFailureCode | null,
): Promise<OrchestrationFailureCode | null> {
  const identities = profileSampleIdentities(context, item.profile.profileId)
  let suppression = initialSuppression
  let executionFailure: unknown
  for (const identity of identities) {
    if (suppression !== null || item.authority.execution === null) {
      emitSkippedSampleLifecycle(
        context,
        state,
        identity,
        suppression === null ? 'prerequisite-unsatisfied' : suppression,
      )
      continue
    }
    try {
      suppression = await executeSample(context, item, identity, state)
    } catch (cause) {
      executionFailure = cause
      suppression = isOwnershipCleanupError(cause)
        ? 'containment-cleanup-failed'
        : 'collector-failed'
    }
  }
  if (executionFailure !== undefined) throw executionFailure
  return suppression
}

async function executeSample(
  context: RunnerContext,
  item: PreparedProfile,
  identity: NetworkMatrixIdentity,
  state: RunnerState,
): Promise<OrchestrationFailureCode | null> {
  const authority = item.authority.execution
  if (authority === null) networkMatrixError('unsatisfied authority reached sample execution')
  const operationId = networkMatrixSampleOperationId(context.runId, identity)
  const traceIdentity = sampleTraceIdentity(operationId, context.runId, identity)
  let terminalOutcome: NetworkMatrixTraceOutcome = 'failed'
  let settledCleanupOutcome: RunnerCleanupOutcome = 'not-required'
  let lastMilestone = 'sample-started'
  let terminalContext: Readonly<Record<string, unknown>> = Object.freeze({
    cleanupOutcome: settledCleanupOutcome,
    lastMilestone,
  })
  emitTrace(context.appendTrace, traceIdentity, 'sample-started', 'started', {
    runtimeKind: authority.runtimeKind,
  })
  try {
    const attempt = await executeSampleOperation(context, item, identity, operationId, traceIdentity)
    settledCleanupOutcome = attempt.cleanupOutcome
    lastMilestone = attempt.lastMilestone
    if (attempt.orchestrationFailure !== null) {
      terminalContext = Object.freeze({
        cleanupOutcome: attempt.cleanupOutcome,
        lastMilestone: attempt.lastMilestone,
        processInstanceId: null,
        sampleOutcome: 'not-recorded',
        failureCode: attempt.orchestrationFailure,
      })
      return attempt.orchestrationFailure
    }
    if (attempt.execution !== null) {
      try {
        registerProcessInstance(attempt.execution.processInstanceId, state.processInstances)
      } catch (cause) {
        attempt.failureCode = sampleFailureCode(cause)
      }
    }
    const recorded = recordExecution(
      context.collector,
      identity,
      attempt.execution,
      attempt.failureCode,
    )
    terminalOutcome = sampleHardOracleTraceOutcome(recorded)
    terminalContext = Object.freeze({
      cleanupOutcome: attempt.cleanupOutcome,
      lastMilestone: attempt.lastMilestone,
      processInstanceId: attempt.execution?.processInstanceId ?? null,
      sampleOutcome: recorded.sampleOutcome,
      attemptId: recorded.attemptEvidence?.attemptAuthority.attemptId ?? null,
      pionAuthority: recorded.attemptEvidence?.pionAuthority ?? null,
      challengeBindingSha256: recorded.attemptEvidence?.challenge?.bindingSha256 ?? null,
      candidatePolicyOutcome: recorded.candidatePolicyOutcome,
      failureCode: recorded.failure?.failureCode ?? null,
    })
    return null
  } catch (cause) {
    terminalContext = Object.freeze({
      cleanupOutcome: settledCleanupOutcome,
      lastMilestone,
      ...opaqueFailureContext('sample-operation-failed'),
    })
    throw cause
  } finally {
    emitTrace(context.appendTrace, traceIdentity, 'sample-terminal', terminalOutcome, terminalContext)
    state.terminalSampleTraceIds.add(operationId)
  }
}

interface SampleAttempt {
  readonly execution: NetworkMatrixSampleExecution | null
  failureCode: NetworkSampleFailure['failureCode'] | null
  readonly orchestrationFailure: OrchestrationFailureCode | null
  readonly cleanupOutcome: 'completed' | 'failed'
  readonly lastMilestone: string
}

async function executeSampleOperation(
  context: RunnerContext,
  item: PreparedProfile,
  identity: NetworkMatrixIdentity,
  operationId: string,
  traceIdentity: NetworkMatrixTraceIdentity,
): Promise<SampleAttempt> {
  const authority = item.authority.execution
  if (authority === null) networkMatrixError('unsatisfied authority reached sample operation')
  emitTrace(context.appendTrace, traceIdentity, 'sample-execution-started', 'started', {
    deadlineMs: context.deadlines.sampleExecutionMs,
  })
  try {
    const execution = await settleOwnedOperation(
      deferredOwnedOperation(() => context.options.samples.execute({
        runId: context.runId,
        manifestSha256: context.options.registry.manifestSha256,
        identity,
        profile: item.profile,
        authority,
        operationId,
      })),
      'sample-execute',
      context.deadlines.sampleExecutionMs,
      context.scheduler,
      ownershipAuthority(context, operationId),
    )
    emitTrace(context.appendTrace, traceIdentity, 'sample-execution-completed', 'succeeded')
    return {
      execution,
      failureCode: null,
      orchestrationFailure: null,
      cleanupOutcome: 'completed',
      lastMilestone: 'sample-execution-completed',
    }
  } catch (cause) {
    const primaryFailure = isOwnershipCleanupError(cause)
      ? cause.primaryFailure
      : cause
    const lastMilestone = sampleFailureMilestone(primaryFailure)
    emitTrace(
      context.appendTrace,
      traceIdentity,
      lastMilestone,
      'failed',
      opaqueFailureContext('sample-execution-failed'),
    )
    if (isOwnershipCleanupError(cause)) {
      emitTrace(
        context.appendTrace,
        traceIdentity,
        'sample-cleanup-failed',
        'failed',
        opaqueFailureContext('sample-cleanup-failed'),
      )
      return {
        execution: null,
        failureCode: null,
        orchestrationFailure: 'containment-cleanup-failed',
        cleanupOutcome: 'failed',
        lastMilestone,
      }
    }
    if (isOrchestrationError(cause)) {
      return {
        execution: null,
        failureCode: null,
        orchestrationFailure: cause.failureCode,
        cleanupOutcome: 'completed',
        lastMilestone,
      }
    }
    return {
      execution: null,
      failureCode: sampleFailureCode(cause),
      orchestrationFailure: null,
      cleanupOutcome: 'completed',
      lastMilestone,
    }
  }
}

function sampleFailureMilestone(cause: unknown): string {
  if (isDeadlineExceeded(cause)) return 'sample-execution-deadline-exceeded'
  if (isOrchestrationError(cause)) return 'sample-orchestration-failed'
  return 'sample-execution-failed'
}

function sampleFailureCode(cause: unknown): NetworkSampleFailure['failureCode'] {
  if (isDeadlineExceeded(cause)) return 'sample-deadline-exceeded'
  if (isSampleExecutionError(cause)) return cause.failureCode
  return 'sample-runner-failed'
}

function registerProcessInstance(instanceIdValue: string, observed: Set<string>): void {
  const instanceId = requireRunId(instanceIdValue, 'network matrix browser process instance ID')
  if (observed.has(instanceId)) {
    throw new NetworkMatrixSampleExecutionError(
      'sample-runner-failed',
      'browser process instance was reused across matrix samples',
    )
  }
  observed.add(instanceId)
}

function ownershipAuthority(
  context: RunnerContext,
  operationId: string,
): { readonly registrar: NonNullable<RunnerContext['options']['ownershipRegistrar']>; readonly operationId: string } | undefined {
  return context.options.ownershipRegistrar === undefined
    ? undefined
    : Object.freeze({ registrar: context.options.ownershipRegistrar, operationId })
}

function profileSampleIdentities(
  context: RunnerContext,
  profileId: NetworkMatrixProfileId,
): readonly NetworkMatrixIdentity[] {
  return networkMatrixIdentities(
    context.options.registry.manifest,
    context.options.executionMode,
  ).filter((identity) => identity.profileId === profileId)
}

export function terminalizeRemainingProfileSamples(
  context: RunnerContext,
  state: RunnerState,
  profileId: NetworkMatrixProfileId,
  reason: string,
): void {
  for (const identity of profileSampleIdentities(context, profileId)) {
    const operationId = networkMatrixSampleOperationId(context.runId, identity)
    if (!state.terminalSampleTraceIds.has(operationId)) {
      emitSkippedSampleLifecycle(context, state, identity, reason)
    }
  }
}

function emitSkippedSampleLifecycle(
  context: RunnerContext,
  state: RunnerState,
  identity: NetworkMatrixIdentity,
  reason: string,
): void {
  const operationId = networkMatrixSampleOperationId(context.runId, identity)
  if (state.terminalSampleTraceIds.has(operationId)) return
  const traceIdentity = sampleTraceIdentity(operationId, context.runId, identity)
  emitTrace(context.appendTrace, traceIdentity, 'sample-started', 'started', {
    executionSuppressed: true,
  })
  emitTrace(context.appendTrace, traceIdentity, 'sample-execution-skipped', 'skipped', {
    reason,
  })
  emitTrace(context.appendTrace, traceIdentity, 'sample-terminal', 'skipped', {
    cleanupOutcome: 'not-required',
    lastMilestone: 'sample-execution-skipped',
    processInstanceId: null,
    sampleOutcome: 'not-recorded',
    candidatePolicyOutcome: 'not-evaluated',
    failureCode: null,
    skipReason: reason,
  })
  state.terminalSampleTraceIds.add(operationId)
}

function sampleHardOracleTraceOutcome(
  sample: NetworkSampleResult,
): NetworkMatrixTraceOutcome {
  return sample.sampleOutcome === 'observed' && sample.candidatePolicyOutcome === 'matched'
    ? 'succeeded'
    : 'failed'
}

function recordExecution(
  collector: NetworkMatrixRunCollectorPort,
  identity: NetworkMatrixIdentity,
  execution: NetworkMatrixSampleExecution | null,
  failureCode: NetworkSampleFailure['failureCode'] | null,
) {
  if (failureCode !== null || execution === null) {
    return collector.recordSample(identity, null, {
      sampleOutcome: 'infrastructure-failed',
      failureCode: failureCode ?? 'sample-runner-failed',
    })
  }
  try {
    return collector.recordSample(identity, execution.processInstanceId, execution.observation)
  } catch {
    return collector.recordSample(identity, null, {
      sampleOutcome: 'infrastructure-failed',
      failureCode: 'evidence-collection-failed',
    })
  }
}
