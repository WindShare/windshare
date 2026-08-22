import {
  lifecycleDeadline,
  nextReceiveLifecycleState,
  stableDeadline,
  type NeedsAttentionReason,
  type PlanKind,
  type ReceiveLifecycleState,
} from './state'
import {
  observeRecovery,
  type ReceiveOperationTraceListener,
  type RecoveryDecision,
} from './trace'

export type AbandonedOperationObservation =
  | Readonly<{
      kind: 'verified-receive'
      checkpointSetDigest: string
      completedFileCount: bigint
      completedBytes: bigint
      partialReceiptDigest?: string
      lastVerifiedRecordDigest: string
    }>
  | Readonly<{
      kind: 'tree-finalized'
      outcome: 'published' | 'partial-directory' | 'resumable'
      receiptDigest: string
      successCount: bigint
      failureCount: bigint
      checkpointSetDigest?: string
      completedBytes: bigint
      lastVerifiedRecordDigest: string
    }>
  | Readonly<{
      kind: 'target-rolled-back'
      receiptDigest: string
      lastVerifiedRecordDigest: string
    }>
  | Readonly<{
      kind: 'atomic-commit'
      outcome: 'committed' | 'not-committed' | 'unknown'
      receiptDigest: string
      lastVerifiedRecordDigest: string
    }>
  | Readonly<{
      kind: 'restart-preparation'
      preparationId: string
      cleanupReceiptDigest: string
      lastVerifiedRecordDigest: string
    }>
  | Readonly<{
      kind: 'verified-package'
      sealedMaterializationDigest: string
      tempCleanupProofDigest: string
      lastVerifiedRecordDigest: string
    }>
  | Readonly<{
      kind: 'verified-artifact'
      packageDigest: string
      lastVerifiedRecordDigest: string
    }>
  | Readonly<{
      kind: 'publication'
      outcome: 'committed' | 'not-committed' | 'unknown'
      receiptDigest: string
      lastVerifiedRecordDigest: string
    }>
  | Readonly<{
      kind: 'handoff'
      outcome: 'started' | 'not-started' | 'unknown'
      lastVerifiedRecordDigest: string
      expiryReceiptDigest: string
    }>
  | Readonly<{
      kind: 'verified-download'
      lastVerifiedRecordDigest: string
      expiryReceiptDigest: string
    }>
  | Readonly<{
      kind: 'published-cleanup'
      outcome: 'retry' | 'completed' | 'unknown'
      cleanupReceiptDigest: string
      lastVerifiedRecordDigest: string
    }>
  | Readonly<{
      kind: 'unknown'
      authority: 'target' | 'publication' | 'cleanup'
      lastVerifiedRecordDigest: string
    }>

export interface RecoveryContext {
  readonly planKind: PlanKind
  readonly nowMilliseconds: number
  readonly expiryReceiptDigest: string
  readonly onTrace?: ReceiveOperationTraceListener
}

export interface RecoveryReduction {
  readonly state: ReceiveLifecycleState
  readonly decision: RecoveryDecision
}

export function recoverAbandonedOperation(
  state: ReceiveLifecycleState,
  observation: AbandonedOperationObservation,
  context: RecoveryContext,
): RecoveryReduction {
  requireRecoveryClock(context.nowMilliseconds)
  assertDurableRecoveryPlan(state, context.planKind)
  const deadline = lifecycleDeadline(state)
  let reduction: RecoveryReduction
  if (deadline !== undefined && context.nowMilliseconds >= deadline) {
    reduction = Object.freeze({
      state: expiredState(state, deadline, context.expiryReceiptDigest),
      decision: 'expired',
    })
  } else {
    reduction = recoverUnexpired(state, observation, context)
  }
  observeRecovery({
    ...(context.onTrace === undefined ? {} : { listener: context.onTrace }),
    atMilliseconds: context.nowMilliseconds,
    observed: state,
    reduced: reduction.state,
    decision: reduction.decision,
  })
  return reduction
}

function recoverUnexpired(
  state: ReceiveLifecycleState,
  observation: AbandonedOperationObservation,
  context: RecoveryContext,
): RecoveryReduction {
  if (observation.kind === 'unknown') {
    return attentionReduction(
      state,
      unknownReason(observation.authority),
      observation.lastVerifiedRecordDigest,
    )
  }
  switch (state.kind) {
    case 'receiving': return recoverReceiving(state, observation, context)
    case 'finalizing-tree': return recoverFinalizingTree(state, observation, context)
    case 'committing-atomic': return recoverAtomicCommit(state, observation)
    case 'preparing': return recoverPreparation(state, observation, context.nowMilliseconds)
    case 'materialization-sealed':
    case 'packaging': return recoverPackaging(state, observation, context.nowMilliseconds)
    case 'artifact-sealed': return recoverArtifact(state, observation, context.nowMilliseconds)
    case 'publishing-managed':
      return recoverPublication(state, observation, context.nowMilliseconds)
    case 'handing-off': return recoverHandoff(state, observation, context.nowMilliseconds)
    case 'download-started': return recoverDownload(state, observation, context.nowMilliseconds)
    case 'published': return recoverPublished(state, observation)
    default:
      throw new TypeError(`state ${state.kind} is not an abandoned active operation`)
  }
}

function recoverReceiving(
  state: Extract<ReceiveLifecycleState, { kind: 'receiving' }>,
  observation: AbandonedOperationObservation,
  context: RecoveryContext,
): RecoveryReduction {
  if (context.planKind === 'direct-atomic') {
    if (observation.kind !== 'target-rolled-back') {
      return attentionReduction(state, 'target-ownership-unknown', lastDigest(observation))
    }
    return reduction(nextReceiveLifecycleState(state, {
      kind: 'restart-required',
      reason: 'direct-atomic-rolled-back',
      receiptDigest: observation.receiptDigest,
    }), 'restart-required')
  }
  if (observation.kind !== 'verified-receive') {
    const reason = context.planKind === 'direct-tree'
      ? 'target-ownership-unknown'
      : 'cleanup-unknown'
    return attentionReduction(state, reason, lastDigest(observation))
  }
  return reduction(resumableReceive(state, observation, context.nowMilliseconds), 'resume-receive')
}

function recoverFinalizingTree(
  state: Extract<ReceiveLifecycleState, { kind: 'finalizing-tree' }>,
  observation: AbandonedOperationObservation,
  context: RecoveryContext,
): RecoveryReduction {
  if (observation.kind !== 'tree-finalized') {
    return attentionReduction(state, 'target-ownership-unknown', lastDigest(observation))
  }
  if (observation.outcome === 'published') {
    return reduction(nextReceiveLifecycleState(state, {
      kind: 'published',
      receiptDigest: observation.receiptDigest,
      cleanupState: 'cleanup-pending',
    }), 'published')
  }
  if (observation.outcome === 'partial-directory') {
    return reduction(nextReceiveLifecycleState(state, {
      kind: 'partial-directory',
      reason: 'failures',
      successCount: observation.successCount,
      failureCount: observation.failureCount,
      receiptDigest: observation.receiptDigest,
    }), 'published')
  }
  if (observation.checkpointSetDigest === undefined) {
    throw new TypeError('retryable tree recovery lacks checkpoint evidence')
  }
  return reduction(nextReceiveLifecycleState(state, {
    kind: 'resumable-receive',
    payloadKind: 'file-set',
    checkpointSetDigest: observation.checkpointSetDigest,
    completedFileCount: observation.successCount,
    completedBytes: observation.completedBytes,
    expiresAt: stableDeadline(context.nowMilliseconds),
    partialReceiptDigest: observation.receiptDigest,
  }), 'resume-receive')
}

function recoverAtomicCommit(
  state: Extract<ReceiveLifecycleState, { kind: 'committing-atomic' }>,
  observation: AbandonedOperationObservation,
): RecoveryReduction {
  if (observation.kind !== 'atomic-commit' || observation.outcome === 'unknown') {
    return attentionReduction(state, 'publication-unknown', lastDigest(observation))
  }
  if (observation.outcome === 'committed') {
    return reduction(nextReceiveLifecycleState(state, {
      kind: 'published',
      receiptDigest: observation.receiptDigest,
      cleanupState: 'cleanup-pending',
    }), 'published')
  }
  return reduction(nextReceiveLifecycleState(state, {
    kind: 'restart-required',
    reason: 'direct-atomic-rolled-back',
    receiptDigest: observation.receiptDigest,
  }), 'restart-required')
}

function recoverPreparation(
  state: Extract<ReceiveLifecycleState, { kind: 'preparing' }>,
  observation: AbandonedOperationObservation,
  nowMilliseconds: number,
): RecoveryReduction {
  if (observation.kind === 'verified-receive') {
    return reduction(resumableReceive(state, observation, nowMilliseconds), 'resume-receive')
  }
  if (observation.kind !== 'restart-preparation') {
    return attentionReduction(state, 'cleanup-unknown', lastDigest(observation))
  }
  return reduction(nextReceiveLifecycleState(state, {
    kind: 'preparing',
    preparationId: observation.preparationId,
  }), 'restart-preparation')
}

function recoverPackaging(
  state: Extract<ReceiveLifecycleState, { kind: 'materialization-sealed' | 'packaging' }>,
  observation: AbandonedOperationObservation,
  nowMilliseconds: number,
): RecoveryReduction {
  if (observation.kind !== 'verified-package' ||
      observation.sealedMaterializationDigest !== state.sealedMaterializationDigest) {
    return attentionReduction(state, 'cleanup-unknown', lastDigest(observation))
  }
  return reduction(nextReceiveLifecycleState(state, {
    kind: 'resumable-package',
    sealedMaterializationDigest: observation.sealedMaterializationDigest,
    tempCleanupProofDigest: observation.tempCleanupProofDigest,
    expiresAt: stableDeadline(nowMilliseconds),
  }), 'resume-package')
}

function recoverArtifact(
  state: Extract<ReceiveLifecycleState, { kind: 'artifact-sealed' }>,
  observation: AbandonedOperationObservation,
  nowMilliseconds: number,
): RecoveryReduction {
  if (observation.kind !== 'verified-artifact' ||
      observation.packageDigest !== state.packageDigest) {
    return attentionReduction(state, 'cleanup-unknown', lastDigest(observation))
  }
  return reduction(nextReceiveLifecycleState(state, {
    kind: 'waiting-to-save',
    packageDigest: observation.packageDigest,
    expiresAt: stableDeadline(nowMilliseconds),
  }), 'waiting-to-save')
}

function recoverPublication(
  state: Extract<ReceiveLifecycleState, { kind: 'publishing-managed' }>,
  observation: AbandonedOperationObservation,
  nowMilliseconds: number,
): RecoveryReduction {
  if (observation.kind !== 'publication' || observation.outcome === 'unknown') {
    return attentionReduction(state, 'publication-unknown', lastDigest(observation))
  }
  if (observation.outcome === 'committed') {
    return reduction(nextReceiveLifecycleState(state, {
      kind: 'published',
      receiptDigest: observation.receiptDigest,
      cleanupState: 'cleanup-pending',
    }), 'published')
  }
  return reduction(nextReceiveLifecycleState(state, {
    kind: 'waiting-to-save',
    packageDigest: state.packageDigest,
    expiresAt: stableDeadline(nowMilliseconds),
  }), 'waiting-to-save')
}

function recoverHandoff(
  state: Extract<ReceiveLifecycleState, { kind: 'handing-off' }>,
  observation: AbandonedOperationObservation,
  nowMilliseconds: number,
): RecoveryReduction {
  if (state.attemptKind !== 'workspace' || observation.kind !== 'handoff' ||
      observation.outcome === 'unknown' || state.packageDigest === undefined ||
      state.retainedDeadline === undefined) {
    return attentionReduction(state, 'publication-unknown', lastDigest(observation))
  }
  if (nowMilliseconds >= state.retainedDeadline) {
    return reduction(expiredState(
      state,
      state.retainedDeadline,
      observation.expiryReceiptDigest,
    ), 'expired')
  }
  const payload = observation.outcome === 'started'
    ? {
        kind: 'download-started' as const,
        attemptKind: 'workspace' as const,
        attemptId: state.attemptId,
        packageDigest: state.packageDigest,
        retryableUntil: state.retainedDeadline,
      }
    : {
        kind: 'waiting-to-save' as const,
        packageDigest: state.packageDigest,
        expiresAt: state.retainedDeadline,
      }
  return reduction(
    nextReceiveLifecycleState(state, payload),
    observation.outcome === 'started' ? 'download-started' : 'waiting-to-save',
  )
}

function recoverDownload(
  state: Extract<ReceiveLifecycleState, { kind: 'download-started' }>,
  observation: AbandonedOperationObservation,
  nowMilliseconds: number,
): RecoveryReduction {
  if (state.attemptKind !== 'workspace' || observation.kind !== 'verified-download' ||
      state.retryableUntil === undefined) {
    return attentionReduction(state, 'cleanup-unknown', lastDigest(observation))
  }
  if (nowMilliseconds >= state.retryableUntil) {
    return reduction(expiredState(
      state,
      state.retryableUntil,
      observation.expiryReceiptDigest,
    ), 'expired')
  }
  return reduction(state, 'download-started')
}

function recoverPublished(
  state: Extract<ReceiveLifecycleState, { kind: 'published' }>,
  observation: AbandonedOperationObservation,
): RecoveryReduction {
  if (state.cleanupState !== 'cleanup-pending' || observation.kind !== 'published-cleanup') {
    throw new TypeError('published recovery requires pending cleanup observation')
  }
  if (observation.outcome === 'unknown') {
    return attentionReduction(state, 'cleanup-unknown', observation.lastVerifiedRecordDigest)
  }
  if (observation.outcome === 'completed') {
    return reduction(nextReceiveLifecycleState(state, {
      kind: 'published',
      receiptDigest: state.receiptDigest,
      cleanupState: 'clean',
    }), 'published')
  }
  return reduction(state, 'published-cleanup-retry')
}

function resumableReceive(
  state: ReceiveLifecycleState,
  observation: Extract<AbandonedOperationObservation, { kind: 'verified-receive' }>,
  nowMilliseconds: number,
): ReceiveLifecycleState {
  return nextReceiveLifecycleState(state, {
    kind: 'resumable-receive',
    payloadKind: 'file-set',
    checkpointSetDigest: observation.checkpointSetDigest,
    completedFileCount: observation.completedFileCount,
    completedBytes: observation.completedBytes,
    expiresAt: stableDeadline(nowMilliseconds),
    ...(observation.partialReceiptDigest === undefined
      ? {}
      : { partialReceiptDigest: observation.partialReceiptDigest }),
  })
}

function expiredState(
  state: ReceiveLifecycleState,
  expiresAt: number,
  expiryReceiptDigest: string,
): ReceiveLifecycleState {
  const priorStableState = stableStateKind(state)
  return nextReceiveLifecycleState(state, {
    kind: 'expired',
    priorStableState,
    expiresAt,
    cleanupState: 'cleanup-pending',
    expiryReceiptDigest,
  })
}

function attentionReduction(
  state: ReceiveLifecycleState,
  reason: NeedsAttentionReason,
  lastVerifiedRecordDigest: string,
): RecoveryReduction {
  return reduction(nextReceiveLifecycleState(state, {
    kind: 'needs-attention',
    reason,
    lastVerifiedRecordDigest,
  }), 'needs-attention')
}

function reduction(
  state: ReceiveLifecycleState,
  decision: RecoveryDecision,
): RecoveryReduction {
  return Object.freeze({ state, decision })
}

function unknownReason(authority: 'target' | 'publication' | 'cleanup'): NeedsAttentionReason {
  switch (authority) {
    case 'target': return 'target-ownership-unknown'
    case 'publication': return 'publication-unknown'
    case 'cleanup': return 'cleanup-unknown'
  }
}

function lastDigest(observation: AbandonedOperationObservation): string {
  return observation.lastVerifiedRecordDigest
}

function requireRecoveryClock(nowMilliseconds: number): void {
  if (!Number.isSafeInteger(nowMilliseconds) || nowMilliseconds < 0) {
    throw new TypeError('recovery clock must be a non-negative safe integer')
  }
}

function assertDurableRecoveryPlan(
  state: ReceiveLifecycleState,
  planKind: PlanKind,
): void {
  if (planKind === 'portable-handoff') {
    throw new TypeError('Portable lifecycle is runtime-only and has no durable recovery state')
  }
  if (planKind === 'direct-tree' &&
      state.kind !== 'receiving' &&
      state.kind !== 'resumable-receive' &&
      state.kind !== 'finalizing-tree' &&
      state.kind !== 'published') {
    throw new TypeError('lifecycle state does not belong to DirectTree recovery')
  }
  if (planKind === 'direct-atomic' &&
      state.kind !== 'receiving' &&
      state.kind !== 'committing-atomic' &&
      state.kind !== 'published') {
    throw new TypeError('lifecycle state does not belong to DirectAtomic recovery')
  }
  if (planKind === 'workspace-then-publish' &&
      (state.kind === 'finalizing-tree' ||
       state.kind === 'committing-atomic' ||
       state.kind === 'partial-directory' ||
       state.kind === 'restart-required')) {
    throw new TypeError('lifecycle state does not belong to Workspace recovery')
  }
}

function stableStateKind(
  state: ReceiveLifecycleState,
): import('./state').RetainedLifecycleKind {
  if (state.kind === 'resumable-receive' || state.kind === 'resumable-package' ||
      state.kind === 'waiting-to-save' || state.kind === 'download-started' ||
      state.kind === 'authorization-required' ||
      state.kind === 'target-verification-required' ||
      state.kind === 'destination-space-required') return state.kind
  if (state.kind === 'handing-off') return 'waiting-to-save'
  throw new TypeError('recovery expiry requires a stable state')
}
