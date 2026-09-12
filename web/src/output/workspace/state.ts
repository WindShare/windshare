import type { MaterializationPlan } from '../../transfer/intent'
import { snapshotIdentity } from './canonical'
import { advanceReceiveTiming, receiveTimingFields, type ReceiveTiming } from './lifecycle/timing'

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
export const RECEIVE_STATE_NEEDS_ATTENTION = 20 as const
export const RECEIVE_STATE_AUTHORIZATION_REQUIRED = 21 as const
export const RECEIVE_STATE_TARGET_VERIFICATION_REQUIRED = 22 as const
export const RECEIVE_STATE_DESTINATION_SPACE_REQUIRED = 23 as const

const RECEIVE_STATE_BYTES_BY_KIND = Object.freeze({
  'intent-frozen': RECEIVE_STATE_INTENT_FROZEN,
  'preparing': RECEIVE_STATE_PREPARING,
  'receiving': RECEIVE_STATE_RECEIVING,
  'resumable-receive': RECEIVE_STATE_RESUMABLE_RECEIVE,
  'finalizing-tree': RECEIVE_STATE_FINALIZING_TREE,
  'committing-atomic': RECEIVE_STATE_COMMITTING_ATOMIC,
  'materialization-sealed': RECEIVE_STATE_MATERIALIZATION_SEALED,
  'packaging': RECEIVE_STATE_PACKAGING,
  'resumable-package': RECEIVE_STATE_RESUMABLE_PACKAGE,
  'artifact-sealed': RECEIVE_STATE_ARTIFACT_SEALED,
  'waiting-to-save': RECEIVE_STATE_WAITING_TO_SAVE,
  'publishing-managed': RECEIVE_STATE_PUBLISHING_MANAGED,
  'handing-off': RECEIVE_STATE_HANDING_OFF,
  'published': RECEIVE_STATE_PUBLISHED,
  'download-started': RECEIVE_STATE_DOWNLOAD_STARTED,
  'partial-directory': RECEIVE_STATE_PARTIAL_DIRECTORY,
  'restart-required': RECEIVE_STATE_RESTART_REQUIRED,
  'discarded': RECEIVE_STATE_DISCARDED,
  'needs-attention': RECEIVE_STATE_NEEDS_ATTENTION,
  'authorization-required': RECEIVE_STATE_AUTHORIZATION_REQUIRED,
  'target-verification-required': RECEIVE_STATE_TARGET_VERIFICATION_REQUIRED,
  'destination-space-required': RECEIVE_STATE_DESTINATION_SPACE_REQUIRED,
} satisfies Readonly<Record<ReceiveLifecycleState['kind'], number>>)

export type ReceiveStateByte = (typeof RECEIVE_STATE_BYTES_BY_KIND)[ReceiveLifecycleState['kind']]

const VALID_RECEIVE_STATE_BYTES: ReadonlySet<number> = new Set(Object.values(RECEIVE_STATE_BYTES_BY_KIND))

interface LifecycleStateBase {
  readonly timing?: ReceiveTiming
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly generation: bigint
}

/** Retained payload is retired by explicit deletion or verified publication; browser handoff proves neither. */
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
      partialReceiptDigest?: string
    }>
  | Readonly<LifecycleStateBase & {
      kind: 'resumable-receive'
      payloadKind: 'opfs-zip'
      objectId: string
      checkpointGeneration: bigint
      occupiedBytes: bigint
      completedFileCount: bigint
      completedBytes: bigint
      discoveryComplete: boolean
      pauseReason?: 'storage-pressure'
    }>
  | Readonly<LifecycleStateBase & {
      kind: 'resumable-receive'
      payloadKind: 'direct-zip'
      directZipCheckpointDigest: string
      safeSelectedPayloadBytes: bigint
      committedArchiveLength: bigint
      checkpointPhase: DirectZipCheckpointPhase
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
    }>
  | Readonly<LifecycleStateBase & { kind: 'artifact-sealed'; packageDigest: string }>
  | Readonly<LifecycleStateBase & { kind: 'waiting-to-save'; packageDigest: string }>
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
      kind: 'needs-attention'
      reason: NeedsAttentionReason
      lastVerifiedRecordDigest: string
    }>
  | Readonly<LifecycleStateBase & {
      kind: RecoveryGateKind
      recoveryGateDigest: string
    }>

type StatePayload<T> = T extends LifecycleStateBase
  ? Omit<T, keyof LifecycleStateBase>
  : never
export type ReceiveLifecycleStatePayload = StatePayload<ReceiveLifecycleState>

export function initialReceiveLifecycleState(input: {
  readonly startedAtMilliseconds?: number
  readonly operationId: string
  readonly receiveIntentDigest: string
}): ReceiveLifecycleState {
  return Object.freeze({
    kind: 'intent-frozen',
    operationId: snapshotIdentity(input.operationId, 16, 'operation ID'),
    receiveIntentDigest: snapshotIdentity(input.receiveIntentDigest, 32, 'receive intent digest'),
    generation: 1n,
    ...receiveTimingFields({ startedAtMilliseconds: input.startedAtMilliseconds }),
  })
}

export function nextReceiveLifecycleState(
  current: ReceiveLifecycleState,
  payload: ReceiveLifecycleStatePayload,
  clock: () => number = Date.now,
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
    ...advanceReceiveTiming(current.timing, payload.kind, clock),
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

export function receiveStateByte(state: ReceiveLifecycleState): ReceiveStateByte {
  return RECEIVE_STATE_BYTES_BY_KIND[state.kind]
}

export function isReceiveStateByte(value: number): value is ReceiveStateByte {
  return VALID_RECEIVE_STATE_BYTES.has(value)
}

export function isTerminalLifecycleState(state: ReceiveLifecycleState): boolean {
  return state.kind === 'published' || state.kind === 'partial-directory' ||
    state.kind === 'restart-required' || state.kind === 'discarded' ||
    state.kind === 'needs-attention'
}
