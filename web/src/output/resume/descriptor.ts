import {
  lifecycleDeadline,
  type ReceiveLifecycleState,
} from '../workspace/state'

export const RECEIVE_OPERATION_RESUME_DESCRIPTOR_VERSION = 2 as const

export type ReceiveOperationContinuation =
  | 'resume-receive'
  | 'resume-direct-zip'
  | 'reauthorize-direct-zip'
  | 'verify-direct-zip-target'
  | 'retry-direct-zip-space'
  | 'pending-catch-up'
  | 'restoration-available'
  | 'resume-package'
  | 'save-artifact'
  | 'retry-download'
  | 'cleanup-expired'
  | 'retry-cleanup'
  | 'needs-attention'

export interface ReceiveOperationResumeDescriptor {
  readonly schemaVersion: typeof RECEIVE_OPERATION_RESUME_DESCRIPTOR_VERSION
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly lifecycleGeneration: bigint
  readonly lifecycle: ReceiveLifecycleState
  readonly continuation: ReceiveOperationContinuation
  readonly expiresAt?: number
}

/**
 * Inventory projects strict Lifecycle V2 truth and never copies checkpoint ranges
 * into an aggregate byte counter.
 */
export function receiveOperationResumeDescriptor(
  lifecycle: ReceiveLifecycleState,
  nowMilliseconds: number,
): ReceiveOperationResumeDescriptor | undefined {
  requireClock(nowMilliseconds)
  const continuation = continuationFor(lifecycle, nowMilliseconds)
  if (continuation === undefined) return undefined
  const expiresAt = lifecycleDeadline(lifecycle)
  return Object.freeze({
    schemaVersion: RECEIVE_OPERATION_RESUME_DESCRIPTOR_VERSION,
    operationId: lifecycle.operationId,
    receiveIntentDigest: lifecycle.receiveIntentDigest,
    lifecycleGeneration: lifecycle.generation,
    lifecycle,
    continuation,
    ...(expiresAt === undefined ? {} : { expiresAt }),
  })
}

export function assertReceiveOperationCanContinue(
  descriptor: ReceiveOperationResumeDescriptor,
  nowMilliseconds: number,
): void {
  requireClock(nowMilliseconds)
  if (descriptor.continuation === 'needs-attention' ||
      descriptor.continuation === 'cleanup-expired' ||
      descriptor.continuation === 'retry-cleanup') {
    throw new DOMException('Receive operation cannot continue automatically', 'InvalidStateError')
  }
  if (descriptor.expiresAt !== undefined && nowMilliseconds >= descriptor.expiresAt) {
    throw new DOMException('Receive operation retention deadline has elapsed', 'InvalidStateError')
  }
}

function continuationFor(
  lifecycle: ReceiveLifecycleState,
  nowMilliseconds: number,
): ReceiveOperationContinuation | undefined {
  const deadline = lifecycleDeadline(lifecycle)
  if (deadline !== undefined && nowMilliseconds >= deadline) return 'cleanup-expired'
  switch (lifecycle.kind) {
    case 'receiving': return 'pending-catch-up'
    case 'resumable-receive': return lifecycle.payloadKind === 'direct-zip'
      ? 'resume-direct-zip'
      : 'resume-receive'
    case 'authorization-required': return 'reauthorize-direct-zip'
    case 'target-verification-required': return 'verify-direct-zip-target'
    case 'destination-space-required': return 'retry-direct-zip-space'
    case 'resumable-package': return 'resume-package'
    case 'waiting-to-save': return 'save-artifact'
    case 'download-started':
      return lifecycle.attemptKind === 'workspace' && lifecycle.retryableUntil !== undefined
        ? 'retry-download'
        : undefined
    case 'expired':
      return lifecycle.cleanupState === 'cleanup-pending' ? 'cleanup-expired' : undefined
    case 'published':
      return lifecycle.cleanupState === 'cleanup-pending'
        ? 'retry-cleanup'
        : 'restoration-available'
    case 'partial-directory': return 'restoration-available'
    case 'needs-attention': return 'needs-attention'
    default: return undefined
  }
}

function requireClock(nowMilliseconds: number): void {
  if (!Number.isSafeInteger(nowMilliseconds) || nowMilliseconds < 0) {
    throw new TypeError('resume inventory clock must be a non-negative safe integer')
  }
}
