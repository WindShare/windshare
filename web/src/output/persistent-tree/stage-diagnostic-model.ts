import type { FileCheckpointV2 } from '../persistence/checkpoint'

export type PersistentOutputStage =
  | 'fsa.binding.parent-handle.verify'
  | 'fsa.root.permission.query'
  | 'fsa.root.permission.request'
  | 'fsa.root.entry.inspect'
  | 'fsa.root.entry.open'
  | 'fsa.root.entry.create'
  | 'fsa.root.handle.verify'
  | 'fsa.directory.entry.inspect'
  | 'fsa.directory.entry.open'
  | 'fsa.directory.entry.create'
  | 'fsa.directory.handle.verify'
  | 'fsa.file.entry.inspect'
  | 'fsa.file.entry.open'
  | 'fsa.file.entry.create'
  | 'fsa.file.handle.verify'
  | 'fsa.file.writer.create'
  | 'fsa.file.writer.write'
  | 'fsa.file.writer.close'
  | 'fsa.file.committed-bytes.read'
  | 'indexeddb.binding.operation.read'
  | 'indexeddb.binding.reservation.read'
  | 'indexeddb.binding.parent-handle.read'
  | 'indexeddb.root-handle.read'
  | 'indexeddb.root-handle.persist'
  | 'indexeddb.root-handle.committed-read'
  | 'indexeddb.directory-handle.read'
  | 'indexeddb.directory-handle.persist'
  | 'indexeddb.directory-handle.committed-read'
  | 'indexeddb.file-handle.read'
  | 'indexeddb.file-handle.persist'
  | 'indexeddb.file-handle.committed-read'
  | 'indexeddb.checkpoint.lineage-read'
  | 'indexeddb.checkpoint.candidate-install'
  | 'indexeddb.checkpoint.candidate-stage'
  | 'indexeddb.checkpoint.commit'
  | 'indexeddb.checkpoint.committed-read'

export type PersistentOutputStageTarget = 'binding' | 'root' | 'directory' | 'file'

export interface PersistentOutputStageCorrelation {
  readonly operationId: string
  readonly outputSessionId: string
  readonly target: PersistentOutputStageTarget
  readonly artifactId: string
  readonly artifactPath: readonly string[]
  readonly ownedObjectId?: string
  readonly checkpointRecordId?: string
  readonly checkpointGeneration?: bigint
}

export interface PersistentOutputRawException {
  /** Exact in-process identity is local-only; bounded scalar fields support deterministic export. */
  readonly raw: unknown
  readonly valueType: string
  readonly constructorName?: string
  readonly name?: string
  readonly message?: string
  readonly stack?: string
}

export type PersistentOutputObservedFact<Value> =
  | Readonly<{ readonly status: 'observed'; readonly value: Value }>
  | Readonly<{
      readonly status: 'unavailable'
      readonly exception: PersistentOutputRawException
    }>

export interface PersistentOutputWriterFacts {
  readonly state: 'not-created' | 'open' | 'closed' | 'close-failed'
  readonly closeFailure?: PersistentOutputRawException
}

export interface PersistentOutputFSAFacts {
  readonly entry: PersistentOutputObservedFact<'absent' | 'file' | 'directory' | 'other'>
  readonly committedBytes?: PersistentOutputObservedFact<bigint | 'absent' | 'not-file'>
  readonly permissions: Readonly<{
    readonly target: 'entry' | 'parent'
    readonly read: PersistentOutputObservedFact<PermissionState | 'unsupported'>
    readonly readwrite: PersistentOutputObservedFact<PermissionState | 'unsupported'>
  }>
  readonly persistedHandle: PersistentOutputObservedFact<
    'absent' | 'matches-entry' | 'mismatches-entry' | 'invalid'
  >
  readonly writer?: PersistentOutputWriterFacts
}

export interface PersistentOutputCheckpointRecordFact {
  readonly recordId: string
  readonly checkpointGeneration: bigint
  readonly commitState: FileCheckpointV2['commitState']
  readonly checksum: string
  readonly verifiedEnd: bigint
}

export interface PersistentOutputCheckpointFacts {
  readonly candidates: PersistentOutputObservedFact<readonly PersistentOutputCheckpointRecordFact[]>
  readonly committed: PersistentOutputObservedFact<readonly PersistentOutputCheckpointRecordFact[]>
}

export type PersistentOutputFactTruncation =
  | 'checkpoint-pages'
  | 'checkpoint-records'
  | 'string-bytes'
  | 'total-bytes'

export type PersistentOutputFailureFactProviderName = 'binding' | 'fsa' | 'checkpoint'

export interface PersistentOutputFailureObservation {
  readonly deadlineMilliseconds: number
  readonly timedOut: boolean
  readonly providerCount: number
  readonly completedProviderCount: number
  readonly activeFileEvidenceCount: number
  readonly checkpointPagesRead: number
  readonly checkpointRecordsRetained: number
  readonly retainedBytes: number
  readonly truncation: readonly PersistentOutputFactTruncation[]
  readonly unavailableProviders: readonly Readonly<{
    readonly provider: PersistentOutputFailureFactProviderName
    readonly reason: 'rejected' | 'timeout'
    readonly exception?: PersistentOutputRawException
  }>[]
}

export interface PersistentOutputFailureFacts {
  readonly fsa?: PersistentOutputFSAFacts
  readonly checkpoint?: PersistentOutputCheckpointFacts
  readonly probeFailures?: readonly PersistentOutputRawException[]
  readonly observation: PersistentOutputFailureObservation
}

export type PersistentOutputStageMilestone =
  | Readonly<{
      readonly sequence: number
      readonly transition: 'started' | 'completed'
      readonly stage: PersistentOutputStage
      readonly correlation: PersistentOutputStageCorrelation
    }>
  | Readonly<{
      readonly sequence: number
      readonly transition: 'failed'
      readonly stage: PersistentOutputStage
      readonly correlation: PersistentOutputStageCorrelation
      readonly exception: PersistentOutputRawException
      readonly facts: PersistentOutputFailureFacts
    }>

export type PersistentOutputStageFailureMilestone = Extract<
  PersistentOutputStageMilestone,
  { readonly transition: 'failed' }
>
export type PersistentOutputExceptionProjection = Omit<PersistentOutputRawException, 'raw'>

export interface PersistentOutputCheckpointRecordProjection {
  readonly recordId: string
  readonly checkpointGeneration: string
  readonly commitState: FileCheckpointV2['commitState']
  readonly checksum: string
  readonly verifiedEnd: string
}

export type ProjectedObservedFact<Value> =
  | Readonly<{ readonly status: 'observed'; readonly value: Value }>
  | Readonly<{
      readonly status: 'unavailable'
      readonly exception: PersistentOutputExceptionProjection
    }>

export interface PersistentOutputStageFailureProjectionV1 {
  readonly schemaVersion: 1
  readonly sequence: number
  readonly stage: PersistentOutputStage
  readonly correlation: Readonly<{
    readonly operationId: string
    readonly outputSessionId: string
    readonly target: PersistentOutputStageTarget
    readonly artifactId: string
    readonly artifactPath: readonly string[]
    readonly ownedObjectId?: string
    readonly checkpointRecordId?: string
    readonly checkpointGeneration?: string
  }>
  readonly exception: PersistentOutputExceptionProjection
  readonly facts: Readonly<{
    readonly fsa?: Readonly<{
      readonly entry: ProjectedObservedFact<'absent' | 'file' | 'directory' | 'other'>
      readonly committedBytes?: ProjectedObservedFact<string | 'absent' | 'not-file'>
      readonly permissions: Readonly<{
        readonly target: 'entry' | 'parent'
        readonly read: ProjectedObservedFact<PermissionState | 'unsupported'>
        readonly readwrite: ProjectedObservedFact<PermissionState | 'unsupported'>
      }>
      readonly persistedHandle: ProjectedObservedFact<
        'absent' | 'matches-entry' | 'mismatches-entry' | 'invalid'
      >
      readonly writer?: Readonly<{
        readonly state: PersistentOutputWriterFacts['state']
        readonly closeFailure?: PersistentOutputExceptionProjection
      }>
    }>
    readonly checkpoint?: Readonly<{
      readonly candidates: ProjectedObservedFact<
        readonly PersistentOutputCheckpointRecordProjection[]
      >
      readonly committed: ProjectedObservedFact<
        readonly PersistentOutputCheckpointRecordProjection[]
      >
    }>
    readonly probeFailures?: readonly PersistentOutputExceptionProjection[]
    readonly observation: Omit<PersistentOutputFailureObservation, 'unavailableProviders'> & Readonly<{
      readonly unavailableProviders: readonly Readonly<{
        readonly provider: PersistentOutputFailureFactProviderName
        readonly reason: 'rejected' | 'timeout'
        readonly exception?: PersistentOutputExceptionProjection
      }>[]
    }>
  }>
}

export interface PersistentOutputStageFailureRecord {
  readonly local: PersistentOutputStageFailureMilestone
  readonly projection: PersistentOutputStageFailureProjectionV1
}

export interface PersistentOutputStageDiagnostics {
  /** Supplied by the caller so diagnostic correlation never invents a session identity. */
  readonly outputSessionId: string
  readonly beforeStage?: (
    stage: PersistentOutputStage,
    correlation: PersistentOutputStageCorrelation,
  ) => void | Promise<void>
  readonly observe: (milestone: PersistentOutputStageMilestone) => void
}

export interface PersistentOutputFailureFactContext {
  readonly signal: AbortSignal
  exception(error: unknown): PersistentOutputRawException
  claimCheckpointPage(): boolean
  remainingCheckpointRecords(): number
  checkpointRecord(record: FileCheckpointV2): PersistentOutputCheckpointRecordFact | undefined
  markTruncated(reason: PersistentOutputFactTruncation): void
}

export type PersistentOutputFailureFactProvider =
  (context: PersistentOutputFailureFactContext) =>
    Promise<Omit<PersistentOutputFailureFacts, 'observation'>>
