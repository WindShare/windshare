import {
  createDirectAtomicPlan,
  createDirectTreePlan,
  createDirectResumableZipPlan,
  createPortableHandoffPlan,
  createReceiveIntent,
  createWorkspaceThenPublishPlan,
} from '../../transfer/intent'
import type {
  DestinationReservation,
  FSAOwnedFileBinding,
  PortableBinding,
  ReceiveIntent,
  SelectionSpec,
  WorkspaceBinding,
} from '../../transfer/intent'
import type {
  EnvironmentTargetOffer,
  OfferedMaterializationRoute,
  ResolvedArtifactAction,
} from './contracts'
import {
  materializationPlanSemantics,
  sameGuaranteeFacts,
  sameMaterializationPlanSemantics,
} from './guarantees'

export type CandidateMaterializationBinding =
  | Readonly<{
      kind: 'destination-reservation'
      targetRouteId: string
      reservation: DestinationReservation
    }>
  | Readonly<{
      kind: 'workspace-binding'
      workspaceRouteId: string
      publicationTargetRouteId: string
      workspace: WorkspaceBinding
    }>
  | Readonly<{
      kind: 'portable-binding'
      portableRouteId: string
      handoffTargetRouteId: string
      portable: PortableBinding
    }>
  | Readonly<{
      kind: 'fsa-owned-file-binding'
      targetRouteId: string
      binding: FSAOwnedFileBinding
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

export interface BoundReceiveIntent {
  readonly intent: ReceiveIntent
  readonly decision: IntentFrozenDecision
}

export async function bindReceiveIntent(input: Readonly<{
  selection: SelectionSpec
  action: ResolvedArtifactAction
  candidate: CandidateMaterializationBinding
}>): Promise<BoundReceiveIntent> {
  assertAction(input.selection, input.action)
  const plan = await bindPlan(input.action, input.candidate)
  const intent = await createReceiveIntent({
    selection: input.selection,
    artifact: input.action.artifact,
    plan,
  })
  return Object.freeze({
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

function assertAction(selection: SelectionSpec, action: ResolvedArtifactAction): void {
  if (selection.digest !== action.selectionDigest) {
    throw new TypeError('selection does not match the resolved artifact action')
  }
  if (action.artifact.digest !== action.resolvedArtifactDigest ||
      action.artifact.kind !== action.choice.artifactKind) {
    throw new TypeError('resolved artifact evidence does not match the frozen choice')
  }
  if (!sameMaterializationPlanSemantics(
    action.choice.plan,
    materializationPlanSemantics(action.route),
  )) {
    throw new TypeError('resolved route does not represent the frozen plan semantics')
  }
}

async function bindPlan(
  action: ResolvedArtifactAction,
  candidate: CandidateMaterializationBinding,
): Promise<ReceiveIntent['plan']> {
  const route = action.route
  switch (route.kind) {
    case 'direct-tree':
      requireDestinationBinding(route, candidate)
      return createDirectTreePlan(action.artifact, candidate.reservation)
    case 'direct-atomic':
      requireDestinationBinding(route, candidate)
      return createDirectAtomicPlan(action.artifact, candidate.reservation)
    case 'workspace-then-publish':
      if (candidate.kind !== 'workspace-binding' ||
          candidate.workspaceRouteId !== route.workspace.routeId ||
          candidate.publicationTargetRouteId !== route.publicationTarget.routeId) {
        throw new TypeError('workspace binding does not match the resolved route identities')
      }
      return createWorkspaceThenPublishPlan(action.artifact, candidate.workspace)
    case 'portable-handoff':
      if (candidate.kind !== 'portable-binding' ||
          candidate.portableRouteId !== route.portable.routeId ||
          candidate.handoffTargetRouteId !== route.handoffTarget.routeId) {
        throw new TypeError('portable binding does not match the resolved route identities')
      }
      if (candidate.portable.maximumArtifactBytes !== route.portable.maximumArtifactBytes ||
          candidate.portable.assemblyPartBytes !== route.portable.assemblyPartBytes ||
          candidate.portable.maximumParts !== route.portable.maximumParts ||
          candidate.portable.objectUrlLeaseMilliseconds !== route.portable.objectUrlLeaseMilliseconds) {
        throw new TypeError('portable binding does not match the resolved hard policies')
      }
      return createPortableHandoffPlan(action.artifact, candidate.portable)
    case 'direct-resumable-zip':
      if (candidate.kind !== 'fsa-owned-file-binding' ||
          candidate.targetRouteId !== route.target.routeId ||
          candidate.binding.guarantees.profile !== route.target.legalProfile ||
          !sameGuaranteeFacts(candidate.binding.guarantees, route.target.guarantees) ||
          !sameDirectZipPolicyDigests(candidate.binding.policies, route.target.support.policies)) {
        throw new TypeError('direct ZIP binding does not match the reviewed route authority')
      }
      return createDirectResumableZipPlan(action.artifact, candidate.binding)
  }
}

function requireDestinationBinding(
  route: Extract<OfferedMaterializationRoute, { kind: 'direct-tree' | 'direct-atomic' }>,
  candidate: CandidateMaterializationBinding,
): asserts candidate is Extract<CandidateMaterializationBinding, { kind: 'destination-reservation' }> {
  if (candidate.kind !== 'destination-reservation' ||
      candidate.targetRouteId !== route.target.routeId ||
      !reservationMatchesTarget(candidate.reservation, route.target)) {
    throw new TypeError('destination binding does not match the resolved route guarantees')
  }
}

function reservationMatchesTarget(
  reservation: DestinationReservation,
  target: Exclude<EnvironmentTargetOffer, { kind: 'browser-handoff' | 'precreated-browser-file' }>,
): boolean {
  if (reservation.guarantees.profile !== target.legalProfile ||
      !sameGuaranteeFacts(reservation.guarantees, target.guarantees)) return false
  switch (target.kind) {
    case 'native-directory-container':
      return reservation.authorityKind === 'native-container'
    case 'fsa-parent-directory':
      return reservation.authorityKind === 'fsa-container'
    case 'managed-atomic-file-target':
      return reservation.authorityKind === 'managed-atomic-target'
    case 'fsa-owned-file-target':
      return false
  }
}

function sameDirectZipPolicyDigests(
  left: FSAOwnedFileBinding['policies'],
  right: FSAOwnedFileBinding['policies'],
): boolean {
  return left.zipEncoding === right.zipEncoding && left.layout === right.layout &&
    left.checkpoint === right.checkpoint && left.journalBudget === right.journalBudget &&
    left.epoch === right.epoch
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
