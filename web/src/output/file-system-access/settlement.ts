import type { OutputDiagnosticsPorts } from '../diagnostics'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import { snapshotIdentity } from '../workspace/canonical'
import type { ReceiveOperationRepository } from '../workspace/repository'
import type { ReceiveLifecycleState } from '../workspace/state'
import { createDirectoryAdmissionScope, type DirectoryAdmissionScope } from '../../transfer/directory-admission'
import { snapshotTransferJobId } from '../../transfer/job/identity'
import type { ReceiveIntent } from '../../transfer/intent'
import type {
  PersistentDirectTreeSettlementAuthority,
} from '../../transfer/settlement/persistent-execution'
import {
  FileSystemAccessOutputSession,
} from './session'
import {
  snapshotReceiveAdmissionFallback,
  type ReceiveAdmissionFallback,
} from './admission-fallback'
import {
  requireDirectTreeIntent,
  type DirectTreeIntent,
} from './settlement-proof'
import { FSAOperationSettlementAuthority } from './settlement-authority'

export { fsaCheckpointSetDigest } from './settlement-proof'

export type FSASettlementRepository = Pick<
  ReceiveOperationRepository,
  'commitTransition' | 'readLifecycle' | 'readLease' | 'readRecord' | 'readHandle'
>

export type FSASettlementTraceEvent =
  | Readonly<{
      name: 'receive.fsa.settlement.completed'
      operation_id: string
      receive_intent_digest: string
      transfer_job_id: string
      outcome: 'published' | 'partial-directory' | 'resumable-receive' | 'discarded' | 'needs-attention'
      checkpoint_count: bigint
      completed_file_count: bigint
      completed_bytes: bigint
      ownership_stage?: TargetOwnershipUnknownError['stage']
    }>
  | Readonly<{
      name: 'receive.fsa.continuation.admission_failed'
      operation_id: string
      receive_intent_digest: string
      transfer_job_id: string
      restored_checkpoint_set_digest: string
      restored_completed_file_count: bigint
      restored_completed_bytes: bigint
      restored_expires_at_ms: number
    }>

export interface FileSystemAccessOperationSettlementAuthority {
  bindMaterialization(session: FileSystemAccessOutputSession): PersistentDirectTreeSettlementAuthority
  settleExecutionAdmissionFailure(
    intent: ReceiveIntent,
    reason: unknown,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState>
  recordSettlementUnknown(
    intent: ReceiveIntent,
    signal: AbortSignal,
  ): Promise<Extract<ReceiveLifecycleState, { readonly kind: 'needs-attention' }>>
}

export interface CreateFileSystemAccessSettlementAuthorityOptions {
  readonly intent: ReceiveIntent
  readonly repository: FSASettlementRepository
  readonly lifecycleLeaseId: string
  readonly transferJobId: string
  /** Exact durable state to restore when a continuation cannot admit an execution. */
  readonly admissionFallback?: ReceiveAdmissionFallback
  readonly clock?: () => number
  readonly diagnostics?: OutputDiagnosticsPorts
  readonly trace?: (event: FSASettlementTraceEvent) => void
}

export interface PreparedFileSystemAccessSettlement {
  readonly intent: DirectTreeIntent
  readonly admissionFallback?: ReceiveAdmissionFallback
  readonly directoryScope: DirectoryAdmissionScope
}


export {
  catchUpFileSystemAccessPendingOutcome,
} from './settlement-catch-up'
export type {
  FileSystemAccessPendingOutcomeCatchUpResult,
} from './settlement-catch-up'

export async function prepareFileSystemAccessSettlement(
  intentInput: ReceiveIntent,
  admissionFallbackInput?: ReceiveAdmissionFallback,
): Promise<PreparedFileSystemAccessSettlement> {
  const intent = await requireDirectTreeIntent(intentInput)
  const admissionFallback = snapshotReceiveAdmissionFallback(intent, admissionFallbackInput)
  return Object.freeze({
    intent,
    ...(admissionFallback === undefined ? {} : { admissionFallback }),
    directoryScope: await createDirectoryAdmissionScope(intent),
  })
}

export function activatePreparedFileSystemAccessSettlement(
  prepared: PreparedFileSystemAccessSettlement,
  options: Omit<CreateFileSystemAccessSettlementAuthorityOptions, 'intent' | 'admissionFallback'>,
): FileSystemAccessOperationSettlementAuthority {
  return new FSAOperationSettlementAuthority({
    intent: prepared.intent,
    repository: options.repository,
    lifecycleLeaseId: snapshotIdentity(options.lifecycleLeaseId, 16, 'lifecycle lease ID'),
    transferJobId: snapshotTransferJobId(options.transferJobId),
    ...(prepared.admissionFallback === undefined
      ? {}
      : { admissionFallback: prepared.admissionFallback }),
    clock: options.clock ?? Date.now,
    ...(options.diagnostics === undefined
      ? {}
      : { diagnostics: options.diagnostics }),
    ...(options.trace === undefined ? {} : { trace: options.trace }),
    directoryScope: prepared.directoryScope,
  })
}

/**
 * Owns the reducer and durable receipt cut for one FSA operation. Transfer supplies
 * evidence, but only this authority can turn an observed namespace into lifecycle state.
 */
export async function createFileSystemAccessSettlementAuthority(
  options: CreateFileSystemAccessSettlementAuthorityOptions,
): Promise<FileSystemAccessOperationSettlementAuthority> {
  const prepared = await prepareFileSystemAccessSettlement(options.intent, options.admissionFallback)
  return activatePreparedFileSystemAccessSettlement(prepared, {
    repository: options.repository,
    lifecycleLeaseId: options.lifecycleLeaseId,
    transferJobId: options.transferJobId,
    ...(options.clock === undefined ? {} : { clock: options.clock }),
    ...(options.diagnostics === undefined
      ? {}
      : { diagnostics: options.diagnostics }),
    ...(options.trace === undefined ? {} : { trace: options.trace }),
  })
}
