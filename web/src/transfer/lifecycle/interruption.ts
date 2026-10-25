import type { PlanKind, ReceiveLifecycleState } from './state'

export type ReceiveInterruption = 'pause' | 'stop'

const PAUSE = Object.freeze(['pause'] as const)
const STOP = Object.freeze(['stop'] as const)
const PAUSE_AND_STOP = Object.freeze(['pause', 'stop'] as const)
const NO_CONTROLS = Object.freeze([])

export function receiveInterruptionControls(plan: PlanKind): readonly ReceiveInterruption[] {
  switch (plan) {
    case 'direct-tree': return PAUSE_AND_STOP
    case 'direct-resumable-zip':
    case 'workspace-then-publish': return PAUSE
    case 'portable-handoff': return STOP
    case 'direct-atomic': return NO_CONTROLS
  }
}

export class TransferPauseRequestedError extends Error {
  constructor(message = 'Transfer paused by receiver', options?: ErrorOptions) {
    super(message, options)
    this.name = 'TransferPauseRequestedError'
  }
}

export class TransferStopRequestedError extends Error {
  constructor(message = 'Transfer stopped by receiver') {
    super(message)
    this.name = 'TransferStopRequestedError'
  }
}

export function receiveInterruptionReason(control: ReceiveInterruption): Error {
  return control === 'pause' ? new TransferPauseRequestedError() : new TransferStopRequestedError()
}

/** A stop releases abandoned work, never a delivered result or a complete result awaiting save. */
export type ReceiveRetention = 'continuation' | 'complete-results' | 'delivered-results' | 'browser-handoff' | 'none' | 'reconcile'

export type ReceiveStorageSettlement = Readonly<{
  task: 'continuable' | 'ended' | 'unsettled'
  retention: ReceiveRetention
  cleanup: 'settled' | 'retryable'
}>

/** Derived from the durable lifecycle cut; an abort signal alone cannot settle storage. */
export function receiveStorageSettlement(state: ReceiveLifecycleState): ReceiveStorageSettlement {
  switch (state.kind) {
    case 'resumable-start':
    case 'resumable-receive':
    case 'resumable-package':
    case 'authorization-required':
    case 'target-verification-required':
    case 'destination-space-required':
      return { task: 'continuable', retention: 'continuation', cleanup: 'settled' }
    case 'materialization-sealed':
    case 'artifact-sealed':
    case 'waiting-to-save':
      return { task: 'ended', retention: 'complete-results', cleanup: 'settled' }
    case 'download-started':
      return { task: 'ended', retention: state.attemptKind === 'workspace' ? 'complete-results' : 'browser-handoff', cleanup: 'settled' }
    case 'published':
      return { task: 'ended', retention: 'delivered-results', cleanup: state.cleanupState === 'clean' ? 'settled' : 'retryable' }
    case 'partial-directory':
      return { task: 'ended', retention: 'complete-results', cleanup: 'settled' }
    case 'restart-required':
      return state.reason === 'direct-atomic-rolled-back' || state.reason === 'portable-aborted' || state.reason === 'target-deleted'
        ? { task: 'ended', retention: 'none', cleanup: 'settled' }
        : { task: 'ended', retention: 'reconcile', cleanup: 'retryable' }
    case 'discarded':
      return { task: 'ended', retention: 'none', cleanup: 'settled' }
    case 'source-invalidated':
      return { task: 'ended', retention: 'reconcile', cleanup: 'retryable' }
    case 'needs-attention':
      return { task: 'unsettled', retention: 'reconcile', cleanup: 'retryable' }
    default:
      return { task: 'unsettled', retention: 'reconcile', cleanup: 'settled' }
  }
}

export function receiveLifecycleStorageEvidence(state: ReceiveLifecycleState): Readonly<{
  operationId: string
  lifecycleKind: ReceiveLifecycleState['kind']
  generation: bigint
  artifactId?: string
  checkpointId?: string
  checkpointGeneration?: bigint
  settlement: ReceiveStorageSettlement
}> {
  return Object.freeze({
    operationId: state.operationId, lifecycleKind: state.kind, generation: state.generation,
    settlement: receiveStorageSettlement(state),
    ...('packageDigest' in state ? { artifactId: state.packageDigest } : {}),
    ...('checkpointSetDigest' in state ? { checkpointId: state.checkpointSetDigest } : {}),
    ...(state.kind === 'resumable-receive' && state.payloadKind === 'workspace-archive'
      ? { artifactId: state.objectId, checkpointId: state.objectId, checkpointGeneration: state.checkpointGeneration } : {}),
    ...(state.kind === 'resumable-receive' && state.payloadKind === 'direct-zip'
      ? { checkpointId: state.directZipCheckpointDigest } : {}),
  })
}
