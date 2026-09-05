import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import {
  reduceReceiveLifecycle,
  type LifecycleReducerContext,
} from '../workspace/lifecycle'
import {
  validatePersistedReceiveRecord,
  type PersistedReceiveRecord,
} from '../workspace/records'
import { storedReceiveLifecycleState } from '../workspace/state-codec'
import type { ReceiveLifecycleState } from '../workspace/state'
import type { ReopenedDirectTreeOperation } from '../resume/reopen-authority'
import {
  openFileSystemAccessCompatibleNameCatchUp,
  type FileSystemAccessCompatibleNameCatchUpSession,
} from './session'
import type { CompatibleNamePendingTerminalOutcomeV1 } from './compatible-name/model'

export interface FileSystemAccessCompatibleNameCatchUpResult {
  readonly lifecycle: ReceiveLifecycleState
  readonly repairSummary: import('./compatible-name/model').CompatibleNameRepairSummary
}

export async function catchUpFileSystemAccessCompatibleNames(input: Readonly<{
  operation: ReopenedDirectTreeOperation
  signal: AbortSignal
  openSession?: (
    operation: ReopenedDirectTreeOperation,
  ) => Promise<FileSystemAccessCompatibleNameCatchUpSession>
  clock?: () => number
}>): Promise<FileSystemAccessCompatibleNameCatchUpResult> {
  input.signal.throwIfAborted()
  const session = await (input.openSession ?? openCompatibleNameCatchUpSession)(input.operation)
  // Acquiring local mutation authority is the retry commit point. From here cancellation
  // cannot strand a footer/lifecycle cut halfway through a second time.
  let attempt:
    | Readonly<{ succeeded: true; result: FileSystemAccessCompatibleNameCatchUpResult }>
    | Readonly<{ succeeded: false; error: unknown }>
  try {
    attempt = await session.runExclusive(async () => {
      const pending = session.pendingOutcome
      if (pending === undefined) {
        if (input.operation.lifecycle.kind !== 'receiving' &&
            input.operation.lifecycle.kind !== 'resumable-receive') {
          throw new TypeError('active compatible-name catch-up requires a receive continuation')
        }
        const repairSummary = await session.synchronizeActiveProjector()
        return Object.freeze({
          succeeded: true as const,
          result: Object.freeze({ lifecycle: input.operation.lifecycle, repairSummary }),
        })
      }
      if (pending.ordinaryLifecycle.operationId !== input.operation.intent.operationId ||
          pending.ordinaryLifecycle.receiveIntentDigest !== input.operation.intent.digest) {
        throw new TypeError('pending compatible-name outcome escaped its receive operation')
      }
      const receipt = await validatePersistedReceiveRecord(pending.terminalReceipt)
      const repairSummary = await session.drainTerminalProjector()
      const lifecycle = await reconcilePendingTerminalLifecycle(
        input.operation,
        pending,
        receipt,
        input.clock ?? Date.now,
      )
      await session.retireRecoveryMetadata()
      await session.clearPendingOutcome()
      return Object.freeze({
        succeeded: true as const,
        result: Object.freeze({
          lifecycle, repairSummary: Object.freeze({ ...repairSummary, terminalSettlement: 'complete' as const }),
        }),
      })
    })
  } catch (error) {
    attempt = Object.freeze({ succeeded: false, error })
  }
  try {
    await session.close()
  } catch (closeFailure) {
    if (attempt.succeeded) throw closeFailure
  }
  if (!attempt.succeeded) throw attempt.error
  return attempt.result
}

function openCompatibleNameCatchUpSession(
  operation: ReopenedDirectTreeOperation,
): Promise<FileSystemAccessCompatibleNameCatchUpSession> {
  return openFileSystemAccessCompatibleNameCatchUp({
    intent: operation.intent,
    operationRepository: operation.repository,
  })
}

async function reconcilePendingTerminalLifecycle(
  operation: ReopenedDirectTreeOperation,
  pending: CompatibleNamePendingTerminalOutcomeV1,
  receipt: PersistedReceiveRecord,
  clock: () => number,
): Promise<Extract<ReceiveLifecycleState, { kind: 'published' | 'partial-directory' }>> {
  const desired = requirePendingTerminalLifecycle(pending)
  if (receipt.operationId !== operation.intent.operationId ||
      receipt.digest !== desired.receiptDigest) {
    throw new TypeError('pending FSA receipt disagrees with its bound terminal lifecycle')
  }
  let current = operation.lifecycle
  if (terminalLifecycleSemanticsMatch(current, desired)) return current as typeof desired
  if (current.kind !== 'receiving') {
    throw new TypeError('FSA catch-up lifecycle is neither pending nor already terminal')
  }
  const context: LifecycleReducerContext = {
    planKind: 'direct-tree' as const,
    preparationRequired: false,
    activeLeaseId: operation.lease.leaseId,
    nowMilliseconds: clock(),
  }
  current = await rebindCatchUpLifecycle(operation, current, context)
  if (pending.footerState === 'stopped') {
    return reconcileStoppedCatchUp(operation, current, desired, receipt, context)
  }
  return reconcileCompletedCatchUp(operation, current, desired, receipt, context)
}

function requirePendingTerminalLifecycle(
  pending: CompatibleNamePendingTerminalOutcomeV1,
): Extract<ReceiveLifecycleState, { kind: 'published' | 'partial-directory' }> {
  const desired = pending.ordinaryLifecycle
  if (desired.kind !== 'published' && desired.kind !== 'partial-directory') {
    throw new TypeError('FSA catch-up requires an ordinary DirectTree terminal lifecycle')
  }
  return desired
}

async function rebindCatchUpLifecycle(
  operation: ReopenedDirectTreeOperation,
  current: Extract<ReceiveLifecycleState, { kind: 'receiving' }>,
  context: LifecycleReducerContext,
): Promise<Extract<ReceiveLifecycleState, { kind: 'receiving' }>> {
  const reacquired = reduceReceiveLifecycle(current, {
    kind: 'receive-authority-reacquired',
    expectedGeneration: current.generation,
    leaseId: operation.lease.leaseId,
  }, context)
  if (reacquired.status !== 'applied' || reacquired.state.kind !== 'receiving') {
    throw new TypeError('FSA catch-up could not rebind its local lifecycle authority')
  }
  return commitCatchUpLifecycle(operation, current, reacquired.state)
}

async function reconcileStoppedCatchUp(
  operation: ReopenedDirectTreeOperation,
  current: Extract<ReceiveLifecycleState, { kind: 'receiving' }>,
  desired: Extract<ReceiveLifecycleState, { kind: 'published' | 'partial-directory' }>,
  receipt: PersistedReceiveRecord,
  context: LifecycleReducerContext,
): Promise<Extract<ReceiveLifecycleState, { kind: 'partial-directory' }>> {
  if (desired.kind !== 'partial-directory' || desired.reason !== 'stopped') {
    throw new TypeError('stopped footer disagrees with pending lifecycle')
  }
  const reduced = reduceReceiveLifecycle(current, {
    kind: 'stop-requested',
    successCount: desired.successCount,
    failureCount: desired.failureCount,
    receiptDigest: desired.receiptDigest,
    cleanupReceiptDigest: desired.receiptDigest,
    expectedGeneration: current.generation,
    leaseId: operation.lease.leaseId,
  }, context)
  if (reduced.status !== 'applied' || reduced.state.kind !== 'partial-directory' ||
      !terminalLifecycleSemanticsMatch(reduced.state, desired)) {
    throw new TypeError('stopped catch-up does not reproduce its pending lifecycle')
  }
  return commitCatchUpLifecycle(operation, current, reduced.state, [receipt])
}

async function reconcileCompletedCatchUp(
  operation: ReopenedDirectTreeOperation,
  current: Extract<ReceiveLifecycleState, { kind: 'receiving' }>,
  desired: Extract<ReceiveLifecycleState, { kind: 'published' | 'partial-directory' }>,
  receipt: PersistedReceiveRecord,
  context: LifecycleReducerContext,
): Promise<Extract<ReceiveLifecycleState, { kind: 'published' | 'partial-directory' }>> {
  const finalizing = reduceReceiveLifecycle(current, {
    kind: 'discovery-completed',
    expectedGeneration: current.generation,
    leaseId: operation.lease.leaseId,
  }, context)
  if (finalizing.status !== 'applied' || finalizing.state.kind !== 'finalizing-tree') {
    throw new TypeError('completed catch-up could not enter finalization')
  }
  const committedFinalizing = await commitCatchUpLifecycle(operation, current, finalizing.state)
  const reduced = reduceReceiveLifecycle(committedFinalizing, {
    kind: 'tree-finalization-completed',
    outcome: desired.kind === 'published' ? 'published' : 'partial-directory',
    receiptDigest: desired.receiptDigest,
    completedFileCount: 0n,
    completedBytes: 0n,
    successCount: desired.kind === 'partial-directory' ? desired.successCount : 0n,
    failureCount: desired.kind === 'partial-directory' ? desired.failureCount : 0n,
    expectedGeneration: committedFinalizing.generation,
    leaseId: operation.lease.leaseId,
  }, context)
  if (reduced.status !== 'applied' ||
      (reduced.state.kind !== 'published' && reduced.state.kind !== 'partial-directory') ||
      !terminalLifecycleSemanticsMatch(reduced.state, desired)) {
    throw new TypeError('completed catch-up does not reproduce its pending lifecycle')
  }
  return commitCatchUpLifecycle(operation, committedFinalizing, reduced.state, [receipt])
}

async function commitCatchUpLifecycle<Lifecycle extends ReceiveLifecycleState>(
  operation: ReopenedDirectTreeOperation,
  current: ReceiveLifecycleState,
  next: Lifecycle,
  records: readonly PersistedReceiveRecord[] = [],
): Promise<Lifecycle> {
  await operation.repository.commitTransition({
    operationId: operation.intent.operationId,
    expectedLifecycleGeneration: current.generation,
    expectedLeaseId: operation.lease.leaseId,
    ...(records.length === 0 ? {} : { records }),
    lifecycle: next,
  })
  const observed = await operation.repository.readLifecycle(operation.intent.operationId)
  if (observed === undefined || observed.digest !== (await storedReceiveLifecycleState(next)).digest) {
    throw new TargetOwnershipUnknownError('settlement', operation.intent.operationId)
  }
  return next
}

function terminalLifecycleSemanticsMatch(
  observed: ReceiveLifecycleState,
  desired: Extract<ReceiveLifecycleState, { kind: 'published' | 'partial-directory' }>,
): boolean {
  if (observed.operationId !== desired.operationId ||
      observed.receiveIntentDigest !== desired.receiveIntentDigest || observed.kind !== desired.kind) {
    return false
  }
  if (observed.kind === 'published' && desired.kind === 'published') {
    return observed.receiptDigest === desired.receiptDigest &&
      observed.cleanupState === desired.cleanupState
  }
  return observed.kind === 'partial-directory' && desired.kind === 'partial-directory' &&
    observed.reason === desired.reason && observed.successCount === desired.successCount &&
    observed.failureCount === desired.failureCount && observed.receiptDigest === desired.receiptDigest
}
