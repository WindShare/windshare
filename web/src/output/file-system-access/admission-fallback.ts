import { snapshotIdentity } from '../workspace/canonical'
import type { LifecycleEvent } from '../workspace/lifecycle'
import {
  snapshotRecoverySelectionFacts,
  type ReceiveLifecycleState,
} from '../workspace/state'
import type { DirectTreeIntent } from './settlement-proof'

export type ReceiveAdmissionFallback = Extract<
  ReceiveLifecycleState,
  { kind: 'receiving' } | { kind: 'resumable-receive'; payloadKind: 'file-set' }
>

export function snapshotReceiveAdmissionFallback(
  intent: DirectTreeIntent,
  input: ReceiveAdmissionFallback | undefined,
): ReceiveAdmissionFallback | undefined {
  if (input === undefined) return undefined
  if (input.kind === 'receiving') {
    if (input.operationId !== intent.operationId || input.receiveIntentDigest !== intent.digest ||
        typeof input.generation !== 'bigint' || input.generation < 0n) {
      throw new TypeError('FSA interrupted admission fallback is foreign')
    }
    return Object.freeze({
      kind: 'receiving',
      operationId: input.operationId,
      receiveIntentDigest: input.receiveIntentDigest,
      generation: input.generation,
      activeLeaseId: snapshotIdentity(input.activeLeaseId, 16, 'interrupted receive lease'),
    })
  }
  if (typeof input !== 'object' || input === null || input.kind !== 'resumable-receive' ||
      input.operationId !== intent.operationId || input.receiveIntentDigest !== intent.digest ||
      typeof input.generation !== 'bigint' || input.generation < 0n ||
      typeof input.completedFileCount !== 'bigint' || input.completedFileCount < 0n ||
      typeof input.completedBytes !== 'bigint' || input.completedBytes < 0n ||
      !Number.isSafeInteger(input.expiresAt) || input.expiresAt < 0) {
    throw new TypeError('FSA admission fallback does not belong to the receive continuation')
  }
  return Object.freeze({
    kind: 'resumable-receive',
    payloadKind: 'file-set',
    operationId: input.operationId,
    receiveIntentDigest: input.receiveIntentDigest,
    generation: input.generation,
    checkpointSetDigest: snapshotIdentity(input.checkpointSetDigest, 32, 'checkpoint set digest'),
    completedFileCount: input.completedFileCount,
    completedBytes: input.completedBytes,
    selectionFacts: snapshotRecoverySelectionFacts(
      input.selectionFacts,
      input.completedFileCount,
      input.completedBytes,
    ),
    expiresAt: input.expiresAt,
    ...(input.partialReceiptDigest === undefined
      ? {}
      : { partialReceiptDigest: snapshotIdentity(input.partialReceiptDigest, 32, 'partial receipt digest') }),
  })
}

export function sameReceiveAdmissionFallback(
  state: ReceiveLifecycleState,
  fallback: ReceiveAdmissionFallback,
): boolean {
  return fallback.kind === 'resumable-receive' && state.kind === 'resumable-receive' && state.payloadKind === 'file-set' &&
    state.checkpointSetDigest === fallback.checkpointSetDigest &&
    state.completedFileCount === fallback.completedFileCount &&
    state.completedBytes === fallback.completedBytes &&
    state.selectionFacts.discoveredFileCount === fallback.selectionFacts.discoveredFileCount &&
    state.selectionFacts.discoveredBytes === fallback.selectionFacts.discoveredBytes &&
    state.selectionFacts.discovery === fallback.selectionFacts.discovery &&
    state.expiresAt === fallback.expiresAt &&
    state.partialReceiptDigest === fallback.partialReceiptDigest
}

/** Admission rollback restores the exact paused evidence, including its original retention deadline. */
export function receiveAdmissionFailureEvent(
  state: Extract<ReceiveLifecycleState, { kind: 'receiving' }>,
  fallback: Extract<ReceiveAdmissionFallback, { kind: 'resumable-receive' }>,
  leaseId: string,
): Extract<LifecycleEvent, { kind: 'resume-admission-failed' }> {
  return Object.freeze({
    kind: 'resume-admission-failed',
    checkpointSetDigest: fallback.checkpointSetDigest,
    completedFileCount: fallback.completedFileCount,
    completedBytes: fallback.completedBytes,
    selectionFacts: fallback.selectionFacts,
    expiresAt: fallback.expiresAt,
    ...(fallback.partialReceiptDigest === undefined
      ? {}
      : { partialReceiptDigest: fallback.partialReceiptDigest }),
    expectedGeneration: state.generation,
    leaseId,
  })
}

export function isFSAStableOrTerminal(state: ReceiveLifecycleState): boolean {
  return (state.kind === 'resumable-receive' && state.payloadKind === 'file-set') ||
    state.kind === 'published' ||
    state.kind === 'partial-directory' || state.kind === 'restart-required' ||
    state.kind === 'discarded' || state.kind === 'expired' || state.kind === 'needs-attention'
}
