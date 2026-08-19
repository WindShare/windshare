import { BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS } from '../../../transfer/intent'
import type { PersistentTreeTraceEvent } from '../../persistent-tree/contracts'
import {
  validateReceiveIntent,
  type ReceiveIntent,
} from '../../../transfer/intent'
import {
  type WorkspaceBudgetAdmission,
  type WorkspaceBudgetV1,
  type WorkspaceCapacitySnapshot,
} from '../budget'
import {
  type WorkspaceOwnedCleanupPort,
  type WorkspaceOwnedObjectCleanupTarget,
} from '../cleanup'
import { snapshotIdentity } from '../canonical'
import type {
  CleanupReceiptV1,
  ExpiryReceiptV1,
  PreparationAdmissionReceiptV1,
} from '../receipts'
import {
  type ExternalAttemptReason,
  type PackageFailureReason,
  type PreparationAdmissionReason,
  type ReceiveLifecycleState,
} from '../state'

export const WORKSPACE_HANDLE_ROOT = 16 as const
export const WORKSPACE_HANDLE_RAW_OBJECT = 17 as const
export const WORKSPACE_HANDLE_PACKAGE_OBJECT = 18 as const
export const WORKSPACE_HANDLE_ZIP_LAYOUT = 19 as const
export const WORKSPACE_HANDLE_PUBLICATION_TARGET = 20 as const

const CONTENT_GATES = new WeakSet<object>()
const U64_MAXIMUM = 0xffff_ffff_ffff_ffffn
const WORKSPACE_ZIP_LAYOUT_HANDLE_DOMAIN = 'windshare/workspace-zip-layout/v1'

export function workspaceZipLayoutHandleId(operationId: string, preparationId: string): string {
  return `${WORKSPACE_ZIP_LAYOUT_HANDLE_DOMAIN}/${snapshotIdentity(
    operationId,
    16,
    'operation ID',
  )}/${snapshotIdentity(preparationId, 16, 'preparation ID')}`
}

export interface WorkspaceContentRequestCounter {
  count(): bigint
}

export interface WorkspaceBudgetClaim {
  readonly budgetDigest: string
  readonly capacity: WorkspaceCapacitySnapshot
  readonly admission: Extract<WorkspaceBudgetAdmission, { kind: 'accepted' }>
  release(): Promise<void>
}

export type WorkspaceBudgetClaimResult =
  | Readonly<{ kind: 'accepted'; claim: WorkspaceBudgetClaim }>
  | Readonly<{
      kind: 'rejected'
      capacity: WorkspaceCapacitySnapshot
      admission: Extract<WorkspaceBudgetAdmission, { kind: 'rejected' }>
    }>

export interface WorkspaceBudgetAuthority {
  claim(budget: WorkspaceBudgetV1): Promise<WorkspaceBudgetClaimResult>
}

export interface WorkspaceContentGate {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly workspaceBudgetDigest: string
  readonly preparationManifestDigest?: string
}

export interface PackageTemporaryCleanupEvidence {
  readonly operationId: string
  readonly packageOwnedObjectId: string
  readonly packageHandleId: string
  readonly result: 'removed' | 'already-absent'
  readonly digest: string
}

export type WorkspaceReceiveIntent = ReceiveIntent & {
  readonly plan: Extract<ReceiveIntent['plan'], { kind: 'workspace-then-publish' }>
}

export type WorkspaceStageTraceEvent =
  | PersistentTreeTraceEvent
  | Readonly<{
      name: 'receive.preparation.started'
      operation_id: string
      receive_intent_digest: string
      preparation_id: string
      artifact_kind: 'zip-archive'
    }>
  | Readonly<{
      name: 'receive.preparation.sealed'
      operation_id: string
      receive_intent_digest: string
      preparation_digest: string
      entry_count: bigint
      file_count: bigint
      directory_count: bigint
      selected_bytes: bigint
      metadata_bytes: bigint
    }>
  | Readonly<{
      name: 'receive.preparation_admission.accepted'
      operation_id: string
      receive_intent_digest: string
      plan_kind: 'workspace-then-publish'
      admission_kind: 'workspace-budget'
      artifact_bytes: bigint
      metadata_bytes: bigint
      unique_raw_bytes: bigint
      package_bytes: bigint
      peak_temporary_bytes: bigint
      durable_metadata_bytes: bigint
      peak_owned_bytes: bigint
      limit_class: 'none'
    }>
  | Readonly<{
      name: 'receive.preparation_admission.rejected'
      operation_id: string
      receive_intent_digest: string
      plan_kind: 'workspace-then-publish'
      admission_kind: 'workspace-budget'
      artifact_bytes: bigint
      metadata_bytes: bigint
      unique_raw_bytes: bigint
      package_bytes: bigint
      peak_temporary_bytes: bigint
      durable_metadata_bytes: bigint
      peak_owned_bytes: bigint
      limit_class: 'workspace-job' | 'workspace-process' | 'workspace-quota'
      preparation_admission_reason: PreparationAdmissionReason
    }>
  | Readonly<{
      name: 'receive.materialization.completed'
      operation_id: string
      receive_intent_digest: string
      transfer_job_id: string
      entry_count: bigint
      file_count: bigint
      directory_count: bigint
      raw_bytes: bigint
    }>
  | Readonly<{
      name: 'receive.materialization.sealed'
      operation_id: string
      receive_intent_digest: string
      sealed_materialization_digest: string
      entry_count: bigint
      file_count: bigint
      directory_count: bigint
      raw_bytes: bigint
    }>
  | Readonly<{
      name: 'receive.package.started'
      operation_id: string
      sealed_materialization_digest: string
      artifact_kind: 'original-file' | 'zip-archive'
    }>
  | Readonly<{
      name: 'receive.package.sealed'
      operation_id: string
      package_digest: string
      layout_digest: string
      artifact_bytes: bigint
    }>
  | Readonly<{
      name: 'receive.package.retry_started'
      operation_id: string
      sealed_materialization_digest: string
      package_failure_reason: PackageFailureReason
    }>
  | Readonly<{
      name: 'receive.waiting_to_save'
      operation_id: string
      package_digest: string
      expires_at_ms: number
    }>
  | Readonly<{
      name: 'receive.publication.started'
      operation_id: string
      package_digest: string
      publication_attempt_id: string
      publication_route: 'managed' | 'handoff'
    }>
  | Readonly<{
      name: 'receive.publication.committed'
      operation_id: string
      package_digest: string
      publication_attempt_id: string
      artifact_bytes: bigint
    }>
  | Readonly<{
      name: 'receive.publication.not_committed'
      operation_id: string
      package_digest: string
      publication_attempt_id: string
      external_attempt_reason: ExternalAttemptReason
    }>
  | Readonly<{
      name: 'receive.publication.unknown'
      operation_id: string
      package_digest: string
      publication_attempt_id: string
      needs_attention_reason: 'publication-unknown'
    }>
  | Readonly<{
      name: 'receive.operation.needs_attention'
      operation_id: string
      prior_state: ReceiveLifecycleState['kind']
      needs_attention_reason: 'cleanup-unknown' | 'publication-unknown' | 'target-ownership-unknown'
    }>
  | Readonly<{
      name: 'receive.handoff.started'
      operation_id: string
      attempt_kind: 'workspace'
      attempt_id: string
      package_digest_present: true
      package_digest: string
      object_url_lease_ms: bigint
    }>
  | Readonly<{
      name: 'receive.handoff.download_started'
      operation_id: string
      attempt_kind: 'workspace'
      attempt_id: string
      package_digest_present: true
      package_digest: string
      retryable_until_present: true
      retryable_until_ms: number
    }>
  | Readonly<{
      name: 'receive.handoff.not_started'
      operation_id: string
      attempt_kind: 'workspace'
      attempt_id: string
      external_attempt_reason: ExternalAttemptReason
    }>
  | Readonly<{
      name: 'receive.handoff.unknown'
      operation_id: string
      attempt_kind: 'workspace'
      attempt_id: string
      needs_attention_reason: 'publication-unknown'
    }>
  | Readonly<{
      name: 'receive.materialization.paused'
      operation_id: string
      receive_intent_digest: string
      resumable_stage: 'receive'
      completed_file_count: bigint
      completed_bytes: bigint
      expires_at_ms: number
    }>
  | Readonly<{
      name: 'receive.continuation.admission_failed'
      operation_id: string
      receive_intent_digest: string
      restored_checkpoint_set_digest: string
      restored_completed_file_count: bigint
      restored_completed_bytes: bigint
      restored_expires_at_ms: number
    }>
  | Readonly<{
      name: 'receive.operation.expired'
      operation_id: string
      prior_stable_state: ExpiryReceiptV1['priorStableState']
      expires_at_ms: number
    }>
  | Readonly<{
      name: 'receive.operation.discarded' | 'receive.operation.cleanup_completed'
      operation_id: string
      cleanup_generation: bigint
      removed_object_count: bigint
      removed_record_count: bigint
    }>

export type WorkspaceStageTraceListener = (event: WorkspaceStageTraceEvent) => void

export interface AdmittedWorkspaceContent {
  readonly gate: WorkspaceContentGate
  readonly budget: WorkspaceBudgetV1
  readonly admissionReceipt: PreparationAdmissionReceiptV1
  readonly claim: WorkspaceBudgetClaim
}

export interface WorkspaceCleanupRequest {
  readonly targets: readonly WorkspaceOwnedObjectCleanupTarget[]
  readonly metadataHandleIds?: readonly string[]
  readonly port: WorkspaceOwnedCleanupPort
}

export type WorkspaceCleanupResult =
  | Readonly<{
      kind: 'clean'
      receipt: CleanupReceiptV1
      state: ReceiveLifecycleState
    }>
  | Readonly<{
      kind: 'retryable-failure'
      state: ReceiveLifecycleState
    }>
  | Readonly<{
      kind: 'needs-attention'
      receipt: CleanupReceiptV1
      state: Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>
    }>



export function assertWorkspaceContentGate(
  gate: WorkspaceContentGate,
  expected: {
    readonly operationId: string
    readonly receiveIntentDigest: string
    readonly workspaceBudgetDigest: string
  },
): void {
  if (!CONTENT_GATES.has(gate as object) ||
      gate.operationId !== expected.operationId ||
      gate.receiveIntentDigest !== expected.receiveIntentDigest ||
      gate.workspaceBudgetDigest !== expected.workspaceBudgetDigest) {
    throw new TypeError('workspace content gate was not issued by committed admission')
  }
}

export function issueContentGate(input: WorkspaceContentGate): WorkspaceContentGate {
  const gate = Object.freeze({ ...input })
  CONTENT_GATES.add(gate)
  return gate
}

export async function requireWorkspaceIntent(
  candidate: ReceiveIntent,
): Promise<ReceiveIntent & {
  readonly plan: Extract<ReceiveIntent['plan'], { kind: 'workspace-then-publish' }>
}> {
  const intent = await validateReceiveIntent(candidate)
  if (intent.plan.kind !== 'workspace-then-publish' || intent.artifact.kind === 'directory-tree') {
    throw new TypeError('workspace stages require a complete-artifact workspace intent')
  }
  return intent as ReceiveIntent & {
    readonly plan: Extract<ReceiveIntent['plan'], { kind: 'workspace-then-publish' }>
  }
}

export function checkedAdd(...values: readonly bigint[]): bigint {
  let total = 0n
  for (const value of values) {
    if (typeof value !== 'bigint' || value < 0n) throw new TypeError('metadata byte count is invalid')
    total += value
    if (total > U64_MAXIMUM) throw new RangeError('durable metadata byte count overflow')
  }
  return total
}

export function checkedLeaseEnd(startedAt: number): number {
  const duration = Number(BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS)
  if (!Number.isSafeInteger(startedAt) || startedAt < 0 ||
      startedAt > Number.MAX_SAFE_INTEGER - duration) {
    throw new TypeError('handoff URL lease start is invalid')
  }
  return startedAt + duration
}

export function mergeHandleIds(...groups: readonly (readonly string[])[]): readonly string[] {
  const values = groups.flatMap((group) => [...group])
  if (values.some((value) => typeof value !== 'string' || value.length === 0) ||
      new Set(values).size !== values.length) {
    throw new TypeError('workspace cleanup handle inventory is invalid')
  }
  values.sort()
  return Object.freeze(values)
}

export function stableStateKind(
  state: ReceiveLifecycleState,
): ExpiryReceiptV1['priorStableState'] {
  switch (state.kind) {
    case 'resumable-receive':
    case 'resumable-package':
    case 'waiting-to-save':
      return state.kind
    case 'download-started':
      if (state.attemptKind === 'workspace') return state.kind
      break
  }
  throw new TypeError('workspace state is not durably expirable')
}
