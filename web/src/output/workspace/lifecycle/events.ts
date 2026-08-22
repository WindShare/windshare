import type {
  DirectoryFailureReason,
  DirectZipCheckpointPhase,
  ExternalAttemptReason,
  PackageFailureReason,
  PlanKind,
  PreparationAdmissionReason,
  ReceiveLifecycleState,
  RecoveryGateKind,
  RestartRequiredReason,
} from '../state'

interface LifecycleEventAuthority {
  readonly expectedGeneration: bigint
  readonly leaseId: string
}

export type LifecycleEvent =
  | Readonly<LifecycleEventAuthority & { kind: 'receive-started'; preparationId?: string }>
  | Readonly<LifecycleEventAuthority & { kind: 'preparation-admitted' }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'preparation-rejected'
      reason: PreparationAdmissionReason
      cleanupReceiptDigest: string
    }>
  | Readonly<LifecycleEventAuthority & { kind: 'pause-requested'; stage: 'receive' | 'package' }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'pause-verified'
      stage: 'receive'
      checkpointSetDigest: string
      completedFileCount: bigint
      completedBytes: bigint
      partialReceiptDigest?: string
    }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'direct-zip-pause-verified'
      checkpointDigest: string
      safeSelectedPayloadBytes: bigint
      committedArchiveLength: bigint
      checkpointPhase: DirectZipCheckpointPhase
    }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'direct-zip-recovery-gated'
      gateKind: RecoveryGateKind
      recoveryGateDigest: string
    }>
  | Readonly<LifecycleEventAuthority & { kind: 'direct-zip-recovery-resumed' }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'pause-verified'
      stage: 'package'
      sealedMaterializationDigest: string
      tempCleanupProofDigest: string
    }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'resume-started'
      packageTempObjectId?: string
    }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'resume-admission-failed'
      checkpointSetDigest: string
      completedFileCount: bigint
      completedBytes: bigint
      expiresAt: number
      partialReceiptDigest?: string
    }>
  | Readonly<LifecycleEventAuthority & { kind: 'terminal-catch-up-reacquired' }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'stop-requested'
      successCount: bigint
      failureCount: bigint
      receiptDigest: string
      cleanupReceiptDigest: string
    }>
  | Readonly<LifecycleEventAuthority & { kind: 'discovery-completed' }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'selected-entry-failed'
      reason: DirectoryFailureReason
    }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'tree-finalization-completed'
      outcome: 'published' | 'resumable' | 'partial-directory' | 'discarded'
      receiptDigest: string
      checkpointSetDigest?: string
      completedFileCount: bigint
      completedBytes: bigint
      successCount: bigint
      failureCount: bigint
      retryable?: boolean
    }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'materialization-failed'
      reason: DirectoryFailureReason
    }>
  | Readonly<LifecycleEventAuthority & { kind: 'materialization-completed' }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'restart-boundary-verified'
      reason: RestartRequiredReason
      receiptDigest: string
    }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'materialization-seal-verified'
      sealedMaterializationDigest: string
    }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'package-started'
      packageTempObjectId: string
    }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'package-retryable-failure'
      reason: PackageFailureReason
      tempCleanupProofDigest: string
    }>
  | Readonly<LifecycleEventAuthority & { kind: 'package-seal-verified'; packageDigest: string }>
  | Readonly<LifecycleEventAuthority & { kind: 'wait-record-persisted' }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'save-requested'
      publicationAttemptId: string
    }>
  | Readonly<LifecycleEventAuthority & { kind: 'publication-committed'; receiptDigest: string }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'publication-not-committed'
      reason: ExternalAttemptReason
    }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'publication-unknown'
      lastVerifiedRecordDigest: string
    }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'handoff-requested'
      attemptKind: 'workspace' | 'portable'
      attemptId: string
    }>
  | Readonly<LifecycleEventAuthority & { kind: 'handoff-started' }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'handoff-not-started'
      reason: ExternalAttemptReason
      expiryReceiptDigest?: string
    }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'handoff-unknown'
      lastVerifiedRecordDigest: string
    }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'discard-requested'
      ownedObjectCount: bigint
      ownedRecordCount: bigint
    }>
  | Readonly<LifecycleEventAuthority & { kind: 'cleanup-verified'; cleanupReceiptDigest: string }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'cleanup-unknown'
      lastVerifiedRecordDigest: string
    }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'expiry-observed'
      expiryReceiptDigest: string
      cleanupState: 'clean' | 'cleanup-pending'
    }>
  | Readonly<LifecycleEventAuthority & {
      kind: 'ownership-unknown'
      lastVerifiedRecordDigest: string
    }>
  | Readonly<LifecycleEventAuthority & { kind: 'abandoned-operation-observed' }>

export interface LifecycleReducerContext {
  readonly planKind: PlanKind
  readonly preparationRequired: boolean
  readonly activeLeaseId: string
  readonly nowMilliseconds: number
}

export interface LifecycleReduction {
  readonly status: 'applied' | 'stale' | 'not-due'
  readonly state: ReceiveLifecycleState
}
