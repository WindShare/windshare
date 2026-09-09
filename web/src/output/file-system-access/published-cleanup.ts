import type { ReceiveIntent } from '../../transfer/intent'
import { IndexedDbCompatibleNameLedger } from '../browser/indexeddb-compatible-name-ledger'
import { emitOutputTrace, outputTraceEvent, type OutputTraceSource } from '../diagnostics'
import { createMaterializationLedgerBinding } from '../materialization-ledger/codec'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import {
  canonicalFrame, canonicalIdentity, canonicalRecord, canonicalU8,
} from '../workspace/canonical'
import { reduceReceiveLifecycle } from '../workspace/lifecycle'
import {
  createPersistedReceiveRecord, operationRecordId, RECEIVE_RECORD_CLEANUP,
  RECEIVE_RECORD_RECEIPT, validatePersistedReceiveRecord,
} from '../workspace/records'
import { storedReceiveLifecycleState } from '../workspace/state-codec'
import type { ReceiveLifecycleState } from '../workspace/state'
import {
  openFSAFileCheckpointRepository, type FSAFileCheckpointRepositoryOptions,
} from './checkpoint-repository'
import type { CompatibleNameLedger } from './compatible-name/ledger'
import type { CompatibleNameOperationHeaderV1 } from './compatible-name/model'
import { retireFSAMaterializationRecoveryMetadata } from './recovery-metadata-retirement'
import { requireDirectTreeIntent, type DirectTreeIntent } from './settlement-proof'
import type { FSASettlementRepository } from './settlement'

const FSA_PUBLISHED_METADATA_CLEANUP_RECEIPT_V1 = 17

type PublishedLifecycle = Extract<ReceiveLifecycleState, { kind: 'published' }>
type PublicationCleanupLedger = Pick<CompatibleNameLedger,
  'readHeader' | 'clearPendingTerminalOutcome' | 'close'>

export interface FSAPublishedCleanupAuthority {
  readonly intent: ReceiveIntent
  readonly lifecycle: PublishedLifecycle
  readonly repository: FSASettlementRepository
  readonly leaseId: string
  readonly trace?: OutputTraceSource
}

export interface FSAPublishedCleanupResult {
  readonly lifecycle: PublishedLifecycle
  readonly receiptDigest: string
}

/**
 * Publication already owns the user files. Its remaining obligation is retiring
 * browser recovery metadata, so this authority accepts no file deletion port.
 */
export async function completeFileSystemAccessPublishedCleanup(
  input: FSAPublishedCleanupAuthority & { readonly cleanup: () => Promise<void> },
): Promise<FSAPublishedCleanupResult> {
  const intent = await requireDirectTreeIntent(input.intent)
  await verifyPublishedAuthority(input, intent)
  emitCleanup(input, 'started')
  try {
    await input.cleanup()
    const result = await commitPublishedCleanup(input, intent)
    emitCleanup(input, 'completed', result.lifecycle)
    return result
  } catch (error) {
    // A durable publication remains successful when its metadata cleanup is retryable.
    emitCleanup(input, 'retryable_failure')
    throw error
  }
}

export async function cleanupReopenedPublishedFileSystemAccessOutput(
  input: FSAPublishedCleanupAuthority & FSAFileCheckpointRepositoryOptions & {
    readonly openCompatibleNameLedger?: () => Promise<PublicationCleanupLedger>
  },
): Promise<FSAPublishedCleanupResult> {
  const intent = await requireDirectTreeIntent(input.intent)
  const reservation = intent.plan.reservation
  if (reservation.kind !== 'named-container-entry' || reservation.authorityKind !== 'fsa-container') {
    throw new TypeError('Published FSA cleanup requires a bound reserved entry')
  }
  return completeFileSystemAccessPublishedCleanup({
    ...input,
    cleanup: async () => {
      const ledger = await (input.openCompatibleNameLedger ??
        (() => IndexedDbCompatibleNameLedger.open(input.databaseName)))()
      try {
        const header = await ledger.readHeader(intent.operationId)
        verifyPublishedRepair(header, intent, input.lifecycle)
        const checkpoints = await openFSAFileCheckpointRepository(input, intent, reservation)
        try {
          const binding = await createMaterializationLedgerBinding({
            operationId: intent.operationId,
            receiveIntentDigest: intent.digest,
            materializationBindingDigest: intent.plan.reservation.digest,
            authorityRef: intent.plan.reservation.authorityRef,
          })
          await retireFSAMaterializationRecoveryMetadata(checkpoints, binding)
        } finally {
          checkpoints.close()
        }
        if (header?.pendingTerminalOutcome !== undefined && header.repairSummary !== undefined) {
          await ledger.clearPendingTerminalOutcome({
            operationId: intent.operationId,
            repairSummary: { ...header.repairSummary, terminalSettlement: 'complete' },
          })
          if ((await ledger.readHeader(intent.operationId))?.pendingTerminalOutcome !== undefined) {
            throw new DOMException('Published FSA repair metadata was not retired', 'OperationError')
          }
        }
      } finally {
        ledger.close()
      }
    },
  })
}

async function verifyPublishedAuthority(
  input: FSAPublishedCleanupAuthority,
  intent: DirectTreeIntent,
): Promise<void> {
  const { lifecycle, repository, leaseId } = input
  if (lifecycle.kind !== 'published' || lifecycle.cleanupState !== 'cleanup-pending' ||
      lifecycle.operationId !== intent.operationId || lifecycle.receiveIntentDigest !== intent.digest) {
    throw new DOMException('FSA publication has no pending metadata cleanup', 'InvalidStateError')
  }
  const expected = await storedReceiveLifecycleState(lifecycle)
  const [current, lease, receipt] = await Promise.all([
    repository.readLifecycle(intent.operationId),
    repository.readLease(intent.operationId),
    repository.readRecord(operationRecordId(intent.operationId, RECEIVE_RECORD_RECEIPT, lifecycle.receiptDigest)),
  ])
  if (current?.digest !== expected.digest || lease?.operationId !== intent.operationId ||
      lease.leaseId !== leaseId) {
    throw new DOMException('FSA publication cleanup authority is stale or foreign', 'InvalidStateError')
  }
  await validatePersistedReceiveRecord(current)
  if (receipt === undefined || receipt.kind !== RECEIVE_RECORD_RECEIPT ||
      receipt.operationId !== intent.operationId || receipt.digest !== lifecycle.receiptDigest) {
    throw new TargetOwnershipUnknownError('settlement', intent.operationId)
  }
  await validatePersistedReceiveRecord(receipt)
}

function verifyPublishedRepair(
  header: CompatibleNameOperationHeaderV1 | undefined,
  intent: DirectTreeIntent,
  lifecycle: PublishedLifecycle,
): void {
  if (header === undefined) return
  // Published was committed after the terminal footer was observed. Reopening user
  // files here would make metadata retirement depend on later edits or moves.
  const pending = header.pendingTerminalOutcome
  const summary = header.repairSummary
  const footer = summary?.latestObservedFooter
  if (header.operationId !== intent.operationId || header.authorityRef !== intent.plan.reservation.authorityRef ||
      summary?.sidecarSync !== 'current' ||
      (pending === undefined && summary.terminalSettlement !== 'complete') ||
      footer?.state !== 'completed' || footer.committedCount !== summary.committedCount ||
      (pending !== undefined && (
        pending.ordinaryLifecycle.kind !== 'published' ||
        pending.ordinaryLifecycle.receiveIntentDigest !== intent.digest ||
        pending.ordinaryLifecycle.receiptDigest !== lifecycle.receiptDigest ||
        pending.footerState !== 'completed'
      ))) {
    throw new DOMException('Published FSA cleanup requires settled restoration metadata', 'InvalidStateError')
  }
}

async function commitPublishedCleanup(
  input: FSAPublishedCleanupAuthority,
  intent: DirectTreeIntent,
): Promise<FSAPublishedCleanupResult> {
  // The lease/generation fence is rechecked after retirement and again by the atomic commit.
  await verifyPublishedAuthority(input, intent)
  const prior = await storedReceiveLifecycleState(input.lifecycle)
  const receipt = await createPersistedReceiveRecord({
    operationId: intent.operationId,
    kind: RECEIVE_RECORD_CLEANUP,
    canonicalBytes: canonicalRecord('windshare/receive-receipt/v1', 1, [
      canonicalU8(FSA_PUBLISHED_METADATA_CLEANUP_RECEIPT_V1),
      canonicalFrame(canonicalIdentity(intent.operationId, 16, 'operation ID')),
      canonicalFrame(canonicalIdentity(intent.digest, 32, 'receive intent digest')),
      canonicalFrame(canonicalIdentity(intent.plan.reservation.digest, 32, 'reservation digest')),
      canonicalFrame(canonicalIdentity(prior.digest, 32, 'published lifecycle digest')),
      canonicalFrame(canonicalIdentity(input.lifecycle.receiptDigest, 32, 'publication receipt digest')),
    ]),
  })
  const reduced = reduceReceiveLifecycle(input.lifecycle, {
    kind: 'cleanup-verified',
    cleanupReceiptDigest: receipt.digest,
    expectedGeneration: input.lifecycle.generation,
    leaseId: input.leaseId,
  }, {
    planKind: 'direct-tree',
    preparationRequired: false,
    activeLeaseId: input.leaseId,
  })
  if (reduced.status !== 'applied' || reduced.state.kind !== 'published' ||
      reduced.state.cleanupState !== 'clean') {
    throw new TypeError('FSA metadata cleanup did not preserve its publication')
  }
  const expected = await storedReceiveLifecycleState(reduced.state)
  const committed = async (): Promise<boolean> => {
    const [lifecycle, cleanup, lease] = await Promise.all([
      input.repository.readLifecycle(intent.operationId),
      input.repository.readRecord(receipt.id),
      input.repository.readLease(intent.operationId),
    ])
    if (lifecycle === undefined || cleanup === undefined || lease?.leaseId !== input.leaseId) return false
    return (await validatePersistedReceiveRecord(lifecycle)).digest === expected.digest &&
      (await validatePersistedReceiveRecord(cleanup)).digest === receipt.digest
  }
  try {
    await input.repository.commitTransition({
      operationId: intent.operationId,
      expectedLifecycleGeneration: input.lifecycle.generation,
      expectedLeaseId: input.leaseId,
      records: [receipt],
      lifecycle: reduced.state,
    })
  } catch (cause) {
    // IndexedDB completion can be lost after the transaction committed.
    if (!await committed().catch(() => false)) throw cause
  }
  if (!await committed()) throw new TargetOwnershipUnknownError('settlement', intent.operationId)
  return Object.freeze({ lifecycle: reduced.state, receiptDigest: receipt.digest })
}

function emitCleanup(
  input: FSAPublishedCleanupAuthority,
  transition: 'started' | 'completed' | 'retryable_failure',
  lifecycle = input.lifecycle,
): void {
  emitOutputTrace(input.trace, () => outputTraceEvent('cleanup', {
    backend: 'file_system_access',
    transition,
    operation_id: input.intent.operationId,
    receive_intent_digest: input.intent.digest,
    lifecycle_generation: lifecycle.generation.toString(),
    cleanup_kind: 'published_metadata',
  }))
}
