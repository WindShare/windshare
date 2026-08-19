export type PersistentOutputErrorKind =
  | 'authorization'
  | 'checkpoint-invalid'
  | 'collision'
  | 'incomplete-file'
  | 'output-state'
  | 'owned-object-unknown'
  | 'revision-conflict'
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

export type CheckpointBlockDecision =
  | 'revision-conflict'
  | 'ownership-conflict'
  | 'invalid'

export class CheckpointLineageDecisionError extends PersistentOutputError {
  readonly decision: CheckpointBlockDecision

  constructor(decision: CheckpointBlockDecision) {
    super(
      checkpointBlockKind(decision),
      'Authenticated local checkpoint evidence blocks this file lineage',
    )
    this.name = 'CheckpointLineageDecisionError'
    this.decision = decision
  }
}

function checkpointBlockKind(decision: CheckpointBlockDecision): PersistentOutputErrorKind {
  switch (decision) {
    case 'revision-conflict': return 'revision-conflict'
    case 'ownership-conflict': return 'owned-object-unknown'
    case 'invalid': return 'checkpoint-invalid'
  }
}

export class DestinationCollisionError extends PersistentOutputError {
  constructor(options?: ErrorOptions) {
    super('collision', 'An existing unowned destination blocks this file', options)
    this.name = 'DestinationCollisionError'
  }
}

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
