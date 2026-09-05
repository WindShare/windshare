import {
  lifecycleDeadline,
  nextReceiveLifecycleState,
  stableDeadline,
  type PlanKind,
  type ReceiveLifecycleState,
  type RetainedLifecycleKind,
} from './state'
import type { LifecycleEvent, LifecycleReducerContext, LifecycleReduction } from './lifecycle/events'
import {
  gateDirectZipRecovery,
  pauseDirectZip,
  resumeDirectZipRecovery,
} from './lifecycle/recovery'
import {
  activeLeaseMismatch,
  applied,
  needsAttention,
  requireClock,
  requireState,
  requireWorkspaceState,
} from './lifecycle/transitions'

export type { LifecycleEvent, LifecycleReducerContext, LifecycleReduction } from './lifecycle/events'

export function reduceReceiveLifecycle(
  state: ReceiveLifecycleState,
  event: LifecycleEvent,
  context: LifecycleReducerContext,
): LifecycleReduction {
  requireClock(context.nowMilliseconds)
  const authorityReacquisition = event.kind === 'receive-authority-reacquired'
  if (event.expectedGeneration !== state.generation || event.leaseId !== context.activeLeaseId ||
      (!authorityReacquisition && activeLeaseMismatch(state, context.activeLeaseId))) {
    return Object.freeze({ status: 'stale', state })
  }
  const deadline = lifecycleDeadline(state)
  if (deadline !== undefined && context.nowMilliseconds >= deadline &&
      (event.kind === 'resume-started' || event.kind === 'save-requested' ||
       event.kind === 'handoff-requested' || event.kind === 'direct-zip-recovery-gated' ||
       event.kind === 'direct-zip-recovery-resumed')) {
    throw new TypeError('stable lifecycle deadline elapsed before continuation')
  }
  if (event.kind === 'direct-zip-pause-verified') {
    return applied(pauseDirectZip(state, event, context))
  }
  if (event.kind === 'direct-zip-recovery-gated') {
    return applied(gateDirectZipRecovery(state, event, context))
  }
  if (event.kind === 'direct-zip-recovery-resumed') {
    return applied(resumeDirectZipRecovery(state, context))
  }
  switch (event.kind) {
    case 'receive-started': return applied(startReceive(state, event, context))
    case 'preparation-admitted': return applied(admitPreparation(state, context))
    case 'preparation-rejected': return applied(rejectPreparation(state, event))
    case 'pause-verified': return applied(pauseVerified(state, event, context))
    case 'resume-started': return applied(resumeStable(state, event, context))
    case 'resume-admission-failed': return applied(restoreReceiveContinuation(state, event, context))
    case 'receive-authority-reacquired': return applied(reacquireReceiveAuthority(state, context))
    case 'stop-requested': return applied(stopReceive(state, event, context.planKind))
    case 'discovery-completed': return applied(discoveryCompleted(state, context))
    case 'tree-finalization-completed': return applied(finalizeTree(state, event, context))
    case 'materialization-completed': return applied(materializationCompleted(state, context))
    case 'restart-boundary-verified': return applied(restartRequired(state, event, context.planKind))
    case 'materialization-seal-verified': return applied(sealMaterialization(state, event, context))
    case 'package-started': return applied(startPackage(state, event, context))
    case 'package-retryable-failure': return applied(pausePackage(state, event, context.nowMilliseconds))
    case 'package-seal-verified': return applied(sealPackage(state, event))
    case 'wait-record-persisted': return applied(waitToSave(state, context.nowMilliseconds))
    case 'save-requested': return applied(startManagedPublication(state, event, context))
    case 'publication-committed': return applied(commitPublication(state, event))
    case 'publication-not-committed':
      return applied(publicationNotCommitted(state, context.nowMilliseconds))
    case 'publication-unknown':
      requireWorkspaceState(state, context.planKind, 'publishing-managed')
      return applied(needsAttention(state, 'publication-unknown', event.lastVerifiedRecordDigest))
    case 'handoff-unknown':
      requireWorkspaceState(state, context.planKind, 'handing-off')
      if (state.attemptKind !== 'workspace') {
        throw new TypeError('portable handoff cannot persist an unknown outcome')
      }
      return applied(needsAttention(state, 'publication-unknown', event.lastVerifiedRecordDigest))
    case 'handoff-requested': return applied(startHandoff(state, event, context))
    case 'handoff-started': return applied(handoffStarted(state))
    case 'handoff-not-started': return handoffNotStarted(state, event, context.nowMilliseconds)
    case 'cleanup-verified': return applied(cleanupVerified(state, event.cleanupReceiptDigest))
    case 'cleanup-unknown':
      return applied(needsAttention(state, 'cleanup-unknown', event.lastVerifiedRecordDigest))
    case 'expiry-observed': return expireState(state, event, context.nowMilliseconds)
    case 'ownership-unknown':
      return applied(needsAttention(
        state,
        'target-ownership-unknown',
        event.lastVerifiedRecordDigest,
      ))
    case 'pause-requested':
    case 'selected-entry-failed':
    case 'materialization-failed':
    case 'discard-requested':
    case 'abandoned-operation-observed':
      return applied(state)
  }
}

function reacquireReceiveAuthority(
  state: ReceiveLifecycleState,
  context: LifecycleReducerContext,
): ReceiveLifecycleState {
  requireState(state, 'receiving')
  if (context.planKind !== 'direct-tree') {
    throw new TypeError('receive authority reacquisition is exclusive to DirectTree')
  }
  return nextReceiveLifecycleState(state, {
    kind: 'receiving',
    activeLeaseId: context.activeLeaseId,
  })
}

function startReceive(
  state: ReceiveLifecycleState,
  event: Extract<LifecycleEvent, { kind: 'receive-started' }>,
  context: LifecycleReducerContext,
): ReceiveLifecycleState {
  requireState(state, 'intent-frozen')
  if ((context.planKind === 'direct-tree' || context.planKind === 'direct-atomic') &&
      context.preparationRequired) {
    throw new TypeError('direct plans do not acquire a preparation content gate')
  }
  if (context.planKind === 'portable-handoff' && !context.preparationRequired) {
    throw new TypeError('portable handoff requires sealed preparation')
  }
  if (context.preparationRequired) {
    if (event.preparationId === undefined) throw new TypeError('preparation identity is required')
    return nextReceiveLifecycleState(state, {
      kind: 'preparing',
      preparationId: event.preparationId,
    })
  }
  if (event.preparationId !== undefined) throw new TypeError('unprepared plan cannot persist preparation')
  return nextReceiveLifecycleState(state, {
    kind: 'receiving',
    activeLeaseId: context.activeLeaseId,
  })
}

function admitPreparation(
  state: ReceiveLifecycleState,
  context: LifecycleReducerContext,
): ReceiveLifecycleState {
  requireState(state, 'preparing')
  if (!context.preparationRequired ||
      (context.planKind !== 'workspace-then-publish' &&
       context.planKind !== 'portable-handoff')) {
    throw new TypeError('plan does not accept preparation')
  }
  return nextReceiveLifecycleState(state, {
    kind: 'receiving',
    activeLeaseId: context.activeLeaseId,
  })
}

function rejectPreparation(
  state: ReceiveLifecycleState,
  event: Extract<LifecycleEvent, { kind: 'preparation-rejected' }>,
): ReceiveLifecycleState {
  requireState(state, 'preparing')
  return nextReceiveLifecycleState(state, {
    kind: 'discarded',
    cleanupReceiptDigest: event.cleanupReceiptDigest,
  })
}

function pauseVerified(
  state: ReceiveLifecycleState,
  event: Extract<LifecycleEvent, { kind: 'pause-verified' }>,
  context: LifecycleReducerContext,
): ReceiveLifecycleState {
  const expiresAt = stableDeadline(context.nowMilliseconds)
  if (event.stage === 'receive') {
    requireState(state, 'receiving')
    if (context.planKind !== 'direct-tree' &&
        context.planKind !== 'workspace-then-publish') {
      throw new TypeError('plan cannot persist receive continuation')
    }
    return nextReceiveLifecycleState(state, {
      kind: 'resumable-receive',
      payloadKind: 'file-set',
      checkpointSetDigest: event.checkpointSetDigest,
      completedFileCount: event.completedFileCount,
      completedBytes: event.completedBytes,
      selectionFacts: event.selectionFacts,
      expiresAt,
      ...(event.partialReceiptDigest === undefined
        ? {}
        : { partialReceiptDigest: event.partialReceiptDigest }),
    })
  }
  requireWorkspaceState(state, context.planKind, 'packaging')
  return nextReceiveLifecycleState(state, {
    kind: 'resumable-package',
    sealedMaterializationDigest: event.sealedMaterializationDigest,
    tempCleanupProofDigest: event.tempCleanupProofDigest,
    expiresAt,
  })
}

function resumeStable(
  state: ReceiveLifecycleState,
  event: Extract<LifecycleEvent, { kind: 'resume-started' }>,
  context: LifecycleReducerContext,
): ReceiveLifecycleState {
  if (state.kind === 'resumable-receive') {
    const directZipResume = context.planKind === 'direct-resumable-zip' &&
      state.payloadKind === 'direct-zip'
    const fileSetResume = (context.planKind === 'direct-tree' ||
      context.planKind === 'workspace-then-publish') && state.payloadKind === 'file-set'
    if (!directZipResume && !fileSetResume) {
      throw new TypeError('plan cannot resume receive state')
    }
    if (event.packageTempObjectId !== undefined) {
      throw new TypeError('receive resume cannot allocate a package object')
    }
    return nextReceiveLifecycleState(state, {
      kind: 'receiving',
      activeLeaseId: context.activeLeaseId,
    })
  }
  if (state.kind === 'resumable-package') {
    if (context.planKind !== 'workspace-then-publish') {
      throw new TypeError('plan cannot resume package state')
    }
    if (event.packageTempObjectId === undefined) {
      throw new TypeError('package resume requires a new owned package allocation')
    }
    return nextReceiveLifecycleState(state, {
      kind: 'packaging',
      activeLeaseId: context.activeLeaseId,
      sealedMaterializationDigest: state.sealedMaterializationDigest,
      packageTempObjectId: event.packageTempObjectId,
    })
  }
  throw new TypeError('resume-started requires a resumable state')
}

function restoreReceiveContinuation(
  state: ReceiveLifecycleState,
  event: Extract<LifecycleEvent, { kind: 'resume-admission-failed' }>,
  context: LifecycleReducerContext,
): ReceiveLifecycleState {
  requireState(state, 'receiving')
  if (context.planKind !== 'direct-tree' && context.planKind !== 'workspace-then-publish') {
    throw new TypeError('plan cannot restore a receive continuation')
  }
  return nextReceiveLifecycleState(state, {
    kind: 'resumable-receive',
    payloadKind: 'file-set',
    checkpointSetDigest: event.checkpointSetDigest,
    completedFileCount: event.completedFileCount,
    completedBytes: event.completedBytes,
    selectionFacts: event.selectionFacts,
    expiresAt: event.expiresAt,
    ...(event.partialReceiptDigest === undefined
      ? {}
      : { partialReceiptDigest: event.partialReceiptDigest }),
  })
}

function stopReceive(
  state: ReceiveLifecycleState,
  event: Extract<LifecycleEvent, { kind: 'stop-requested' }>,
  planKind: PlanKind,
): ReceiveLifecycleState {
  if (state.kind !== 'receiving' && state.kind !== 'resumable-receive') {
    throw new TypeError('stop-requested requires receive state')
  }
  if (planKind !== 'direct-tree') {
    throw new TypeError('stop-requested is exclusive to DirectTree operations')
  }
  if (event.successCount === 0n) {
    return nextReceiveLifecycleState(state, {
      kind: 'discarded',
      cleanupReceiptDigest: event.cleanupReceiptDigest,
    })
  }
  return nextReceiveLifecycleState(state, {
    kind: 'partial-directory',
    reason: 'stopped',
    successCount: event.successCount,
    failureCount: event.failureCount,
    receiptDigest: event.receiptDigest,
  })
}

function discoveryCompleted(
  state: ReceiveLifecycleState,
  context: LifecycleReducerContext,
): ReceiveLifecycleState {
  requireState(state, 'receiving')
  if (context.planKind === 'direct-tree') {
    return nextReceiveLifecycleState(state, {
      kind: 'finalizing-tree',
      activeLeaseId: context.activeLeaseId,
    })
  }
  if (context.planKind === 'direct-atomic') {
    return nextReceiveLifecycleState(state, {
      kind: 'committing-atomic',
      activeLeaseId: context.activeLeaseId,
    })
  }
  throw new TypeError('discovery completion is not a workspace/portable state boundary')
}

function finalizeTree(
  state: ReceiveLifecycleState,
  event: Extract<LifecycleEvent, { kind: 'tree-finalization-completed' }>,
  context: LifecycleReducerContext,
): ReceiveLifecycleState {
  requireState(state, 'finalizing-tree')
  if (context.planKind !== 'direct-tree') {
    throw new TypeError('tree finalization is exclusive to DirectTree')
  }
  switch (event.outcome) {
    case 'published':
      if (event.failureCount !== 0n) throw new TypeError('published tree contains failures')
      return nextReceiveLifecycleState(state, {
        kind: 'published',
        receiptDigest: event.receiptDigest,
        cleanupState: 'cleanup-pending',
      })
    case 'resumable':
      if (event.retryable !== true) {
        throw new TypeError('resumable tree lacks retryable checkpoint evidence')
      }
      return nextReceiveLifecycleState(state, {
        kind: 'resumable-receive',
        payloadKind: 'file-set',
        checkpointSetDigest: event.checkpointSetDigest,
        completedFileCount: event.completedFileCount,
        completedBytes: event.completedBytes,
        selectionFacts: event.selectionFacts,
        expiresAt: stableDeadline(context.nowMilliseconds),
        partialReceiptDigest: event.receiptDigest,
      })
    case 'partial-directory':
      return nextReceiveLifecycleState(state, {
        kind: 'partial-directory',
        reason: 'failures',
        successCount: event.successCount,
        failureCount: event.failureCount,
        receiptDigest: event.receiptDigest,
      })
    case 'discarded':
      if (event.successCount !== 0n) throw new TypeError('discarded tree retained successful output')
      return nextReceiveLifecycleState(state, {
        kind: 'discarded',
        cleanupReceiptDigest: event.receiptDigest,
      })
  }
}

function materializationCompleted(
  state: ReceiveLifecycleState,
  context: LifecycleReducerContext,
): ReceiveLifecycleState {
  requireState(state, 'receiving')
  if (context.planKind === 'direct-tree' || context.planKind === 'direct-atomic') {
    return discoveryCompleted(state, context)
  }
  return state
}

function restartRequired(
  state: ReceiveLifecycleState,
  event: Extract<LifecycleEvent, { kind: 'restart-boundary-verified' }>,
  planKind: PlanKind,
): ReceiveLifecycleState {
  const directAtomicState = planKind === 'direct-atomic' &&
    (state.kind === 'receiving' || state.kind === 'committing-atomic')
  const portableState = planKind === 'portable-handoff' &&
    (state.kind === 'receiving' ||
     (state.kind === 'handing-off' && state.attemptKind === 'portable'))
  const directZipDeleted = planKind === 'direct-resumable-zip' &&
    event.reason === 'target-deleted' &&
    (state.kind === 'receiving' || state.kind === 'resumable-receive' ||
     state.kind === 'authorization-required' ||
     state.kind === 'target-verification-required' ||
     state.kind === 'destination-space-required')
  if (!directAtomicState && !portableState && !directZipDeleted) {
    throw new TypeError('restart boundary is legal only for DirectAtomic or Portable')
  }
  if (event.reason === 'direct-atomic-rolled-back' && !directAtomicState) {
    throw new TypeError('atomic rollback reason requires a DirectAtomic boundary')
  }
  if (event.reason === 'portable-aborted' && !portableState) {
    throw new TypeError('portable abort reason requires a portable handoff')
  }
  if (event.reason === 'target-deleted' && !directZipDeleted) {
    throw new TypeError('target deletion reason requires a DirectResumableZip boundary')
  }
  return nextReceiveLifecycleState(state, {
    kind: 'restart-required',
    reason: event.reason,
    receiptDigest: event.receiptDigest,
  })
}

function sealMaterialization(
  state: ReceiveLifecycleState,
  event: Extract<LifecycleEvent, { kind: 'materialization-seal-verified' }>,
  context: LifecycleReducerContext,
): ReceiveLifecycleState {
  requireState(state, 'receiving')
  if (context.planKind !== 'workspace-then-publish') {
    throw new TypeError('only workspace materialization can be sealed')
  }
  return nextReceiveLifecycleState(state, {
    kind: 'materialization-sealed',
    sealedMaterializationDigest: event.sealedMaterializationDigest,
  })
}

function startPackage(
  state: ReceiveLifecycleState,
  event: Extract<LifecycleEvent, { kind: 'package-started' }>,
  context: LifecycleReducerContext,
): ReceiveLifecycleState {
  requireWorkspaceState(state, context.planKind, 'materialization-sealed')
  return nextReceiveLifecycleState(state, {
    kind: 'packaging',
    activeLeaseId: context.activeLeaseId,
    sealedMaterializationDigest: state.sealedMaterializationDigest,
    packageTempObjectId: event.packageTempObjectId,
  })
}

function pausePackage(
  state: ReceiveLifecycleState,
  event: Extract<LifecycleEvent, { kind: 'package-retryable-failure' }>,
  nowMilliseconds: number,
): ReceiveLifecycleState {
  requireState(state, 'packaging')
  return nextReceiveLifecycleState(state, {
    kind: 'resumable-package',
    sealedMaterializationDigest: state.sealedMaterializationDigest,
    tempCleanupProofDigest: event.tempCleanupProofDigest,
    expiresAt: stableDeadline(nowMilliseconds),
  })
}

function sealPackage(
  state: ReceiveLifecycleState,
  event: Extract<LifecycleEvent, { kind: 'package-seal-verified' }>,
): ReceiveLifecycleState {
  requireState(state, 'packaging')
  return nextReceiveLifecycleState(state, {
    kind: 'artifact-sealed',
    packageDigest: event.packageDigest,
  })
}

function waitToSave(
  state: ReceiveLifecycleState,
  nowMilliseconds: number,
): ReceiveLifecycleState {
  requireState(state, 'artifact-sealed')
  return nextReceiveLifecycleState(state, {
    kind: 'waiting-to-save',
    packageDigest: state.packageDigest,
    expiresAt: stableDeadline(nowMilliseconds),
  })
}

function startManagedPublication(
  state: ReceiveLifecycleState,
  event: Extract<LifecycleEvent, { kind: 'save-requested' }>,
  context: LifecycleReducerContext,
): ReceiveLifecycleState {
  if (context.planKind !== 'workspace-then-publish' ||
      (state.kind !== 'waiting-to-save' && state.kind !== 'download-started')) {
    throw new TypeError('save request requires a retained workspace package')
  }
  if (state.kind === 'download-started' && state.attemptKind !== 'workspace') {
    throw new TypeError('portable handoff cannot become managed publication')
  }
  const packageDigest = state.packageDigest
  if (packageDigest === undefined) throw new TypeError('managed publication lacks package digest')
  return nextReceiveLifecycleState(state, {
    kind: 'publishing-managed',
    activeLeaseId: context.activeLeaseId,
    packageDigest,
    publicationAttemptId: event.publicationAttemptId,
  })
}

function commitPublication(
  state: ReceiveLifecycleState,
  event: Extract<LifecycleEvent, { kind: 'publication-committed' }>,
): ReceiveLifecycleState {
  requireState(state, 'publishing-managed')
  return nextReceiveLifecycleState(state, {
    kind: 'published',
    receiptDigest: event.receiptDigest,
    cleanupState: 'cleanup-pending',
  })
}

function publicationNotCommitted(
  state: ReceiveLifecycleState,
  nowMilliseconds: number,
): ReceiveLifecycleState {
  requireState(state, 'publishing-managed')
  return nextReceiveLifecycleState(state, {
    kind: 'waiting-to-save',
    packageDigest: state.packageDigest,
    expiresAt: stableDeadline(nowMilliseconds),
  })
}

function startHandoff(
  state: ReceiveLifecycleState,
  event: Extract<LifecycleEvent, { kind: 'handoff-requested' }>,
  context: LifecycleReducerContext,
): ReceiveLifecycleState {
  if (event.attemptKind === 'workspace') {
    if (context.planKind !== 'workspace-then-publish' ||
        (state.kind !== 'waiting-to-save' && state.kind !== 'download-started') ||
        (state.kind === 'download-started' && state.attemptKind !== 'workspace')) {
      throw new TypeError('workspace handoff requires a retained workspace package')
    }
    const deadline = lifecycleDeadline(state)
    if (deadline === undefined) throw new TypeError('workspace handoff lost its waiting deadline')
    return nextReceiveLifecycleState(state, {
      kind: 'handing-off',
      activeLeaseId: context.activeLeaseId,
      attemptKind: 'workspace',
      attemptId: event.attemptId,
      packageDigest: state.packageDigest,
      retainedDeadline: deadline,
    })
  }
  if (context.planKind !== 'portable-handoff' || state.kind !== 'receiving') {
    throw new TypeError('portable handoff requires completed portable receive')
  }
  return nextReceiveLifecycleState(state, {
    kind: 'handing-off',
    activeLeaseId: context.activeLeaseId,
    attemptKind: 'portable',
    attemptId: event.attemptId,
  })
}

function handoffStarted(state: ReceiveLifecycleState): ReceiveLifecycleState {
  requireState(state, 'handing-off')
  if (state.attemptKind === 'workspace') {
    return nextReceiveLifecycleState(state, {
      kind: 'download-started',
      attemptKind: 'workspace',
      attemptId: state.attemptId,
      packageDigest: state.packageDigest,
      retryableUntil: state.retainedDeadline,
    })
  }
  return nextReceiveLifecycleState(state, {
    kind: 'download-started',
    attemptKind: 'portable',
    attemptId: state.attemptId,
  })
}

function handoffNotStarted(
  state: ReceiveLifecycleState,
  event: Extract<LifecycleEvent, { kind: 'handoff-not-started' }>,
  nowMilliseconds: number,
): LifecycleReduction {
  requireState(state, 'handing-off')
  if (state.attemptKind === 'portable') {
    return applied(nextReceiveLifecycleState(state, {
      kind: 'restart-required',
      reason: 'portable-aborted',
      receiptDigest: event.expiryReceiptDigest ?? state.attemptId,
    }))
  }
  if (state.retainedDeadline === undefined || state.packageDigest === undefined) {
    throw new TypeError('workspace handoff lost retained package state')
  }
  if (nowMilliseconds >= state.retainedDeadline) {
    if (event.expiryReceiptDigest === undefined) {
      throw new TypeError('elapsed handoff deadline requires an expiry receipt')
    }
    return applied(nextReceiveLifecycleState(state, {
      kind: 'expired',
      priorStableState: 'waiting-to-save',
      expiresAt: state.retainedDeadline,
      cleanupState: 'cleanup-pending',
      expiryReceiptDigest: event.expiryReceiptDigest,
    }))
  }
  return applied(nextReceiveLifecycleState(state, {
    kind: 'waiting-to-save',
    packageDigest: state.packageDigest,
    expiresAt: state.retainedDeadline,
  }))
}

function cleanupVerified(
  state: ReceiveLifecycleState,
  cleanupReceiptDigest: string,
): ReceiveLifecycleState {
  if (state.kind === 'published') {
    return nextReceiveLifecycleState(state, {
      kind: 'published',
      receiptDigest: state.receiptDigest,
      cleanupState: 'clean',
    })
  }
  if (state.kind === 'expired') {
    return nextReceiveLifecycleState(state, {
      kind: 'expired',
      priorStableState: state.priorStableState,
      expiresAt: state.expiresAt,
      cleanupState: 'clean',
      expiryReceiptDigest: state.expiryReceiptDigest,
    })
  }
  if (state.kind === 'partial-directory' || state.kind === 'restart-required') {
    throw new TypeError('cleanup cannot erase a retained terminal outcome')
  }
  return nextReceiveLifecycleState(state, { kind: 'discarded', cleanupReceiptDigest })
}

function expireState(
  state: ReceiveLifecycleState,
  event: Extract<LifecycleEvent, { kind: 'expiry-observed' }>,
  nowMilliseconds: number,
): LifecycleReduction {
  const deadline = lifecycleDeadline(state)
  if (deadline === undefined) throw new TypeError('expiry applies only to stable durable state')
  if (nowMilliseconds < deadline) return Object.freeze({ status: 'not-due', state })
  return applied(nextReceiveLifecycleState(state, {
    kind: 'expired',
    priorStableState: priorStableStateKind(state),
    expiresAt: deadline,
    cleanupState: event.cleanupState,
    expiryReceiptDigest: event.expiryReceiptDigest,
  }))
}

function priorStableStateKind(
  state: ReceiveLifecycleState,
): RetainedLifecycleKind {
  switch (state.kind) {
    case 'resumable-receive':
    case 'resumable-package':
    case 'waiting-to-save':
    case 'download-started':
    case 'authorization-required':
    case 'target-verification-required':
    case 'destination-space-required':
      return state.kind
    default:
      throw new TypeError('state has no stable expiry identity')
  }
}

export type {
  DirectoryFailureReason,
  ExternalAttemptReason,
  PackageFailureReason,
  PreparationAdmissionReason,
  ReceiveLifecycleState,
  ReceiveLifecycleStatePayload,
} from './state'
