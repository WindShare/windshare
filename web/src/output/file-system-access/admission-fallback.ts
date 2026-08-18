import { snapshotIdentity } from '../workspace/canonical'
import type { ReceiveLifecycleState } from '../workspace/state'
import type { DirectTreeIntent } from './settlement-proof'

export type ReceiveAdmissionFallback = Extract<
  ReceiveLifecycleState,
  { kind: 'resumable-receive' }
>

export function snapshotReceiveAdmissionFallback(
  intent: DirectTreeIntent,
  input: ReceiveAdmissionFallback | undefined,
): ReceiveAdmissionFallback | undefined {
  if (input === undefined) return undefined
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
    operationId: input.operationId,
    receiveIntentDigest: input.receiveIntentDigest,
    generation: input.generation,
    checkpointSetDigest: snapshotIdentity(input.checkpointSetDigest, 32, 'checkpoint set digest'),
    completedFileCount: input.completedFileCount,
    completedBytes: input.completedBytes,
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
  return state.kind === 'resumable-receive' &&
    state.checkpointSetDigest === fallback.checkpointSetDigest &&
    state.completedFileCount === fallback.completedFileCount &&
    state.completedBytes === fallback.completedBytes &&
    state.expiresAt === fallback.expiresAt &&
    state.partialReceiptDigest === fallback.partialReceiptDigest
}

export function isFSAStableOrTerminal(state: ReceiveLifecycleState): boolean {
  return state.kind === 'resumable-receive' || state.kind === 'published' ||
    state.kind === 'partial-directory' || state.kind === 'restart-required' ||
    state.kind === 'discarded' || state.kind === 'expired' || state.kind === 'needs-attention'
}
