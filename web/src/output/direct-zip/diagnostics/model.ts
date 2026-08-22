export const DIRECT_ZIP_DIAGNOSTIC_PLAN_KIND = 'direct-resumable-zip' as const

export const DIRECT_ZIP_DIAGNOSTIC_MILESTONES = Object.freeze([
  'session-started',
  'session-restored',
  'session-paused',
  'session-resumed',
  'session-settled',
  'session-stopped',
  'permission-query',
  'permission-request',
  'candidate-persist',
  'exact-name-lookup',
  'exact-name-create',
  'bootstrap-write',
  'bootstrap-close',
  'snapshot',
  'epoch-open',
  'epoch-write',
  'epoch-truncate',
  'epoch-close',
  'epoch-abort',
  'range-proof',
  'cleanup-delete',
  'cleanup-observe',
  'epoch-opened',
  'member-admitted',
  'member-resumed',
  'checkpoint-policy-decided',
  'candidate-staged',
  'predecessor-verified',
  'epoch-close-observed',
  'candidate-resolved',
  'checkpoint-promoted',
  'closing-entered',
  'central-record-replayed',
  'completion-verified',
  'writer-gated',
  'writer-failed',
] as const)

export type DirectZipDiagnosticMilestone =
  (typeof DIRECT_ZIP_DIAGNOSTIC_MILESTONES)[number]

export const DIRECT_ZIP_CHECKPOINT_PHASES = Object.freeze([
  'between-members',
  'inside-member',
  'closing',
] as const)

export type DirectZipCheckpointPhase =
  (typeof DIRECT_ZIP_CHECKPOINT_PHASES)[number]

export const DIRECT_ZIP_EPOCH_OFFSET_CLASSES = Object.freeze([
  'not-positioned',
  'member-header',
  'member-payload',
  'member-descriptor',
  'central-directory',
  'closing-tail',
] as const)

export type DirectZipEpochOffsetClass =
  (typeof DIRECT_ZIP_EPOCH_OFFSET_CLASSES)[number]

export const DIRECT_ZIP_PREFIX_COPY_DECISIONS = Object.freeze([
  'not-evaluated',
  'admit',
  'decline-evidence-unavailable',
  'decline-prefix-copy-budget',
  'decline-cumulative-copy-budget',
] as const)

export type DirectZipPrefixCopyDecision =
  (typeof DIRECT_ZIP_PREFIX_COPY_DECISIONS)[number]

export const DIRECT_ZIP_PEAK_SPACE_DECISIONS = Object.freeze([
  'not-evaluated',
  'within-budget',
  'confirmation-required',
  'destination-space-required',
  'evidence-unavailable',
] as const)

export type DirectZipPeakSpaceDecision =
  (typeof DIRECT_ZIP_PEAK_SPACE_DECISIONS)[number]

export const DIRECT_ZIP_PERMISSION_DECISIONS = Object.freeze([
  'not-evaluated',
  'granted',
  'authorization-required',
] as const)

export type DirectZipPermissionDecision =
  (typeof DIRECT_ZIP_PERMISSION_DECISIONS)[number]

export const DIRECT_ZIP_IDENTITY_DECISIONS = Object.freeze([
  'not-evaluated',
  'verified',
  'target-verification-required',
  'restart-required',
  'needs-attention',
] as const)

export type DirectZipIdentityDecision =
  (typeof DIRECT_ZIP_IDENTITY_DECISIONS)[number]

export const DIRECT_ZIP_SPACE_DECISIONS = Object.freeze([
  'not-evaluated',
  'admitted',
  'destination-space-required',
  'quota-exceeded',
  'native-effect-ambiguous',
] as const)

export type DirectZipSpaceDecision =
  (typeof DIRECT_ZIP_SPACE_DECISIONS)[number]

export const DIRECT_ZIP_CLEANUP_DECISIONS = Object.freeze([
  'not-evaluated',
  'not-requested',
  'retained',
  'deleted',
  'needs-attention',
] as const)

export type DirectZipCleanupDecision =
  (typeof DIRECT_ZIP_CLEANUP_DECISIONS)[number]

export interface DirectZipDiagnosticDecisionSnapshot {
  readonly prefixCopy: DirectZipPrefixCopyDecision
  readonly peakSpace: DirectZipPeakSpaceDecision
  readonly permission: DirectZipPermissionDecision
  readonly identity: DirectZipIdentityDecision
  readonly space: DirectZipSpaceDecision
  readonly cleanup: DirectZipCleanupDecision
}

export interface DirectZipDiagnosticMilestoneInput {
  readonly operationId: string
  readonly sessionId: string
  readonly planKind: typeof DIRECT_ZIP_DIAGNOSTIC_PLAN_KIND
  readonly milestone: DirectZipDiagnosticMilestone
  readonly checkpointPhase: DirectZipCheckpointPhase
  readonly epochOffsetClass: DirectZipEpochOffsetClass
  readonly decisions: DirectZipDiagnosticDecisionSnapshot
  /** Retained only in the local record; the exported projection never reads it. */
  readonly rawFsaStageFacts?: unknown
  /** Retained only in the local record; export reduces it to a closed native class. */
  readonly rawException?: unknown
}

export interface DirectZipLocalDiagnosticRecord extends DirectZipDiagnosticMilestoneInput {
  readonly observedAtMilliseconds: number
  readonly rawFsaStageFactsObserved: boolean
  readonly rawExceptionObserved: boolean
}

export interface DirectZipDiagnosticClock {
  nowMilliseconds(): number
}
