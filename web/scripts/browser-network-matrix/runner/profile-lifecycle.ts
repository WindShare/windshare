import type {
  NetworkMatrixPrerequisiteOutcome,
  NetworkMatrixProfileId,
} from '../vocabulary.ts'
import type { NetworkMatrixProfileReference } from '../manifest.ts'
import {
  failedNetworkMatrixAuthorityPreparation,
  type NetworkMatrixAuthorityPreparationContext,
  type PreparedNetworkMatrixAuthority,
} from '../runtime-authority.ts'
import { networkMatrixError } from '../contract-support.ts'
import {
  deferredOwnedOperation,
  mapOwnedOperation,
  settleOwnedOperation,
  type NetworkMatrixOwnedOperation,
  type NetworkMatrixOwnershipRegistration,
} from '../owned-operation.ts'
import { parseNetworkRuntimeAttestation } from '../attestation.ts'
import type {
  NetworkMatrixTraceIdentity,
  NetworkMatrixTraceOutcome,
} from '../trace/index.ts'
import {
  type OrchestrationFailureCode,
  type PreparedProfile,
  type RunnerCleanupOutcome,
  type RunnerContext,
  type RunnerState,
} from './contract.ts'
import {
  executeProfileSamples,
  terminalizeRemainingProfileSamples,
} from './sample-lifecycle.ts'
import {
  authorityOperationId,
  combinedFailure,
  emitTrace,
  isDeadlineExceeded,
  isOwnershipCleanupError,
  opaqueFailureContext,
  profileTraceIdentity,
} from './trace-support.ts'

interface ProfileCloseResult {
  readonly failureCode: OrchestrationFailureCode | null
  readonly cleanupOutcome: Exclude<RunnerCleanupOutcome, 'not-required'>
}

interface ProfileLifecycleState {
  item: PreparedProfile | undefined
  failure: unknown | undefined
  lastMilestone: string
  cleanupOutcome: RunnerCleanupOutcome
}

export async function executeProfilesSequentially(
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

export async function closeRemainingProfiles(
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
