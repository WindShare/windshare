export type PersistentOutputErrorKind =
  | 'authorization'
  | 'collision'
  | 'incomplete-file'
  | 'output-state'
  | 'source-revision-changed'
  | 'target-ownership-unknown'

export class PersistentOutputError extends Error {
  readonly kind: PersistentOutputErrorKind

  constructor(kind: PersistentOutputErrorKind, message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'PersistentOutputError'
    this.kind = kind
  }
}

export class TargetOwnershipUnknownError extends DOMException {
  readonly reason = 'target-ownership-unknown' as const
  readonly operationId: string | null
  readonly stage: TargetOwnershipStage

  constructor(
    stage: TargetOwnershipStage,
    operationId: string | null,
    options?: { readonly cause?: unknown },
  ) {
    super('The destination ownership observation is inconclusive', 'InvalidStateError')
    this.operationId = operationId
    this.stage = stage
    if (options?.cause !== undefined) Object.defineProperty(this, 'cause', { value: options.cause })
  }
}

export type TargetOwnershipStage =
  | 'parent-authority'
  | 'reservation'
  | 'namespace-create'
  | 'writer-open'
  | 'checkpoint'
  | 'commit'
  | 'settlement'
  | 'cleanup'

export class SourceRevisionChangedError extends PersistentOutputError {
  constructor(options?: ErrorOptions) {
    super(
      'source-revision-changed',
      'The opened source revision does not match the persisted file checkpoint',
      options,
    )
    this.name = 'SourceRevisionChangedError'
  }
}
