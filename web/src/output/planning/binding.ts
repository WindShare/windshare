import {
  createDirectAtomicPlan,
  createDirectTreePlan,
  createPortableHandoffPlan,
  createReceiveIntent,
  createWorkspaceThenPublishPlan,
} from '../../transfer/intent'
import type {
  DestinationReservation,
  PortableBinding,
  ReceiveIntent,
  WorkspaceBinding,
} from '../../transfer/intent'
import type {
  ArtifactAction,
  ArtifactActionsOffer,
  DiscoveryState,
  EnvironmentOffers,
  EnvironmentTargetOffer,
  OfferDisabledDecision,
  OfferedMaterializationPlan,
  SelectionProjectionV1,
} from './contracts'
import {
  sameGuaranteeFacts,
  sameTargetSemantics,
} from './guarantees'
import { offerArtifacts } from './offers'

export type AcquiredMaterializationAuthority =
  | Readonly<{
      kind: 'destination-reservation'
      environmentTargetOfferId: string
      reservation: DestinationReservation
    }>
  | Readonly<{
      kind: 'workspace-binding'
      workspaceOfferId: string
      workspace: WorkspaceBinding
    }>
  | Readonly<{
      kind: 'portable-binding'
      portableOfferId: string
      handoffTargetOfferId: string
      portable: PortableBinding
    }>

export interface IntentFrozenDecision {
  readonly name: 'receive.intent.frozen'
  readonly operation_id: string
  readonly receive_intent_digest: string
  readonly artifact_kind: ReceiveIntent['artifact']['kind']
  readonly layout_class:
    | 'original-file'
    | 'single-file'
    | 'complete-directory'
    | 'directory-selection'
    | 'synthetic-selection'
  readonly plan_kind: ReceiveIntent['plan']['kind']
}

export type BindMaterializationResult =
  | Readonly<{
      kind: 'bound'
      intent: ReceiveIntent
      decision: IntentFrozenDecision
    }>
  | Readonly<{
      kind: 'awaiting-layout'
      unavailableReason: 'shape-unsettled'
      decision: OfferDisabledDecision
    }>
  | Readonly<{
      kind: 'artifact-choice-required'
      unavailableReason: 'capability-changed'
      decision: OfferDisabledDecision
    }>

export async function bindMaterialization(input: Readonly<{
  selection: ReceiveIntent['selection']
  chosenAction: ArtifactAction
  currentProjection: SelectionProjectionV1
  currentDiscovery: DiscoveryState
  currentEnvironment: EnvironmentOffers
  acquired: AcquiredMaterializationAuthority
}>): Promise<BindMaterializationResult> {
  if (input.selection.digest !== input.currentProjection.selectionDigest ||
      input.chosenAction.projectionEpoch !== input.currentProjection.epoch) {
    return artifactChoiceRequired(input.currentProjection)
  }
  const currentOffers = await offerArtifacts(
    input.currentProjection,
    input.currentDiscovery,
    input.currentEnvironment,
  )
  if (currentOffers.kind !== 'artifact-actions') return artifactChoiceRequired(input.currentProjection)
  const currentAction = findCompatibleAction(input.chosenAction, currentOffers)
  if (currentAction === null) return artifactChoiceRequired(input.currentProjection)
  const artifact = currentAction.artifact
  if (artifact === null) {
    return Object.freeze({
      kind: 'awaiting-layout',
      unavailableReason: 'shape-unsettled',
      decision: disabledDecision(input.currentProjection, 'shape-unsettled'),
    })
  }
  const plan = await bindPlan(currentAction.plan, artifact, input.acquired)
  const intent = await createReceiveIntent({
    selection: input.selection,
    artifact,
    plan,
  })
  return Object.freeze({
    kind: 'bound',
    intent,
    decision: Object.freeze({
      name: 'receive.intent.frozen',
      operation_id: intent.operationId,
      receive_intent_digest: intent.digest,
      artifact_kind: intent.artifact.kind,
      layout_class: layoutClass(intent),
      plan_kind: intent.plan.kind,
    }),
  })
}

function findCompatibleAction(
  chosen: ArtifactAction,
  current: ArtifactActionsOffer,
): ArtifactAction | null {
  for (const candidate of [current.primary, ...current.alternatives]) {
    if (chosen.operation !== candidate.operation ||
        chosen.artifactKind !== candidate.artifactKind ||
        chosen.recovery !== candidate.recovery ||
        !samePreparation(chosen, candidate) ||
        !samePlanSemantics(chosen.plan, candidate.plan)) continue
    if (chosen.artifact !== null && chosen.artifact.digest !== candidate.artifact?.digest) continue
    return candidate
  }
  return null
}

async function bindPlan(
  offeredPlan: OfferedMaterializationPlan,
  artifact: NonNullable<ArtifactAction['artifact']>,
  acquired: AcquiredMaterializationAuthority,
): Promise<ReceiveIntent['plan']> {
  switch (offeredPlan.kind) {
    case 'direct-tree':
      requireDestinationAuthority(offeredPlan, acquired)
      return createDirectTreePlan(artifact, acquired.reservation)
    case 'direct-atomic':
      requireDestinationAuthority(offeredPlan, acquired)
      return createDirectAtomicPlan(artifact, acquired.reservation)
    case 'workspace-then-publish':
      if (acquired.kind !== 'workspace-binding' ||
          acquired.workspaceOfferId !== offeredPlan.workspace.id) {
        throw new TypeError('workspace authority does not match the current offered plan')
      }
      return createWorkspaceThenPublishPlan(artifact, acquired.workspace)
    case 'portable-handoff':
      if (acquired.kind !== 'portable-binding' ||
          acquired.portableOfferId !== offeredPlan.portable.id ||
          acquired.handoffTargetOfferId !== offeredPlan.handoffTarget.id) {
        throw new TypeError('portable authority does not match the current offered plan')
      }
      return createPortableHandoffPlan(artifact, acquired.portable)
  }
}

function requireDestinationAuthority(
  plan: Extract<OfferedMaterializationPlan, { kind: 'direct-tree' | 'direct-atomic' }>,
  acquired: AcquiredMaterializationAuthority,
): asserts acquired is Extract<AcquiredMaterializationAuthority, { kind: 'destination-reservation' }> {
  if (acquired.kind !== 'destination-reservation' ||
      acquired.environmentTargetOfferId !== plan.target.id ||
      !reservationMatchesTarget(acquired.reservation, plan.target)) {
    throw new TypeError('destination authority does not match the current offered guarantees')
  }
}

function reservationMatchesTarget(
  reservation: DestinationReservation,
  target: EnvironmentTargetOffer,
): boolean {
  if (target.legalProfile === null || reservation.guarantees.profile !== target.legalProfile ||
      !sameGuaranteeFacts(reservation.guarantees, target.guarantees)) return false
  switch (target.kind) {
    case 'native-directory-container':
      return reservation.authorityKind === 'native-container'
    case 'fsa-parent-directory':
      return reservation.authorityKind === 'fsa-container'
    case 'managed-atomic-file-target':
      return reservation.authorityKind === 'managed-atomic-target'
    case 'browser-handoff':
      return false
  }
}

function samePlanSemantics(
  left: OfferedMaterializationPlan,
  right: OfferedMaterializationPlan,
): boolean {
  if (left.kind !== right.kind) return false
  switch (left.kind) {
    case 'direct-tree':
      return right.kind === 'direct-tree' && sameTargetSemantics(left.target, right.target)
    case 'direct-atomic':
      return right.kind === 'direct-atomic' && sameTargetSemantics(left.target, right.target)
    case 'workspace-then-publish':
      return right.kind === 'workspace-then-publish' &&
        sameWorkspaceSemantics(left.workspace, right.workspace) &&
        sameTargetSemantics(left.publicationTarget, right.publicationTarget)
    case 'portable-handoff':
      return right.kind === 'portable-handoff' &&
        samePortableSemantics(left.portable, right.portable) &&
        sameTargetSemantics(left.handoffTarget, right.handoffTarget)
  }
}

function sameWorkspaceSemantics(
  left: Extract<OfferedMaterializationPlan, { kind: 'workspace-then-publish' }>['workspace'],
  right: Extract<OfferedMaterializationPlan, { kind: 'workspace-then-publish' }>['workspace'],
): boolean {
  // A quota estimate is an observation, not reserved capacity. Re-probing it must
  // not mutate the displayed plan; exact workspace admission remains mandatory.
  return left.kind === right.kind && left.persistence === right.persistence &&
    left.jobHardLimitBytes === right.jobHardLimitBytes &&
    left.processHardLimitBytes === right.processHardLimitBytes &&
    left.minimumQuotaReserveBytes === right.minimumQuotaReserveBytes
}

function samePortableSemantics(
  left: Extract<OfferedMaterializationPlan, { kind: 'portable-handoff' }>['portable'],
  right: Extract<OfferedMaterializationPlan, { kind: 'portable-handoff' }>['portable'],
): boolean {
  return left.kind === right.kind && left.persistence === right.persistence &&
    left.maximumArtifactBytes === right.maximumArtifactBytes &&
    left.assemblyPartBytes === right.assemblyPartBytes &&
    left.maximumParts === right.maximumParts &&
    left.objectUrlLeaseMilliseconds === right.objectUrlLeaseMilliseconds
}

function samePreparation(left: ArtifactAction, right: ArtifactAction): boolean {
  return left.preparation.manifest === right.preparation.manifest &&
    left.preparation.hardAdmission === right.preparation.hardAdmission
}

function artifactChoiceRequired(
  projection: SelectionProjectionV1,
): Extract<BindMaterializationResult, { kind: 'artifact-choice-required' }> {
  return Object.freeze({
    kind: 'artifact-choice-required',
    unavailableReason: 'capability-changed',
    decision: disabledDecision(projection, 'capability-changed'),
  })
}

function disabledDecision(
  projection: SelectionProjectionV1,
  reason: 'capability-changed' | 'shape-unsettled',
): OfferDisabledDecision {
  return Object.freeze({
    name: 'receive.offer.disabled',
    projection_epoch: projection.epoch,
    shape_proof: projection.proof.kind,
    offer_unavailable_reason: reason,
  })
}

function layoutClass(intent: ReceiveIntent): IntentFrozenDecision['layout_class'] {
  switch (intent.artifact.kind) {
    case 'original-file':
      return 'original-file'
    case 'zip-archive':
      return intent.artifact.layout.class
    case 'directory-tree':
      switch (intent.artifact.layout.kind) {
        case 'single-file': return 'single-file'
        case 'result-root': return intent.artifact.layout.root.class
        case 'catalog-root': throw new TypeError('browser planning cannot bind catalog-root layout')
      }
  }
}
