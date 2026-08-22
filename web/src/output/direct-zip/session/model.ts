import type { ArtifactChoiceID, ReceiveIntent } from '../../../transfer/intent'
import type { BoundReceiveIntent, MaterializationRouteIdentity } from '../../planning'
import type { ReceiveLifecycleState } from '../../workspace/state'
import type {
  DirectZipCheckpointTargetExpectation,
  DirectZipLifecycleDecision,
  DirectZipOwnedTargetBinding,
  DirectZipParentBinding,
  DirectZipReopenResult,
  DirectZipReservationCandidatePort,
  DirectZipTargetObservationV1,
} from '../target'

export type DirectZipSessionMilestone =
  | 'bootstrap-candidate-persisted'
  | 'bootstrap-prefix-verified'
  | 'intent-frozen'
  | 'bootstrap-committed'
  | 'parent-reauthorized'
  | 'target-observed'
  | 'target-slow-verified'
  | 'space-preflight'
  | 'execution-opened'
  | 'pause-committed'
  | 'operation-retained'
  | 'cleanup-verified'

export interface DirectZipSessionTraceEvent {
  readonly name: 'direct_zip.session.milestone'
  readonly operation_id: string
  readonly milestone: DirectZipSessionMilestone
  readonly outcome: string
  readonly lifecycle_decision?: DirectZipLifecycleDecision['kind']
  readonly lifecycle_generation?: bigint
}

export type DirectZipSessionTrace = (event: DirectZipSessionTraceEvent) => void

export interface DirectZipProvisionalOwnedEffectAuthority {
  readonly operationId: string
  readonly choiceId: ArtifactChoiceID
  readonly installedRoute: MaterializationRouteIdentity
  settleActivationFailure(reason: unknown): PromiseLike<unknown>
  detach(): void | PromiseLike<void>
}

export interface DirectZipBootstrapPersistencePort<ParentHandle, FileHandle, Runtime> {
  readonly operationId: string
  readonly operationIdBytes: Uint8Array
  readonly parentBinding: DirectZipParentBinding<ParentHandle>
  /** This port must not reject after its durable candidate cut has committed. */
  readonly reservations: DirectZipReservationCandidatePort<ParentHandle>
  /** Called by the target's candidate port before the first filesystem creation. */
  readonly provisionalAuthority: DirectZipProvisionalOwnedEffectAuthority
  commitBootstrap(input: Readonly<{
    readonly frozen: BoundReceiveIntent
    readonly binding: DirectZipOwnedTargetBinding<ParentHandle, FileHandle>
    readonly observation: DirectZipTargetObservationV1
  }>): Promise<Readonly<{
    readonly lifecycle: ReceiveLifecycleState
    readonly runtime: Runtime
  }>>
}

export interface DirectZipRecoveryLifecyclePort<FileHandle, Runtime> {
  readonly intent: ReceiveIntent
  readonly lifecycle: ReceiveLifecycleState
  gate(
    decision: DirectZipLifecycleDecision,
    evidence: Readonly<{ readonly additionalTemporaryBytesUpperBound?: bigint }>,
  ): Promise<ReceiveLifecycleState>
  /** Opens execution and commits recovery resume as one compensated activation cut. */
  activate(target: DirectZipReopenResult<FileHandle>): Promise<Readonly<{
    readonly lifecycle: ReceiveLifecycleState
    readonly runtime: Runtime
  }>>
  pause(runtime: Runtime, signal: AbortSignal): Promise<ReceiveLifecycleState>
  retain(runtime: Runtime): Promise<void>
  /** Closes writer authority while retaining target/checkpoint proof for cleanup. */
  prepareCleanup(runtime: Runtime): Promise<void>
  /** Retained expiry owns no live writer; this cut fences cleanup without opening execution. */
  prepareRetainedCleanup(signal: AbortSignal): Promise<void>
  deleteOwnedTarget(): Promise<void>
}

export interface DirectZipRecoveryTargetInput<ParentHandle, FileHandle> {
  readonly binding: DirectZipOwnedTargetBinding<ParentHandle, FileHandle>
  readonly currentParent: ParentHandle
  readonly predecessor: DirectZipCheckpointTargetExpectation
  readonly candidate?: import('../target').DirectZipCandidateTargetExpectation
}
