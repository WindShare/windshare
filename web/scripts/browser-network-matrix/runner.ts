import { isProxy } from 'node:util/types'

import {
  NETWORK_MATRIX_ORCHESTRATION_FAILURE_CODES,
  NETWORK_MATRIX_SAMPLE_FAILURE_CODES,
  type NetworkMatrixExecutionMode,
  type NetworkMatrixPrerequisiteOutcome,
  type NetworkMatrixProfileId,
} from './vocabulary.ts'
import {
  networkMatrixIdentities,
  sha256,
  type LoadedNetworkMatrixRegistry,
  type NetworkMatrixIdentity,
  type NetworkMatrixProfileReference,
} from './manifest.ts'
import type { NetworkTopologyProfile } from './profile.ts'
import { networkMatrixSampleOperationId } from './sample-authority.ts'
import type {
  NetworkOrchestrationFailure,
  NetworkRunResult,
  NetworkSampleFailure,
  NetworkSampleResult,
} from './result.ts'
import type { NetworkMatrixSampleObservation } from './run-collector.ts'
import { NetworkMatrixRunCollector } from './run-collector.ts'
import {
  failedNetworkMatrixAuthorityPreparation,
  type NetworkMatrixAuthorityPreparationContext,
  type NetworkMatrixAuthorityResolver,
  type NetworkMatrixExecutionAuthority,
  type PreparedNetworkMatrixAuthority,
} from './runtime-authority.ts'
import {
  networkMatrixError,
  requireEnum,
  requireOperationId,
  requireRunId,
} from './contract-support.ts'
import {
  NetworkMatrixDeadlineExceeded,
  NetworkMatrixOwnershipCleanupError,
  deferredOwnedOperation,
  mapOwnedOperation,
  settleOwnedOperation,
  systemDeadlineScheduler,
  type NetworkMatrixDeadlineScheduler,
  type NetworkMatrixOwnedOperation,
  type NetworkMatrixOwnershipRegistrar,
  type NetworkMatrixOwnershipRegistration,
} from './owned-operation.ts'
import {
  parseNetworkRuntimeAttestation,
  type NetworkRuntimeAttestation,
} from './attestation.ts'
import {
  createNetworkMatrixTraceJournal,
  networkMatrixTrace,
  settleNetworkMatrixTraceJournal,
  type NetworkMatrixTraceChannel,
  type NetworkMatrixTraceEvent,
  type NetworkMatrixTraceIdentity,
  type NetworkMatrixTraceOutcome,
} from './trace/index.ts'

export const NETWORK_MATRIX_AUTHORITY_PREPARATION_DEADLINE_MS = 30_000 as const
export const NETWORK_MATRIX_SAMPLE_EXECUTION_DEADLINE_MS = 180_000 as const
export const NETWORK_MATRIX_AUTHORITY_CLOSE_DEADLINE_MS = 15_000 as const
export const NETWORK_MATRIX_MAXIMUM_TRACE_EVENTS = 512 as const
export const NETWORK_MATRIX_MAXIMUM_TRACE_BYTES = 4_194_304 as const

const NETWORK_MATRIX_AUTHORITY_OPERATION_DOMAIN =
  'windshare.browser-network-matrix.authority-operation/v1' as const

type OrchestrationFailureCode = (typeof NETWORK_MATRIX_ORCHESTRATION_FAILURE_CODES)[number]

export interface NetworkMatrixRunnerDeadlines {
  readonly authorityPreparationMs: number
  readonly sampleExecutionMs: number
  readonly authorityCloseMs: number
}

export const NETWORK_MATRIX_RUNNER_DEADLINES: NetworkMatrixRunnerDeadlines = Object.freeze({
  authorityPreparationMs: NETWORK_MATRIX_AUTHORITY_PREPARATION_DEADLINE_MS,
  sampleExecutionMs: NETWORK_MATRIX_SAMPLE_EXECUTION_DEADLINE_MS,
  authorityCloseMs: NETWORK_MATRIX_AUTHORITY_CLOSE_DEADLINE_MS,
})

export interface NetworkMatrixSampleExecutionContext {
  readonly runId: string
  readonly manifestSha256: string
  readonly identity: NetworkMatrixIdentity
  readonly profile: NetworkTopologyProfile
  readonly authority: NetworkMatrixExecutionAuthority
  readonly operationId: string
}

export interface NetworkMatrixSampleExecution {
  readonly processInstanceId: string
  readonly observation: NetworkMatrixSampleObservation
}

export interface NetworkMatrixSampleExecutor {
  execute(
    context: NetworkMatrixSampleExecutionContext,
  ): NetworkMatrixOwnedOperation<NetworkMatrixSampleExecution>
}

/** Consumer-side boundary keeps authority cleanup testable when evidence storage fails. */
export interface NetworkMatrixRunCollectorPort {
  recordAttestation(attestation: NetworkRuntimeAttestation): NetworkRuntimeAttestation
  recordSample(
    identity: NetworkMatrixIdentity,
    processInstanceId: string | null,
    observation: NetworkMatrixSampleObservation,
  ): NetworkSampleResult
  finalize(orchestrationFailure: NetworkOrchestrationFailure | null): NetworkRunResult
}

export type NetworkMatrixRunTrace = NetworkMatrixTraceEvent

export interface NetworkMatrixRunExecution {
  readonly result: Promise<NetworkRunResult>
  readonly traces: NetworkMatrixTraceChannel
}

export interface NetworkMatrixRunnerOptions {
  readonly registry: LoadedNetworkMatrixRegistry
  readonly runId: string
  readonly executionMode: NetworkMatrixExecutionMode
  readonly authorities: NetworkMatrixAuthorityResolver
  readonly samples: NetworkMatrixSampleExecutor
  readonly collector?: NetworkMatrixRunCollectorPort
  readonly deadlines?: NetworkMatrixRunnerDeadlines
  readonly deadlineScheduler?: NetworkMatrixDeadlineScheduler
  readonly ownershipRegistrar?: NetworkMatrixOwnershipRegistrar
}

export class NetworkMatrixSampleExecutionError extends Error {
  readonly failureCode: NetworkSampleFailure['failureCode']

  constructor(failureCode: NetworkSampleFailure['failureCode'], message: string) {
    super(message)
    this.name = 'NetworkMatrixSampleExecutionError'
    this.failureCode = requireEnum(
      failureCode,
      NETWORK_MATRIX_SAMPLE_FAILURE_CODES,
      'network matrix sample execution failure code',
    )
  }
}

export class NetworkMatrixOrchestrationError extends Error {
  readonly failureCode: OrchestrationFailureCode

  constructor(failureCode: OrchestrationFailureCode, message: string) {
    super(message)
    this.name = 'NetworkMatrixOrchestrationError'
    this.failureCode = requireEnum(
      failureCode,
      NETWORK_MATRIX_ORCHESTRATION_FAILURE_CODES,
      'network matrix orchestration failure code',
    )
  }
}

type RunnerCleanupOutcome = 'not-required' | 'completed' | 'failed'

interface PreparedProfile {
  readonly reference: NetworkMatrixProfileReference
  readonly profile: NetworkTopologyProfile
  readonly authority: PreparedNetworkMatrixAuthority
  readonly ownership?: NetworkMatrixOwnershipRegistration
  closed: boolean
}

interface ProfileCloseResult {
  readonly failureCode: OrchestrationFailureCode | null
  readonly cleanupOutcome: Exclude<RunnerCleanupOutcome, 'not-required'>
}

interface ProfileLifecycleState {
  item?: PreparedProfile
  failure?: unknown
  lastMilestone: string
  cleanupOutcome: RunnerCleanupOutcome
}

interface RunnerContext {
  readonly options: NetworkMatrixRunnerOptions
  readonly runId: string
  readonly appendTrace: (trace: NetworkMatrixRunTrace) => void
  readonly deadlines: NetworkMatrixRunnerDeadlines
  readonly scheduler: NetworkMatrixDeadlineScheduler
  readonly collector: NetworkMatrixRunCollectorPort
}

interface RunnerState {
  readonly processInstances: Set<string>
  readonly terminalSampleTraceIds: Set<string>
  orchestrationFailure: OrchestrationFailureCode | null
}

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

async function executeProfilesSequentially(
  context: RunnerContext,
  state: RunnerState,
  acquired: PreparedProfile[],
): Promise<void> {
  const references = context.options.registry.manifest.profiles.filter(
    ({ executionMode }) => executionMode === context.options.executionMode,
  )
  let firstFailure: unknown
  for (const reference of references) {
    const failure = await executeProfileLifecycle(
      context,
      state,
      acquired,
      reference,
      firstFailure !== undefined,
    )
    firstFailure ??= failure
  }
  if (firstFailure !== undefined) throw firstFailure
}

async function executeProfileLifecycle(
  context: RunnerContext,
  state: RunnerState,
  acquired: PreparedProfile[],
  reference: NetworkMatrixProfileReference,
  hasPriorFailure: boolean,
): Promise<unknown> {
  const identity = profileTraceIdentity(context.runId, reference.profileId)
  const suppressedByPriorFailure = state.orchestrationFailure !== null || hasPriorFailure
  const acquiredIndex = acquired.length
  const lifecycle: ProfileLifecycleState = {
    item: undefined,
    failure: undefined,
    lastMilestone: 'profile-started',
    cleanupOutcome: 'not-required',
  }
  emitTrace(context.appendTrace, identity, lifecycle.lastMilestone, 'started', {
    authorityKind: reference.authorityKind,
    suppressedByPriorFailure,
  })
  try {
    await executeProfileWorkflow(
      context,
      state,
      acquired,
      reference,
      suppressedByPriorFailure,
      identity,
      lifecycle,
    )
  } catch (cause) {
    lifecycle.failure = cause
    state.orchestrationFailure ??= profileExecutionFailureCode(cause)
    lifecycle.lastMilestone = 'profile-execution-failed'
    emitTrace(
      context.appendTrace,
      identity,
      lifecycle.lastMilestone,
      'failed',
      opaqueFailureContext('profile-execution-failed'),
    )
  } finally {
    await finalizeProfileLifecycle(
      context,
      state,
      acquired,
      acquiredIndex,
      reference,
      suppressedByPriorFailure,
      identity,
      lifecycle,
    )
  }
  return lifecycle.failure
}

async function executeProfileWorkflow(
  context: RunnerContext,
  state: RunnerState,
  acquired: PreparedProfile[],
  reference: NetworkMatrixProfileReference,
  suppressedByPriorFailure: boolean,
  identity: NetworkMatrixTraceIdentity,
  lifecycle: ProfileLifecycleState,
): Promise<void> {
  lifecycle.item = suppressedByPriorFailure
    ? registerSyntheticProfile(context, reference, acquired)
    : await prepareProfile(context, reference, state, acquired)
  lifecycle.lastMilestone = suppressedByPriorFailure
    ? 'authority-preparation-skipped'
    : 'authority-preparation-terminal'
  const profileFailure = await executeProfileSamples(
    context,
    lifecycle.item,
    state,
    suppressedByPriorFailure ? state.orchestrationFailure ?? 'collector-failed' : null,
  )
  state.orchestrationFailure ??= profileFailure
  lifecycle.lastMilestone = 'profile-samples-settled'
  emitTrace(
    context.appendTrace,
    identity,
    lifecycle.lastMilestone,
    profileFailure === null ? 'succeeded' : 'failed',
    { failureCode: profileFailure },
  )
}

async function finalizeProfileLifecycle(
  context: RunnerContext,
  state: RunnerState,
  acquired: PreparedProfile[],
  acquiredIndex: number,
  reference: NetworkMatrixProfileReference,
  suppressedByPriorFailure: boolean,
  identity: NetworkMatrixTraceIdentity,
  lifecycle: ProfileLifecycleState,
): Promise<void> {
  terminalizeRemainingProfileSamples(
    context,
    state,
    reference.profileId,
    lifecycle.failure === undefined ? 'profile-not-executable' : 'profile-execution-failed',
  )
  lifecycle.item ??= acquired[acquiredIndex]
  if (lifecycle.item !== undefined && !lifecycle.item.closed) {
    try {
      const close = await closeProfile(context, lifecycle.item)
      lifecycle.cleanupOutcome = close.cleanupOutcome
      state.orchestrationFailure ??= close.failureCode
    } catch (cause) {
      lifecycle.failure = combinedFailure(lifecycle.failure, cause)
      lifecycle.cleanupOutcome = 'failed'
      state.orchestrationFailure ??= 'containment-cleanup-failed'
      emitTrace(
        context.appendTrace,
        identity,
        'authority-close-failed',
        'failed',
        opaqueFailureContext('authority-close-failed'),
      )
    }
  }
  const prerequisiteOutcome = lifecycle.item?.authority.attestation.prerequisiteOutcome
  emitTrace(
    context.appendTrace,
    identity,
    'profile-terminal',
    profileLifecycleOutcome(
      lifecycle.failure,
      suppressedByPriorFailure,
      state.orchestrationFailure,
      prerequisiteOutcome,
    ),
    {
      cleanupOutcome: lifecycle.cleanupOutcome,
      lastMilestone: lifecycle.lastMilestone,
      prerequisiteOutcome: prerequisiteOutcome ?? null,
      orchestrationFailure: state.orchestrationFailure,
    },
  )
}

function profileExecutionFailureCode(cause: unknown): OrchestrationFailureCode {
  return isOwnershipCleanupError(cause)
    ? 'containment-cleanup-failed'
    : 'collector-failed'
}

function profileLifecycleOutcome(
  failure: unknown,
  suppressedByPriorFailure: boolean,
  orchestrationFailure: OrchestrationFailureCode | null,
  prerequisiteOutcome: NetworkMatrixPrerequisiteOutcome | undefined,
): NetworkMatrixTraceOutcome {
  if (failure !== undefined || !suppressedByPriorFailure && orchestrationFailure !== null) {
    return 'failed'
  }
  if (suppressedByPriorFailure || prerequisiteOutcome !== 'satisfied') return 'skipped'
  return 'succeeded'
}

async function prepareProfile(
  context: RunnerContext,
  reference: NetworkMatrixProfileReference,
  state: RunnerState,
  acquired: PreparedProfile[],
): Promise<PreparedProfile> {
  const profile = context.options.registry.profiles.find(
    ({ profileId }) => profileId === reference.profileId,
  )
  if (profile === undefined) networkMatrixError(`loaded profile ${reference.profileId} is absent`)
  const authorityContext = Object.freeze({
    registry: context.options.registry,
    reference,
    profile,
    runId: context.runId,
    signal: new AbortController().signal,
  })
  emitTrace(
    context.appendTrace,
    profileTraceIdentity(context.runId, reference.profileId),
    'authority-preparation-started',
    'started',
    { authorityKind: reference.authorityKind },
  )
  let authority: PreparedNetworkMatrixAuthority
  let ownership: NetworkMatrixOwnershipRegistration | undefined
  try {
    const acquisition = await prepareAuthority(context, authorityContext)
    authority = acquisition.authority
    ownership = acquisition.ownership
  } catch (cause) {
    if (!(isOwnershipCleanupError(cause))) throw cause
    state.orchestrationFailure ??= 'containment-cleanup-failed'
    authority = failedNetworkMatrixAuthorityPreparation(authorityContext)
  }
  const item: PreparedProfile = {
    reference,
    profile,
    authority,
    ...(ownership === undefined ? {} : { ownership }),
    closed: false,
  }
  // Ownership is registered before collector code can throw, so the outer
  // cleanup fence always sees every successfully acquired authority.
  acquired.push(item)
  context.collector.recordAttestation(authority.attestation)
  emitTrace(
    context.appendTrace,
    profileTraceIdentity(context.runId, reference.profileId),
    'authority-preparation-terminal',
    authority.attestation.prerequisiteOutcome === 'satisfied' ? 'succeeded' : 'skipped',
    { prerequisiteOutcome: authority.attestation.prerequisiteOutcome },
  )
  return item
}

function registerSyntheticProfile(
  context: RunnerContext,
  reference: NetworkMatrixProfileReference,
  acquired: PreparedProfile[],
): PreparedProfile {
  const profile = context.options.registry.profiles.find(
    ({ profileId }) => profileId === reference.profileId,
  )
  if (profile === undefined) networkMatrixError(`loaded profile ${reference.profileId} is absent`)
  const authorityContext = Object.freeze({
    registry: context.options.registry,
    reference,
    profile,
    runId: context.runId,
    signal: new AbortController().signal,
  })
  const authority = failedNetworkMatrixAuthorityPreparation(authorityContext)
  const ownership = registerPreparedAuthorityOwnership(context, authority, profile.profileId)
  const item: PreparedProfile = {
    reference,
    profile,
    authority,
    ...(ownership === undefined ? {} : { ownership }),
    closed: false,
  }
  acquired.push(item)
  context.collector.recordAttestation(authority.attestation)
  emitTrace(
    context.appendTrace,
    profileTraceIdentity(context.runId, reference.profileId),
    'authority-preparation-skipped',
    'skipped',
    { prerequisiteOutcome: authority.attestation.prerequisiteOutcome },
  )
  return item
}

async function prepareAuthority(
  context: RunnerContext,
  authorityContext: NetworkMatrixAuthorityPreparationContext,
): Promise<{
  readonly authority: PreparedNetworkMatrixAuthority
  readonly ownership?: NetworkMatrixOwnershipRegistration
}> {
  let successorRegistration: NetworkMatrixOwnershipRegistration | undefined
  try {
    const operation = mapOwnedOperation(
      deferredOwnedOperation(() => context.options.authorities.prepare(authorityContext)),
      (prepared) => normalizePreparedAuthority(prepared, authorityContext),
    )
    const authority = await settleOwnedOperation(
      operation,
      'authority-prepare',
      context.deadlines.authorityPreparationMs,
      context.scheduler,
      context.options.ownershipRegistrar === undefined
        ? undefined
        : Object.freeze({
            registrar: context.options.ownershipRegistrar,
            operationId: authorityOperationId(
              'prepare',
              context.runId,
              authorityContext.profile.profileId,
            ),
            successor: (prepared: PreparedNetworkMatrixAuthority) => Object.freeze({
              operationId: authorityOperationId(
                'live',
                context.runId,
                authorityContext.profile.profileId,
              ),
              operationClass: 'authority-close' as const,
              forceTerminateAndWait: (reason: Parameters<
                PreparedNetworkMatrixAuthority['forceTerminateAndWait']
              >[0]) => prepared.forceTerminateAndWait(reason),
            }),
            onSuccessorRegistered: (registration: NetworkMatrixOwnershipRegistration) => {
              successorRegistration = registration
            },
          }),
    )
    return Object.freeze({
      authority,
      ...(successorRegistration === undefined ? {} : { ownership: successorRegistration }),
    })
  } catch (cause) {
    emitTrace(
      context.appendTrace,
      profileTraceIdentity(context.runId, authorityContext.profile.profileId),
      'authority-preparation-operation-failed',
      'failed',
      opaqueFailureContext('authority-preparation-failed'),
    )
    if (isOwnershipCleanupError(cause)) throw cause
    return Object.freeze({ authority: failedNetworkMatrixAuthorityPreparation(authorityContext) })
  }
}

function normalizePreparedAuthority(
  prepared: PreparedNetworkMatrixAuthority,
  context: NetworkMatrixAuthorityPreparationContext,
): PreparedNetworkMatrixAuthority {
  const attestation = parseNetworkRuntimeAttestation(prepared.attestation, {
    manifest: context.registry.manifest,
    manifestSha256: context.registry.manifestSha256,
    runId: context.runId,
  })
  const satisfied = attestation.prerequisiteOutcome === 'satisfied'
  if (
    satisfied !== (prepared.execution !== null) ||
    (prepared.execution !== null && prepared.execution.profileId !== context.profile.profileId) ||
    typeof prepared.close !== 'function' ||
    typeof prepared.forceTerminateAndWait !== 'function'
  ) networkMatrixError('authority preparation contradicts its attestation or profile')
  return Object.freeze({
    attestation,
    execution: prepared.execution,
    close: prepared.close,
    forceTerminateAndWait: prepared.forceTerminateAndWait,
  })
}

async function executeProfileSamples(
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

async function closeProfile(
  context: RunnerContext,
  item: PreparedProfile,
): Promise<ProfileCloseResult> {
  emitTrace(
    context.appendTrace,
    profileTraceIdentity(context.runId, item.profile.profileId),
    'authority-close-started',
    'started',
  )
  item.closed = true
  let operation: NetworkMatrixOwnedOperation<void>
  try {
    operation = item.authority.close()
  } catch {
    const fallbackOutcome = await forceProfileAuthorityClosed(item)
    emitTrace(
      context.appendTrace,
      profileTraceIdentity(context.runId, item.profile.profileId),
      'authority-close-failed',
      'failed',
      { closeFactoryOutcome: 'threw', fallbackOutcome },
    )
    return Object.freeze({
      failureCode: 'containment-cleanup-failed',
      cleanupOutcome: fallbackOutcome,
    })
  }
  try {
    await settleOwnedOperation(
      operation,
      'authority-close',
      context.deadlines.authorityCloseMs,
      context.scheduler,
    )
  } catch (cause) {
    let fallbackOutcome: RunnerCleanupOutcome = 'not-required'
    if (!isOwnershipCleanupError(cause)) {
      fallbackOutcome = await forceProfileAuthorityClosed(item)
    }
    emitTrace(
      context.appendTrace,
      profileTraceIdentity(context.runId, item.profile.profileId),
      'authority-close-failed',
      'failed',
      { fallbackOutcome },
    )
    return closeFailureResult(cause, fallbackOutcome)
  }
  item.ownership?.normalTerminal()
  emitTrace(
    context.appendTrace,
    profileTraceIdentity(context.runId, item.profile.profileId),
    'authority-close-completed',
    'succeeded',
  )
  return Object.freeze({
    failureCode: null,
    cleanupOutcome: 'completed',
  })
}

async function forceProfileAuthorityClosed(
  item: PreparedProfile,
): Promise<Exclude<RunnerCleanupOutcome, 'not-required'>> {
  try {
    await item.authority.forceTerminateAndWait('authority-close')
    item.ownership?.forcedTerminal()
    return 'completed'
  } catch {
    return 'failed'
  }
}

function closeFailureResult(
  cause: unknown,
  fallbackOutcome: RunnerCleanupOutcome,
): ProfileCloseResult {
  const cleanupOutcome = fallbackOutcome === 'completed' ? 'completed' : 'failed'
  if (fallbackOutcome === 'failed' || isOwnershipCleanupError(cause)) {
    return Object.freeze({ failureCode: 'containment-cleanup-failed', cleanupOutcome })
  }
  if (isDeadlineExceeded(cause)) {
    return Object.freeze({ failureCode: 'orchestrator-deadline-exceeded', cleanupOutcome })
  }
  return Object.freeze({ failureCode: 'collector-failed', cleanupOutcome })
}

function registerPreparedAuthorityOwnership(
  context: RunnerContext,
  authority: PreparedNetworkMatrixAuthority,
  profileId: NetworkMatrixProfileId,
): NetworkMatrixOwnershipRegistration | undefined {
  return context.options.ownershipRegistrar?.register({
    operationId: authorityOperationId('live', context.runId, profileId),
    operationClass: 'authority-close',
    forceTerminateAndWait: (reason) => authority.forceTerminateAndWait(reason),
  })
}

function ownershipAuthority(
  context: RunnerContext,
  operationId: string,
): { readonly registrar: NetworkMatrixOwnershipRegistrar; readonly operationId: string } | undefined {
  return context.options.ownershipRegistrar === undefined
    ? undefined
    : Object.freeze({ registrar: context.options.ownershipRegistrar, operationId })
}

async function closeRemainingProfiles(
  context: RunnerContext,
  prepared: readonly PreparedProfile[],
  state: RunnerState,
): Promise<void> {
  for (const item of prepared) {
    if (item.closed) continue
    const close = await closeProfile(context, item)
    state.orchestrationFailure ??= close.failureCode
  }
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

function terminalizeRemainingProfileSamples(
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

function expectedRunnerTraceIdentities(
  options: NetworkMatrixRunnerOptions,
  runId: string,
): readonly NetworkMatrixTraceIdentity[] {
  const profiles = options.registry.manifest.profiles
    .filter(({ executionMode }) => executionMode === options.executionMode)
    .map(({ profileId }) => profileTraceIdentity(runId, profileId))
  const samples = networkMatrixIdentities(options.registry.manifest, options.executionMode)
    .map((identity) => sampleTraceIdentity(networkMatrixSampleOperationId(runId, identity), runId, identity))
  return Object.freeze([
    runTraceIdentity(runId),
    ...profiles,
    ...samples,
  ])
}

/**
 * Authority ownership outlives individual calls, so its process identifier must
 * stay globally reconstructible while remaining bounded at the maximum run ID.
 */
function authorityOperationId(
  phase: 'prepare' | 'live',
  runId: string,
  profileId: NetworkMatrixProfileId,
): string {
  const digest = sha256(`${JSON.stringify([
    NETWORK_MATRIX_AUTHORITY_OPERATION_DOMAIN,
    phase,
    requireRunId(runId, 'network matrix authority operation run ID'),
    profileId,
  ])}\n`)
  return requireOperationId(
    `authority-${phase}-${digest}`,
    'network matrix authority operation ID',
  )
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

function runTraceIdentity(runId: string): NetworkMatrixTraceIdentity {
  return Object.freeze({
    component: 'browser-network-matrix-runner',
    scenario: 'network-matrix-run',
    operationId: runId,
    runId,
  })
}

function profileTraceIdentity(
  runId: string,
  profileId: NetworkMatrixIdentity['profileId'],
): NetworkMatrixTraceIdentity {
  return Object.freeze({
    component: 'browser-network-matrix-runner',
    scenario: 'network-matrix-profile',
    operationId: `${runId}-${profileId}`,
    runId,
    profileId,
  })
}

function sampleTraceIdentity(
  operationId: string,
  runId: string,
  identity: NetworkMatrixIdentity,
): NetworkMatrixTraceIdentity {
  return Object.freeze({
    component: 'browser-network-matrix-runner',
    scenario: 'network-matrix-sample',
    operationId,
    runId,
    profileId: identity.profileId,
    browser: identity.browser,
    sampleOrdinal: identity.sampleOrdinal,
  })
}

function emitTrace(
  appendTrace: (trace: NetworkMatrixRunTrace) => void,
  identity: NetworkMatrixTraceIdentity,
  milestone: string,
  outcome: NetworkMatrixTraceOutcome,
  context?: Readonly<Record<string, unknown>>,
): void {
  appendTrace(networkMatrixTrace(identity, milestone, outcome, context))
}

type RunnerTraceFailureCode =
  | 'authority-close-failed'
  | 'authority-preparation-failed'
  | 'profile-execution-failed'
  | 'result-finalization-failed'
  | 'run-cleanup-failed'
  | 'run-terminal-failed'
  | 'sample-cleanup-failed'
  | 'sample-execution-failed'
  | 'sample-operation-failed'

/**
 * Dependency causes are deliberately opaque. A thrown value may own hostile
 * accessors or Proxy traps, while the phase-specific code remains a stable and
 * sufficient reconstruction key alongside the lifecycle milestone.
 */
function opaqueFailureContext(
  failureCode: RunnerTraceFailureCode,
): Readonly<Record<string, unknown>> {
  return Object.freeze({ failureCode })
}

function isOwnershipCleanupError(
  value: unknown,
): value is NetworkMatrixOwnershipCleanupError {
  return typeof value === 'object' &&
    value !== null &&
    !isProxy(value) &&
    value instanceof NetworkMatrixOwnershipCleanupError
}

function isDeadlineExceeded(value: unknown): value is NetworkMatrixDeadlineExceeded {
  return typeof value === 'object' &&
    value !== null &&
    !isProxy(value) &&
    value instanceof NetworkMatrixDeadlineExceeded
}

function isOrchestrationError(value: unknown): value is NetworkMatrixOrchestrationError {
  return typeof value === 'object' &&
    value !== null &&
    !isProxy(value) &&
    value instanceof NetworkMatrixOrchestrationError
}

function isSampleExecutionError(value: unknown): value is NetworkMatrixSampleExecutionError {
  return typeof value === 'object' &&
    value !== null &&
    !isProxy(value) &&
    value instanceof NetworkMatrixSampleExecutionError
}

function combinedFailure(first: unknown, second: unknown): unknown {
  return first === undefined
    ? second
    : new AggregateError([first, second], 'network matrix execution and cleanup both failed')
}
