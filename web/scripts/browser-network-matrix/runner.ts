import {
  NETWORK_MATRIX_ORCHESTRATION_FAILURE_CODES,
  NETWORK_MATRIX_SAMPLE_FAILURE_CODES,
  type NetworkMatrixExecutionMode,
  type NetworkMatrixProfileId,
} from './vocabulary.ts'
import {
  networkMatrixIdentities,
  type LoadedNetworkMatrixRegistry,
  type NetworkMatrixIdentity,
  type NetworkMatrixProfileReference,
} from './manifest.ts'
import type { NetworkTopologyProfile } from './profile.ts'
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

export const NETWORK_MATRIX_AUTHORITY_PREPARATION_DEADLINE_MS = 30_000 as const
export const NETWORK_MATRIX_SAMPLE_EXECUTION_DEADLINE_MS = 180_000 as const
export const NETWORK_MATRIX_AUTHORITY_CLOSE_DEADLINE_MS = 15_000 as const

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

export interface NetworkMatrixRunTrace {
  readonly operationId: string
  readonly milestone: string
  readonly runId: string
  readonly profileId?: NetworkMatrixIdentity['profileId']
  readonly browser?: NetworkMatrixIdentity['browser']
  readonly sampleOrdinal?: NetworkMatrixIdentity['sampleOrdinal']
  readonly context?: Readonly<Record<string, unknown>>
}

export type NetworkMatrixRunTraceSink = (trace: NetworkMatrixRunTrace) => void

export interface NetworkMatrixRunnerOptions {
  readonly registry: LoadedNetworkMatrixRegistry
  readonly runId: string
  readonly executionMode: NetworkMatrixExecutionMode
  readonly authorities: NetworkMatrixAuthorityResolver
  readonly samples: NetworkMatrixSampleExecutor
  readonly collector?: NetworkMatrixRunCollectorPort
  readonly deadlines?: NetworkMatrixRunnerDeadlines
  readonly deadlineScheduler?: NetworkMatrixDeadlineScheduler
  readonly trace?: NetworkMatrixRunTraceSink
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

interface PreparedProfile {
  readonly reference: NetworkMatrixProfileReference
  readonly profile: NetworkTopologyProfile
  readonly authority: PreparedNetworkMatrixAuthority
  readonly ownership?: NetworkMatrixOwnershipRegistration
  closed: boolean
}

interface RunnerContext {
  readonly options: NetworkMatrixRunnerOptions
  readonly runId: string
  readonly trace: NetworkMatrixRunTraceSink
  readonly deadlines: NetworkMatrixRunnerDeadlines
  readonly scheduler: NetworkMatrixDeadlineScheduler
  readonly collector: NetworkMatrixRunCollectorPort
}

interface RunnerState {
  readonly processInstances: Set<string>
  orchestrationFailure: OrchestrationFailureCode | null
}

/**
 * A process-instance identity is unique across the frozen expansion. Together
 * with the owned-operation boundary this makes five samples mean five reaped,
 * isolated browser processes—not repeated assertions in one browser context.
 */
export async function runNetworkMatrix(
  options: NetworkMatrixRunnerOptions,
): Promise<NetworkRunResult> {
  const context = runnerContext(options)
  const state: RunnerState = { processInstances: new Set(), orchestrationFailure: null }
  const acquired: PreparedProfile[] = []
  emitRunStarted(context)
  try {
    await executeProfilesSequentially(context, state, acquired)
  } finally {
    await closeRemainingProfiles(context, acquired, state)
  }
  const result = context.collector.finalize(state.orchestrationFailure === null
    ? null
    : { failureCode: state.orchestrationFailure })
  emitTrace(context.trace, {
    operationId: context.runId,
    milestone: 'run-terminal',
    runId: context.runId,
  }, {
    orchestrationOutcome: result.orchestrationOutcome,
    runOutcome: result.runOutcome,
    observedSamples: result.samples.length,
  })
  return result
}

function runnerContext(options: NetworkMatrixRunnerOptions): RunnerContext {
  const runId = requireRunId(options.runId, 'browser network matrix runner run ID')
  return {
    options,
    runId,
    trace: options.trace ?? defaultTraceSink,
    deadlines: options.deadlines ?? NETWORK_MATRIX_RUNNER_DEADLINES,
    scheduler: options.deadlineScheduler ?? systemDeadlineScheduler,
    collector: options.collector ?? new NetworkMatrixRunCollector({
      registry: options.registry,
      runId,
      executionMode: options.executionMode,
    }),
  }
}

function emitRunStarted(context: RunnerContext): void {
  emitTrace(context.trace, {
    operationId: context.runId,
    milestone: 'run-started',
    runId: context.runId,
  }, {
    executionMode: context.options.executionMode,
    expectedSamples: networkMatrixIdentities(
      context.options.registry.manifest,
      context.options.executionMode,
    ).length,
    deadlines: context.deadlines,
  })
}

async function executeProfilesSequentially(
  context: RunnerContext,
  state: RunnerState,
  acquired: PreparedProfile[],
): Promise<void> {
  const references = context.options.registry.manifest.profiles.filter(
    ({ executionMode }) => executionMode === context.options.executionMode,
  )
  for (const reference of references) {
    const item = state.orchestrationFailure === null
      ? await prepareProfile(context, reference, state, acquired)
      : registerSyntheticProfile(context, reference, acquired)
    if (state.orchestrationFailure === null) {
      state.orchestrationFailure = await executeProfileSamples(
        context,
        item,
        state.processInstances,
      )
    }
    const closeFailure = await closeProfile(context, item)
    state.orchestrationFailure ??= closeFailure
  }
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
    context.trace,
    profileTrace(context.runId, reference.profileId, 'authority-preparation-started'),
    { authorityKind: reference.authorityKind },
  )
  let authority: PreparedNetworkMatrixAuthority
  let ownership: NetworkMatrixOwnershipRegistration | undefined
  try {
    const acquisition = await prepareAuthority(context, authorityContext)
    authority = acquisition.authority
    ownership = acquisition.ownership
  } catch (cause) {
    if (!(cause instanceof NetworkMatrixOwnershipCleanupError)) throw cause
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
    context.trace,
    profileTrace(context.runId, reference.profileId, 'authority-preparation-terminal'),
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
    context.trace,
    profileTrace(context.runId, reference.profileId, 'authority-preparation-skipped'),
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
            operationId:
              `authority-prepare-${context.runId}-${authorityContext.profile.profileId}`,
            successor: (prepared: PreparedNetworkMatrixAuthority) => Object.freeze({
              operationId:
                `authority-live-${context.runId}-${authorityContext.profile.profileId}`,
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
      context.trace,
      profileTrace(
        context.runId,
        authorityContext.profile.profileId,
        'authority-preparation-operation-failed',
      ),
      {
        failureType: cause instanceof Error ? cause.name : typeof cause,
        failureMessage: cause instanceof Error ? cause.message.slice(0, 512) : 'non-error failure',
      },
    )
    if (cause instanceof NetworkMatrixOwnershipCleanupError) throw cause
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
  processInstances: Set<string>,
): Promise<OrchestrationFailureCode | null> {
  if (item.authority.execution === null) return null
  const identities = networkMatrixIdentities(
    context.options.registry.manifest,
    context.options.executionMode,
  ).filter(({ profileId }) => profileId === item.profile.profileId)
  for (const identity of identities) {
    const failure = await executeSample(context, item, identity, processInstances)
    if (failure !== null) return failure
  }
  return null
}

async function executeSample(
  context: RunnerContext,
  item: PreparedProfile,
  identity: NetworkMatrixIdentity,
  processInstances: Set<string>,
): Promise<OrchestrationFailureCode | null> {
  const authority = item.authority.execution
  if (authority === null) networkMatrixError('unsatisfied authority reached sample execution')
  const operationId = sampleOperationId(context.runId, identity)
  emitTrace(context.trace, sampleTrace(operationId, context.runId, identity, 'sample-started'), {
    runtimeKind: authority.runtimeKind,
  })
  const attempt = await executeSampleOperation(context, item, identity, operationId)
  if (attempt.orchestrationFailure !== null) return attempt.orchestrationFailure
  if (attempt.execution !== null) {
    try {
      registerProcessInstance(attempt.execution.processInstanceId, processInstances)
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
  emitTrace(context.trace, sampleTrace(operationId, context.runId, identity, 'sample-terminal'), {
    processInstanceId: attempt.execution?.processInstanceId ?? null,
    sampleOutcome: recorded.sampleOutcome,
    attemptId: recorded.attemptEvidence?.attemptAuthority.attemptId ?? null,
    pionAuthority: recorded.attemptEvidence?.pionAuthority ?? null,
    challengeBindingSha256: recorded.attemptEvidence?.challenge?.bindingSha256 ?? null,
    candidatePolicyOutcome: recorded.candidatePolicyOutcome,
    failureCode: recorded.failure?.failureCode ?? null,
  })
  return null
}

interface SampleAttempt {
  readonly execution: NetworkMatrixSampleExecution | null
  failureCode: NetworkSampleFailure['failureCode'] | null
  readonly orchestrationFailure: OrchestrationFailureCode | null
}

async function executeSampleOperation(
  context: RunnerContext,
  item: PreparedProfile,
  identity: NetworkMatrixIdentity,
  operationId: string,
): Promise<SampleAttempt> {
  const authority = item.authority.execution
  if (authority === null) networkMatrixError('unsatisfied authority reached sample operation')
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
    return { execution, failureCode: null, orchestrationFailure: null }
  } catch (cause) {
    if (cause instanceof NetworkMatrixOwnershipCleanupError) {
      return {
        execution: null,
        failureCode: null,
        orchestrationFailure: 'containment-cleanup-failed',
      }
    }
    if (cause instanceof NetworkMatrixOrchestrationError) {
      return { execution: null, failureCode: null, orchestrationFailure: cause.failureCode }
    }
    return { execution: null, failureCode: sampleFailureCode(cause), orchestrationFailure: null }
  }
}

function sampleFailureCode(cause: unknown): NetworkSampleFailure['failureCode'] {
  if (cause instanceof NetworkMatrixDeadlineExceeded) return 'sample-deadline-exceeded'
  if (cause instanceof NetworkMatrixSampleExecutionError) return cause.failureCode
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
): Promise<OrchestrationFailureCode | null> {
  emitTrace(
    context.trace,
    profileTrace(context.runId, item.profile.profileId, 'authority-close-started'),
  )
  item.closed = true
  let operation: NetworkMatrixOwnedOperation<void>
  try {
    operation = item.authority.close()
  } catch {
    let fallbackOutcome: 'completed' | 'failed' = 'completed'
    try {
      await item.authority.forceTerminateAndWait('authority-close')
      item.ownership?.forcedTerminal()
    } catch {
      fallbackOutcome = 'failed'
    }
    emitTrace(
      context.trace,
      profileTrace(context.runId, item.profile.profileId, 'authority-close-failed'),
      { closeFactoryOutcome: 'threw', fallbackOutcome },
    )
    return 'containment-cleanup-failed'
  }
  try {
    await settleOwnedOperation(
      operation,
      'authority-close',
      context.deadlines.authorityCloseMs,
      context.scheduler,
    )
  } catch (cause) {
    let fallbackOutcome: 'not-required' | 'completed' | 'failed' = 'not-required'
    if (!(cause instanceof NetworkMatrixOwnershipCleanupError)) {
      fallbackOutcome = 'completed'
      try {
        await item.authority.forceTerminateAndWait('authority-close')
        item.ownership?.forcedTerminal()
      } catch {
        fallbackOutcome = 'failed'
      }
    }
    emitTrace(
      context.trace,
      profileTrace(context.runId, item.profile.profileId, 'authority-close-failed'),
      { fallbackOutcome },
    )
    if (fallbackOutcome === 'failed') return 'containment-cleanup-failed'
    if (cause instanceof NetworkMatrixOwnershipCleanupError) return 'containment-cleanup-failed'
    return cause instanceof NetworkMatrixDeadlineExceeded
      ? 'orchestrator-deadline-exceeded'
      : 'collector-failed'
  }
  item.ownership?.normalTerminal()
  emitTrace(
    context.trace,
    profileTrace(context.runId, item.profile.profileId, 'authority-close-completed'),
  )
  return null
}

function registerPreparedAuthorityOwnership(
  context: RunnerContext,
  authority: PreparedNetworkMatrixAuthority,
  profileId: NetworkMatrixProfileId,
): NetworkMatrixOwnershipRegistration | undefined {
  return context.options.ownershipRegistrar?.register({
    operationId: `authority-live-${context.runId}-${profileId}`,
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
    const closeFailure = await closeProfile(context, item)
    state.orchestrationFailure ??= closeFailure
  }
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

function sampleOperationId(runId: string, identity: NetworkMatrixIdentity): string {
  return `${runId}-${identity.profileId}-${identity.browser}-${identity.sampleOrdinal}`
}

function profileTrace(
  runId: string,
  profileId: NetworkMatrixIdentity['profileId'],
  milestone: string,
): NetworkMatrixRunTrace {
  return Object.freeze({ operationId: `${runId}-${profileId}`, milestone, runId, profileId })
}

function sampleTrace(
  operationId: string,
  runId: string,
  identity: NetworkMatrixIdentity,
  milestone: string,
): NetworkMatrixRunTrace {
  return Object.freeze({
    operationId,
    milestone,
    runId,
    profileId: identity.profileId,
    browser: identity.browser,
    sampleOrdinal: identity.sampleOrdinal,
  })
}

function emitTrace(
  sink: NetworkMatrixRunTraceSink,
  trace: NetworkMatrixRunTrace,
  context?: Readonly<Record<string, unknown>>,
): void {
  const event = Object.freeze({ ...trace, ...(context === undefined ? {} : { context }) })
  try {
    sink(event)
  } catch {
    try {
      defaultTraceSink(Object.freeze({
        ...trace,
        milestone: 'trace-sink-failed',
        context: Object.freeze({ failedMilestone: trace.milestone }),
      }))
    } catch {
      // Observability is isolated from evidence authority.
    }
  }
}

function defaultTraceSink(trace: NetworkMatrixRunTrace): void {
  process.stderr.write(`${JSON.stringify({ component: 'browser-network-matrix-runner', ...trace })}\n`)
}
