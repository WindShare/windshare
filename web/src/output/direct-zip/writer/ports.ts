import type { DirectZipOwnershipMarkerV1 } from '../format'
import type {
  DirectZipCompletionProofV1,
  DirectZipEpochCandidateV1,
  DirectZipEpochProofV1,
  DirectZipMemberAdmissionV1,
  DirectZipWriterCheckpointV1,
  DirectZipWriterPageStateV1,
} from './model'

export interface DirectZipPositionedEpochWritable {
  write(position: bigint, bytes: Uint8Array): Promise<void>
  closeOnce(): Promise<DirectZipCloseAttemptV1>
  abort(reason: unknown): Promise<void>
}

export type DirectZipCloseAttemptV1 =
  | Readonly<{ kind: 'closed' }>
  | Readonly<{ kind: 'threw'; error: unknown }>

export type DirectZipPredecessorVerificationV1 =
  | Readonly<{ kind: 'accepted-fast' }>
  | Readonly<{ kind: 'digest-readback-required' }>
  | Readonly<{ kind: 'authorization-required' }>
  | Readonly<{ kind: 'target-deleted' }>
  | Readonly<{ kind: 'target-verification-required' }>
  | Readonly<{ kind: 'foreign-target' }>

export type DirectZipOpenEpochResultV1 =
  | Readonly<{ kind: 'opened'; writable: DirectZipPositionedEpochWritable }>
  | Readonly<{ kind: 'authorization-required' }>
  | Readonly<{ kind: 'destination-space-required' }>
  | Readonly<{ kind: 'target-deleted' }>
  | Readonly<{ kind: 'target-verification-required' }>

export type DirectZipObservedLengthV1 =
  'predecessor' | 'candidate' | 'unknown-tail' | 'other'

export type DirectZipObservationMatchV1 = 'predecessor' | 'candidate' | 'neither'
export type DirectZipOwnershipObservationV1 = 'matching' | 'foreign' | 'ambiguous'
export type DirectZipDigestObservationV1 = 'not-read' | 'verified' | 'mismatch'
export type DirectZipCandidateIntegrityV1 =
  'writer-bounded-proof' | 'not-read' | 'verified' | 'mismatch'

export interface DirectZipCandidateObservationV1 {
  readonly permission: 'granted' | 'unavailable'
  readonly presence: 'present' | 'deleted'
  readonly ownership: DirectZipOwnershipObservationV1
  readonly length: DirectZipObservedLengthV1
  readonly observationMatch: DirectZipObservationMatchV1
  readonly candidateIntegrity: DirectZipCandidateIntegrityV1
  readonly predecessorIntegrity: DirectZipDigestObservationV1
  readonly observationDigest?: Uint8Array
  readonly destinationSpaceFailure?: boolean
  readonly candidateResolvedBeforeSpaceFailure?: boolean
}

export type DirectZipTruncateResultV1 =
  | Readonly<{ kind: 'truncated'; observationDigest: Uint8Array }>
  | Readonly<{ kind: 'authorization-required' }>
  | Readonly<{ kind: 'destination-space-required' }>
  | Readonly<{ kind: 'target-verification-required' }>
  | Readonly<{ kind: 'refused' }>

export interface DirectZipBoundedCompletionReadV1 {
  readonly localOwnershipHeader: Uint8Array
  readonly rootCentralRecord: Uint8Array
  readonly closingTail: Uint8Array
  readonly observationDigest: Uint8Array
}

export interface DirectZipTargetVerificationPort {
  /** Fast acceptance is limited to bounded marker/length/observation evidence under target locks. */
  verifyPredecessor(
    checkpoint: DirectZipWriterCheckpointV1,
  ): Promise<DirectZipPredecessorVerificationV1>
  /** Opening must repeat the predecessor fence; a prior verification is not conditional filesystem authority. */
  openEpoch(checkpoint: DirectZipWriterCheckpointV1): Promise<DirectZipOpenEpochResultV1>
  observeCandidate(
    candidate: DirectZipEpochCandidateV1,
    closeAttempt?: DirectZipCloseAttemptV1,
  ): Promise<DirectZipCandidateObservationV1>
  digestRange(start: bigint, end: bigint): Promise<Uint8Array>
  /** The implementation rechecks ownership and predecessor proofs before its nonconditional truncate. */
  truncateToPredecessor(
    checkpoint: DirectZipWriterCheckpointV1,
    candidate: DirectZipEpochCandidateV1,
  ): Promise<DirectZipTruncateResultV1>
  readBoundedCompletionProof(input: Readonly<{
    exactArchiveBytes: bigint
    rootCentralRecordOffset: bigint
    rootCentralRecordBytes: bigint
    closingTailBytes: number
  }>): Promise<DirectZipBoundedCompletionReadV1>
}

export interface DirectZipWriterPageSink {
  stageLayout(admission: DirectZipMemberAdmissionV1): Promise<void>
  stageCentral(input: Readonly<{
    ordinal: bigint
    bytes: Uint8Array
  }>): Promise<void>
  snapshot(): Promise<DirectZipWriterPageStateV1>
  restore(state: DirectZipWriterPageStateV1): Promise<void>
  replayCentral(
    state: DirectZipWriterPageStateV1,
  ): AsyncIterable<Readonly<{ ordinal: bigint; bytes: Uint8Array }>>
  committedEpochProofs(
    checkpoint: DirectZipWriterCheckpointV1,
  ): AsyncIterable<DirectZipEpochProofV1>
}

export interface DirectZipWriterCutSink {
  stageCandidate(candidate: DirectZipEpochCandidateV1): Promise<void>
  promoteCandidate(input: Readonly<{
    candidate: DirectZipEpochCandidateV1
    checkpoint: DirectZipWriterCheckpointV1
    completion?: DirectZipCompletionProofV1
  }>): Promise<void>
  retireCandidate(input: Readonly<{
    candidate: DirectZipEpochCandidateV1
    disposition: 'replay-predecessor' | 'truncate-and-replay'
    checkpoint: DirectZipWriterCheckpointV1
  }>): Promise<void>
  enterClosing(input: Readonly<{
    predecessorGeneration: bigint
    checkpoint: DirectZipWriterCheckpointV1
  }>): Promise<void>
}

export interface DirectZipWriterIdentityPort {
  nextEpochId(): string
  nextCandidateId(): string
}

export type DirectZipWriterTraceEventV1 = Readonly<{
  kind:
    | 'epoch-opened'
    | 'member-admitted'
    | 'member-resumed'
    | 'checkpoint-policy-decided'
    | 'candidate-staged'
    | 'predecessor-verified'
    | 'epoch-close-observed'
    | 'candidate-resolved'
    | 'checkpoint-promoted'
    | 'closing-entered'
    | 'central-record-replayed'
    | 'completion-verified'
    | 'writer-gated'
    | 'writer-failed'
  operationId: string
  checkpointGeneration: bigint
  phase: DirectZipWriterCheckpointV1['phase']
  archiveOffset: bigint
  offsetClass?:
    | 'member-header'
    | 'member-payload'
    | 'member-descriptor'
    | 'central-directory'
    | 'closing-tail'
  epochId?: string
  candidateId?: string
  entryOrdinal?: bigint
  decision?: string
  error?: unknown
}>

export type DirectZipWriterObserver = (event: DirectZipWriterTraceEventV1) => void

export interface DirectZipWriterContextV1 {
  readonly ownershipMarker: DirectZipOwnershipMarkerV1
  readonly rootComponent: string
}
