import type { MaterializationPlan } from '../../transfer/intent'
import { snapshotIdentity } from './canonical'

export const STABLE_RETENTION_MILLISECONDS = 86_400_000
const MAXIMUM_RECOVERY_SELECTION_VALUE = 0xffff_ffff_ffff_ffffn

export type PlanKind = MaterializationPlan['kind']
export type ResumableStage = 'receive' | 'package'
export type OwnedCleanupState = 'clean' | 'cleanup-pending'
export type NeedsAttentionReason =
  | 'target-ownership-unknown'
  | 'publication-unknown'
  | 'cleanup-unknown'
export type RestartRequiredReason =
  | 'direct-atomic-rolled-back'
  | 'portable-aborted'
  | 'source-revision-changed'
  | 'preparation-invalidated'
  | 'content-session-ended'
  | 'target-deleted'
export type DirectZipCheckpointPhase = 'between-members' | 'inside-member' | 'closing'
export type RecoveryGateKind =
  | 'authorization-required'
  | 'target-verification-required'
  | 'destination-space-required'
export type RetainedLifecycleKind =
  | 'resumable-receive'
  | 'resumable-package'
  | 'waiting-to-save'
  | 'download-started'
  | RecoveryGateKind
export type PartialDirectoryReason = 'failures' | 'stopped'
export type RecoveryDiscoveryState = 'complete' | 'failed'

/** Selection evidence is retained with the lifecycle so a checkpoint snapshot cannot redefine its scope. */
export interface RecoverySelectionFacts {
  readonly discoveredFileCount: bigint
  readonly discoveredBytes: bigint
  readonly discovery: RecoveryDiscoveryState
}
export type PreparationAdmissionReason =
  | 'entry-limit'
  | 'metadata-limit'
  | 'artifact-limit'
  | 'job-workspace-limit'
  | 'process-workspace-limit'
  | 'quota-insufficient'
  | 'generation-mismatch'
  | 'arithmetic-overflow'
export type DirectoryFailureReason =
  | 'file-open-failed'
  | 'source-revision-changed'
  | 'content-read-failed'
  | 'output-write-failed'
  | 'output-commit-failed'
  | 'directory-finalize-failed'
export type PackageFailureReason = 'quota-insufficient' | 'writer-failed' | 'layout-mismatch'
export type ExternalAttemptReason =
  | 'user-cancelled'
  | 'permission-denied'
  | 'target-unavailable'
  | 'target-collision'

export const RECEIVE_STATE_INTENT_FROZEN = 1 as const
export const RECEIVE_STATE_PREPARING = 2 as const
export const RECEIVE_STATE_RECEIVING = 3 as const
export const RECEIVE_STATE_RESUMABLE_RECEIVE = 4 as const
export const RECEIVE_STATE_FINALIZING_TREE = 5 as const
export const RECEIVE_STATE_COMMITTING_ATOMIC = 6 as const
export const RECEIVE_STATE_MATERIALIZATION_SEALED = 7 as const
export const RECEIVE_STATE_PACKAGING = 8 as const
export const RECEIVE_STATE_RESUMABLE_PACKAGE = 9 as const
export const RECEIVE_STATE_ARTIFACT_SEALED = 10 as const
export const RECEIVE_STATE_WAITING_TO_SAVE = 11 as const
export const RECEIVE_STATE_PUBLISHING_MANAGED = 12 as const
export const RECEIVE_STATE_HANDING_OFF = 13 as const
export const RECEIVE_STATE_PUBLISHED = 14 as const
export const RECEIVE_STATE_DOWNLOAD_STARTED = 15 as const
export const RECEIVE_STATE_PARTIAL_DIRECTORY = 16 as const
export const RECEIVE_STATE_RESTART_REQUIRED = 17 as const
export const RECEIVE_STATE_DISCARDED = 18 as const
export const RECEIVE_STATE_EXPIRED = 19 as const
export const RECEIVE_STATE_NEEDS_ATTENTION = 20 as const
export const RECEIVE_STATE_AUTHORIZATION_REQUIRED = 21 as const
export const RECEIVE_STATE_TARGET_VERIFICATION_REQUIRED = 22 as const
export const RECEIVE_STATE_DESTINATION_SPACE_REQUIRED = 23 as const

export type ReceiveStateByte =
  | typeof RECEIVE_STATE_INTENT_FROZEN
  | typeof RECEIVE_STATE_PREPARING
  | typeof RECEIVE_STATE_RECEIVING
  | typeof RECEIVE_STATE_RESUMABLE_RECEIVE
  | typeof RECEIVE_STATE_FINALIZING_TREE
  | typeof RECEIVE_STATE_COMMITTING_ATOMIC
  | typeof RECEIVE_STATE_MATERIALIZATION_SEALED
  | typeof RECEIVE_STATE_PACKAGING
  | typeof RECEIVE_STATE_RESUMABLE_PACKAGE
  | typeof RECEIVE_STATE_ARTIFACT_SEALED
  | typeof RECEIVE_STATE_WAITING_TO_SAVE
  | typeof RECEIVE_STATE_PUBLISHING_MANAGED
  | typeof RECEIVE_STATE_HANDING_OFF
  | typeof RECEIVE_STATE_PUBLISHED
  | typeof RECEIVE_STATE_DOWNLOAD_STARTED
  | typeof RECEIVE_STATE_PARTIAL_DIRECTORY
  | typeof RECEIVE_STATE_RESTART_REQUIRED
  | typeof RECEIVE_STATE_DISCARDED
  | typeof RECEIVE_STATE_EXPIRED
  | typeof RECEIVE_STATE_NEEDS_ATTENTION
  | typeof RECEIVE_STATE_AUTHORIZATION_REQUIRED
  | typeof RECEIVE_STATE_TARGET_VERIFICATION_REQUIRED
  | typeof RECEIVE_STATE_DESTINATION_SPACE_REQUIRED

interface LifecycleStateBase {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly generation: bigint
}

export type ReceiveLifecycleState =
  | Readonly<LifecycleStateBase & { kind: 'intent-frozen' }>
  | Readonly<LifecycleStateBase & { kind: 'preparing'; preparationId: string }>
  | Readonly<LifecycleStateBase & { kind: 'receiving'; activeLeaseId: string }>
  | Readonly<LifecycleStateBase & {
      kind: 'resumable-receive'
      payloadKind: 'file-set'
      checkpointSetDigest: string
      completedFileCount: bigint
      completedBytes: bigint
      selectionFacts: RecoverySelectionFacts
      expiresAt: number
      partialReceiptDigest?: string
    }>
  | Readonly<LifecycleStateBase & {
      kind: 'resumable-receive'
      payloadKind: 'direct-zip'
      directZipCheckpointDigest: string
      safeSelectedPayloadBytes: bigint
      committedArchiveLength: bigint
      checkpointPhase: DirectZipCheckpointPhase
      expiresAt: number
    }>
  | Readonly<LifecycleStateBase & { kind: 'finalizing-tree'; activeLeaseId: string }>
  | Readonly<LifecycleStateBase & { kind: 'committing-atomic'; activeLeaseId: string }>
  | Readonly<LifecycleStateBase & {
      kind: 'materialization-sealed'
      sealedMaterializationDigest: string
    }>
  | Readonly<LifecycleStateBase & {
      kind: 'packaging'
      activeLeaseId: string
      sealedMaterializationDigest: string
      packageTempObjectId: string
    }>
  | Readonly<LifecycleStateBase & {
      kind: 'resumable-package'
      sealedMaterializationDigest: string
      tempCleanupProofDigest: string
      expiresAt: number
    }>
  | Readonly<LifecycleStateBase & { kind: 'artifact-sealed'; packageDigest: string }>
  | Readonly<LifecycleStateBase & { kind: 'waiting-to-save'; packageDigest: string; expiresAt: number }>
  | Readonly<LifecycleStateBase & {
      kind: 'publishing-managed'
      activeLeaseId: string
      packageDigest: string
      publicationAttemptId: string
    }>
  | Readonly<LifecycleStateBase & {
      kind: 'handing-off'
      activeLeaseId: string
      attemptKind: 'workspace'
      attemptId: string
      packageDigest: string
      retainedDeadline: number
    }>
  | Readonly<LifecycleStateBase & {
      kind: 'handing-off'
      activeLeaseId: string
      attemptKind: 'portable'
      attemptId: string
    }>
  | Readonly<LifecycleStateBase & {
      kind: 'published'
      receiptDigest: string
      cleanupState: OwnedCleanupState
    }>
  | Readonly<LifecycleStateBase & {
      kind: 'download-started'
      attemptKind: 'workspace'
      attemptId: string
      packageDigest: string
      retryableUntil: number
    }>
  | Readonly<LifecycleStateBase & {
      kind: 'download-started'
      attemptKind: 'portable'
      attemptId: string
    }>
  | Readonly<LifecycleStateBase & {
      kind: 'partial-directory'
      reason: PartialDirectoryReason
      successCount: bigint
      failureCount: bigint
      receiptDigest: string
    }>
  | Readonly<LifecycleStateBase & {
      kind: 'restart-required'
      reason: RestartRequiredReason
      receiptDigest: string
    }>
  | Readonly<LifecycleStateBase & { kind: 'discarded'; cleanupReceiptDigest: string }>
  | Readonly<LifecycleStateBase & {
      kind: 'expired'
      priorStableState: RetainedLifecycleKind
      expiresAt: number
      cleanupState: OwnedCleanupState
      expiryReceiptDigest: string
    }>
  | Readonly<LifecycleStateBase & {
      kind: 'needs-attention'
      reason: NeedsAttentionReason
      lastVerifiedRecordDigest: string
    }>
  | Readonly<LifecycleStateBase & {
      kind: RecoveryGateKind
      recoveryGateDigest: string
      expiresAt: number
    }>

type StatePayload<T> = T extends LifecycleStateBase
  ? Omit<T, keyof LifecycleStateBase>
  : never
export type ReceiveLifecycleStatePayload = StatePayload<ReceiveLifecycleState>

export function initialReceiveLifecycleState(input: {
  readonly operationId: string
  readonly receiveIntentDigest: string
}): ReceiveLifecycleState {
  return Object.freeze({
    kind: 'intent-frozen',
    operationId: snapshotIdentity(input.operationId, 16, 'operation ID'),
    receiveIntentDigest: snapshotIdentity(input.receiveIntentDigest, 32, 'receive intent digest'),
    generation: 1n,
  })
}

export function nextReceiveLifecycleState(
  current: ReceiveLifecycleState,
  payload: ReceiveLifecycleStatePayload,
): ReceiveLifecycleState {
  if (current.generation >= 0xffff_ffff_ffff_ffffn) {
    throw new TypeError('receive lifecycle generation overflow')
  }
  const durablePayload = payload.kind === 'resumable-receive' && payload.payloadKind === 'file-set'
    ? Object.freeze({
        ...payload,
        selectionFacts: snapshotRecoverySelectionFacts(
          payload.selectionFacts,
          payload.completedFileCount,
          payload.completedBytes,
        ),
      })
    : payload
  return Object.freeze({
    ...durablePayload,
    operationId: current.operationId,
    receiveIntentDigest: current.receiveIntentDigest,
    generation: current.generation + 1n,
  }) as ReceiveLifecycleState
}

export function snapshotRecoverySelectionFacts(
  input: RecoverySelectionFacts,
  completedFileCount: bigint = 0n,
  completedBytes: bigint = 0n,
): RecoverySelectionFacts {
  if (typeof input !== 'object' || input === null ||
      typeof input.discoveredFileCount !== 'bigint' ||
      input.discoveredFileCount < 0n ||
      input.discoveredFileCount > MAXIMUM_RECOVERY_SELECTION_VALUE ||
      typeof input.discoveredBytes !== 'bigint' || input.discoveredBytes < 0n ||
      input.discoveredBytes > MAXIMUM_RECOVERY_SELECTION_VALUE ||
      (input.discovery !== 'complete' && input.discovery !== 'failed')) {
    throw new TypeError('recovery selection facts are invalid')
  }
  if (typeof completedFileCount !== 'bigint' || completedFileCount < 0n ||
      typeof completedBytes !== 'bigint' || completedBytes < 0n ||
      completedFileCount > input.discoveredFileCount || completedBytes > input.discoveredBytes ||
      (completedFileCount === 0n && completedBytes !== 0n) ||
      (input.discoveredFileCount === 0n && input.discoveredBytes !== 0n)) {
    throw new TypeError('recovery selection facts do not contain completed output')
  }
  return Object.freeze({
    discoveredFileCount: input.discoveredFileCount,
    discoveredBytes: input.discoveredBytes,
    discovery: input.discovery,
  })
}

export function stableDeadline(nowMilliseconds: number): number {
  if (!Number.isSafeInteger(nowMilliseconds) || nowMilliseconds < 0 ||
      nowMilliseconds > Number.MAX_SAFE_INTEGER - STABLE_RETENTION_MILLISECONDS) {
    throw new TypeError('stable-state clock cannot represent the retention deadline')
  }
  return nowMilliseconds + STABLE_RETENTION_MILLISECONDS
}

export function receiveStateByte(state: ReceiveLifecycleState): ReceiveStateByte {
  switch (state.kind) {
    case 'intent-frozen': return RECEIVE_STATE_INTENT_FROZEN
    case 'preparing': return RECEIVE_STATE_PREPARING
    case 'receiving': return RECEIVE_STATE_RECEIVING
    case 'resumable-receive': return RECEIVE_STATE_RESUMABLE_RECEIVE
    case 'finalizing-tree': return RECEIVE_STATE_FINALIZING_TREE
    case 'committing-atomic': return RECEIVE_STATE_COMMITTING_ATOMIC
    case 'materialization-sealed': return RECEIVE_STATE_MATERIALIZATION_SEALED
    case 'packaging': return RECEIVE_STATE_PACKAGING
    case 'resumable-package': return RECEIVE_STATE_RESUMABLE_PACKAGE
    case 'artifact-sealed': return RECEIVE_STATE_ARTIFACT_SEALED
    case 'waiting-to-save': return RECEIVE_STATE_WAITING_TO_SAVE
    case 'publishing-managed': return RECEIVE_STATE_PUBLISHING_MANAGED
    case 'handing-off': return RECEIVE_STATE_HANDING_OFF
    case 'published': return RECEIVE_STATE_PUBLISHED
    case 'download-started': return RECEIVE_STATE_DOWNLOAD_STARTED
    case 'partial-directory': return RECEIVE_STATE_PARTIAL_DIRECTORY
    case 'restart-required': return RECEIVE_STATE_RESTART_REQUIRED
    case 'discarded': return RECEIVE_STATE_DISCARDED
    case 'expired': return RECEIVE_STATE_EXPIRED
    case 'needs-attention': return RECEIVE_STATE_NEEDS_ATTENTION
    case 'authorization-required': return RECEIVE_STATE_AUTHORIZATION_REQUIRED
    case 'target-verification-required': return RECEIVE_STATE_TARGET_VERIFICATION_REQUIRED
    case 'destination-space-required': return RECEIVE_STATE_DESTINATION_SPACE_REQUIRED
  }
}

export function lifecycleDeadline(state: ReceiveLifecycleState): number | undefined {
  switch (state.kind) {
    case 'resumable-receive':
    case 'resumable-package':
    case 'waiting-to-save':
    case 'authorization-required':
    case 'target-verification-required':
    case 'destination-space-required':
      return state.expiresAt
    case 'download-started':
      return state.attemptKind === 'workspace' ? state.retryableUntil : undefined
    default:
      return undefined
  }
}

export function isTerminalLifecycleState(state: ReceiveLifecycleState): boolean {
  return state.kind === 'published' || state.kind === 'partial-directory' ||
    state.kind === 'restart-required' || state.kind === 'discarded' ||
    state.kind === 'expired' || state.kind === 'needs-attention'
}
