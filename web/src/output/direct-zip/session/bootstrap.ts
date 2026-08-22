import { encodeBase64Url } from '../../../crypto/bytes'
import { createFSAOwnedFileBinding } from '../../../transfer/intent'
import type { ArtifactChoiceID, AvailableDirectZipPolicyDigests } from '../../../transfer/intent'
import type {
  BoundReceiveIntent,
  CandidateMaterializationBinding,
  MaterializationRouteIdentity,
  ResolvedArtifactAction,
} from '../../planning'
import type { ReceiveLifecycleState } from '../../workspace/state'
import type {
  DirectZipOperationLeaseEvidence,
  DirectZipReservationCandidate,
  DirectZipReservationCandidateDraft,
  DirectZipReservationCandidatePort,
  DirectZipReservationRetirementReason,
  DirectZipTargetPort,
} from '../target'
import type {
  DirectZipBootstrapPersistencePort,
  DirectZipProvisionalOwnedEffectAuthority,
  DirectZipSessionTrace,
} from './model'

export interface DirectZipFreshBootstrapInput<ParentHandle, FileHandle, Runtime> {
  readonly action: ResolvedArtifactAction
  readonly preClickRanking: readonly ArtifactChoiceID[]
  readonly currentParent: ParentHandle
  readonly policies: AvailableDirectZipPolicyDigests
  readonly targetRouteId: string
  readonly persistence: DirectZipBootstrapPersistencePort<ParentHandle, FileHandle, Runtime>
  readonly createTarget: (
    reservations: DirectZipReservationCandidatePort<ParentHandle>,
  ) => DirectZipTargetPort<ParentHandle, FileHandle>
  readonly registerProvisionalOwnedEffects: (
    authority: DirectZipProvisionalOwnedEffectAuthority,
  ) => void
  readonly freezeAtFence: (candidate: CandidateMaterializationBinding) => Promise<BoundReceiveIntent>
  readonly trustedAction: boolean
  readonly trace?: DirectZipSessionTrace
}

export type DirectZipFreshBootstrapResult<Runtime> =
  | Readonly<{
      readonly kind: 'bound'
      readonly frozen: BoundReceiveIntent
      readonly lifecycle: ReceiveLifecycleState
      readonly runtime: Runtime
    }>
  | Readonly<{
      readonly kind: 'retryable-precut'
      readonly operationId: string
      readonly decision: import('../target').DirectZipLifecycleDecision
    }>
  | Readonly<{
      readonly kind: 'owned-effects'
      readonly operationId: string
      readonly decision: import('../target').DirectZipLifecycleDecision
      readonly authority: DirectZipProvisionalOwnedEffectAuthority
    }>

/**
 * Candidate persistence is injected into the target so the provisional owner is
 * registered immediately after the durable row and before exact-name creation.
 */
export async function activateFreshDirectZipTarget<ParentHandle, FileHandle, Runtime>(
  input: DirectZipFreshBootstrapInput<ParentHandle, FileHandle, Runtime>,
): Promise<DirectZipFreshBootstrapResult<Runtime>> {
  requireFreshInput(input)
  let provisionalRegistered = false
  const reservations: DirectZipReservationCandidatePort<ParentHandle> = Object.freeze({
    persistCandidate: async (
      draft: DirectZipReservationCandidateDraft<ParentHandle>,
      lease: DirectZipOperationLeaseEvidence,
    ) => {
      const persisted = await input.persistence.reservations.persistCandidate(draft, lease)
      if (!provisionalRegistered) {
        input.registerProvisionalOwnedEffects(input.persistence.provisionalAuthority)
        provisionalRegistered = true
        emit(input, 'bootstrap-candidate-persisted', 'registered')
      }
      return persisted
    },
    retireCandidate: (
      candidate: DirectZipReservationCandidate<ParentHandle>,
      reason: DirectZipReservationRetirementReason,
      lease: DirectZipOperationLeaseEvidence,
    ) =>
      input.persistence.reservations.retireCandidate(candidate, reason, lease),
  })
  const result = await input.createTarget(reservations).reserveBootstrap({
    operationId: input.persistence.operationIdBytes,
    resultRootComponent: requireRootComponent(input.action),
    parentBinding: input.persistence.parentBinding,
    currentParent: input.currentParent,
    trustedAction: input.trustedAction,
  })
  if (result.kind === 'gated') {
    return provisionalRegistered || result.retainedEffect !== undefined
      ? Object.freeze({
          kind: 'owned-effects',
          operationId: input.persistence.operationId,
          decision: result.decision,
          authority: input.persistence.provisionalAuthority,
        })
      : Object.freeze({
          kind: 'retryable-precut',
          operationId: input.persistence.operationId,
          decision: result.decision,
        })
  }
  emit(input, 'bootstrap-prefix-verified', result.value.recoveredExistingPrefix
    ? 'recovered-existing-prefix'
    : 'created')
  const binding = await createFSAOwnedFileBinding({
    operationId: input.persistence.operationId,
    artifact: input.action.artifact,
    stableName: result.value.binding.stableName,
    targetRef: encodeBase64Url(result.value.binding.targetRef),
    policies: input.policies,
  })
  const frozen = await input.freezeAtFence(Object.freeze({
    kind: 'fsa-owned-file-binding',
    targetRouteId: input.targetRouteId,
    binding,
  }))
  requireFrozenOperation(frozen, input)
  emit(input, 'intent-frozen', 'committing')
  const committed = await input.persistence.commitBootstrap({
    frozen,
    binding: result.value.binding,
    observation: result.value.observation,
  })
  emit(input, 'bootstrap-committed', 'ready', committed.lifecycle.generation)
  return Object.freeze({ kind: 'bound', frozen, ...committed })
}

function requireFreshInput(input: DirectZipFreshBootstrapInput<unknown, unknown, unknown>): void {
  if (input.action.route.kind !== 'direct-resumable-zip' ||
      input.action.artifact.kind !== 'zip-archive' ||
      input.action.route.target.routeId !== input.targetRouteId ||
      input.persistence.provisionalAuthority.operationId !== input.persistence.operationId ||
      input.persistence.provisionalAuthority.choiceId !== input.action.choiceId ||
      !sameRoute(input.persistence.provisionalAuthority.installedRoute, {
        kind: 'direct',
        targetRouteId: input.targetRouteId,
      }) || input.preClickRanking.length === 0 ||
      !input.preClickRanking.includes(input.action.choiceId) ||
      new Set(input.preClickRanking).size !== input.preClickRanking.length) {
    throw new TypeError('fresh Direct ZIP activation is not bound to the frozen pre-click route')
  }
}

function requireRootComponent(action: ResolvedArtifactAction): string {
  if (action.artifact.kind !== 'zip-archive') {
    throw new TypeError('Direct ZIP activation requires a ZIP artifact')
  }
  return action.artifact.layout.name
}

function requireFrozenOperation(
  frozen: BoundReceiveIntent,
  input: DirectZipFreshBootstrapInput<unknown, unknown, unknown>,
): void {
  if (frozen.intent.operationId !== input.persistence.operationId ||
      frozen.intent.plan.kind !== 'direct-resumable-zip' ||
      frozen.intent.artifact.digest !== input.action.artifact.digest ||
      frozen.intent.plan.binding.operationId !== input.persistence.operationId ||
      frozen.intent.plan.binding.policies.zipEncoding !== input.policies.zipEncoding ||
      frozen.intent.plan.binding.policies.layout !== input.policies.layout ||
      frozen.intent.plan.binding.policies.checkpoint !== input.policies.checkpoint ||
      frozen.intent.plan.binding.policies.journalBudget !== input.policies.journalBudget ||
      frozen.intent.plan.binding.policies.epoch !== input.policies.epoch) {
    throw new TypeError('frozen Direct ZIP intent does not own the bootstrap target')
  }
}

function sameRoute(left: MaterializationRouteIdentity, right: MaterializationRouteIdentity): boolean {
  return left.kind === 'direct' && right.kind === 'direct' &&
    left.targetRouteId === right.targetRouteId
}

function emit(
  input: Pick<DirectZipFreshBootstrapInput<unknown, unknown, unknown>, 'persistence' | 'trace'>,
  milestone: Parameters<NonNullable<DirectZipSessionTrace>>[0]['milestone'],
  outcome: string,
  generation?: bigint,
): void {
  input.trace?.(Object.freeze({
    name: 'direct_zip.session.milestone',
    operation_id: input.persistence.operationId,
    milestone,
    outcome,
    ...(generation === undefined ? {} : { lifecycle_generation: generation }),
  }))
}
