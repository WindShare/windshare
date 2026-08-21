import {
  MATERIALIZATION_PLAN_DOMAIN,
  RECEIVE_INTENT_DOMAIN,
  canonicalDigestValue,
  canonicalRecord,
  canonicalValue,
  digestText,
  frame,
  requireCanonicalValueProvenance,
  requireSameCanonicalValue,
  requireSameDigestRecord,
} from './canonical'
import {
  validateDestinationReservation,
  validatePortableBinding,
  validateWorkspaceBinding,
} from './destination'
import {
  MATERIALIZATION_PLAN_VERSION,
  RECEIVE_INTENT_VERSION,
  type ArtifactSpec,
  type CanonicalBytes,
  type DestinationReservation,
  type DirectAtomicPlan,
  type DirectTreePlan,
  type MaterializationPlan,
  type PortableBinding,
  type PortableHandoffPlan,
  type ReceiveIntent,
  type SelectionSpec,
  type WorkspaceBinding,
  type WorkspaceThenPublishPlan,
} from './model'
import { validateArtifactSpec, validateSelectionSpec } from './selection'

export async function createDirectTreePlan(
  artifactInput: ArtifactSpec,
  reservationInput: DestinationReservation,
): Promise<DirectTreePlan> {
  const artifact = await validateArtifactSpec(artifactInput)
  const reservation = await validateDestinationReservation(reservationInput, artifact)
  if (artifact.kind !== 'directory-tree') throw new TypeError('direct-tree requires a directory-tree artifact')
  switch (artifact.layout.kind) {
    case 'catalog-root':
      if (reservation.kind !== 'container-root') {
        throw new TypeError('catalog-root direct tree requires a container-root reservation')
      }
      break
    case 'single-file':
      if (reservation.kind !== 'named-container-entry' || reservation.entryKind !== 'single-file') {
        throw new TypeError('single-file direct tree requires a matching named reservation')
      }
      break
    case 'result-root':
      if (reservation.kind !== 'named-container-entry' || reservation.entryKind !== 'result-root') {
        throw new TypeError('result-root direct tree requires a matching named reservation')
      }
      break
  }
  const canonicalBytes = canonicalRecord(MATERIALIZATION_PLAN_DOMAIN, [
    Uint8Array.of(1),
    frame(reservation.canonicalBytes),
    frame(Uint8Array.of(0)),
  ])
  return canonicalValue({
    version: MATERIALIZATION_PLAN_VERSION,
    kind: 'direct-tree' as const,
    reservation,
    preparation: 'none' as const,
  }, canonicalBytes)
}

export async function createDirectAtomicPlan(
  artifactInput: ArtifactSpec,
  reservationInput: DestinationReservation,
): Promise<DirectAtomicPlan> {
  const artifact = await validateArtifactSpec(artifactInput)
  const reservation = await validateDestinationReservation(reservationInput, artifact)
  if (artifact.kind === 'directory-tree' || reservation.kind !== 'atomic-target') {
    throw new TypeError('direct-atomic requires a complete artifact and atomic reservation')
  }
  const canonicalBytes = canonicalRecord(MATERIALIZATION_PLAN_DOMAIN, [
    Uint8Array.of(2),
    frame(reservation.canonicalBytes),
    frame(Uint8Array.of(0)),
  ])
  return canonicalValue({
    version: MATERIALIZATION_PLAN_VERSION,
    kind: 'direct-atomic' as const,
    reservation,
    preparation: 'none' as const,
  }, canonicalBytes)
}

export async function createWorkspaceThenPublishPlan(
  artifactInput: ArtifactSpec,
  workspaceInput: WorkspaceBinding,
): Promise<WorkspaceThenPublishPlan> {
  const artifact = await validateArtifactSpec(artifactInput)
  const workspace = await validateWorkspaceBinding(workspaceInput, artifact)
  const preparation = artifact.kind === 'zip-archive' ? 'exact-zip' : 'none'
  const canonicalBytes = canonicalRecord(MATERIALIZATION_PLAN_DOMAIN, [
    Uint8Array.of(3),
    frame(workspace.canonicalBytes),
    frame(Uint8Array.of(preparation === 'exact-zip' ? 1 : 0)),
  ])
  return canonicalValue({
    version: MATERIALIZATION_PLAN_VERSION,
    kind: 'workspace-then-publish' as const,
    workspace,
    preparation,
  }, canonicalBytes)
}

export async function createPortableHandoffPlan(
  artifactInput: ArtifactSpec,
  portableInput: PortableBinding,
): Promise<PortableHandoffPlan> {
  const artifact = await validateArtifactSpec(artifactInput)
  const portable = await validatePortableBinding(portableInput, artifact)
  const canonicalBytes = canonicalRecord(MATERIALIZATION_PLAN_DOMAIN, [
    Uint8Array.of(4),
    frame(portable.canonicalBytes),
    frame(Uint8Array.of(2)),
    frame(Uint8Array.of(2)),
  ])
  return canonicalValue({
    version: MATERIALIZATION_PLAN_VERSION,
    kind: 'portable-handoff' as const,
    portable,
    publicationRoute: 'browser-handoff' as const,
    preparation: 'exact-artifact' as const,
  }, canonicalBytes)
}

export async function validateMaterializationPlan(
  input: MaterializationPlan,
  artifact: ArtifactSpec,
): Promise<MaterializationPlan> {
  if (input.version !== MATERIALIZATION_PLAN_VERSION) {
    throw new TypeError('materialization plan version is invalid')
  }
  let rebuilt: MaterializationPlan
  switch (input.kind) {
    case 'direct-tree':
      if (input.preparation !== 'none') throw new TypeError('direct-tree preparation is invalid')
      rebuilt = await createDirectTreePlan(artifact, input.reservation)
      break
    case 'direct-atomic':
      if (input.preparation !== 'none') throw new TypeError('direct-atomic preparation is invalid')
      rebuilt = await createDirectAtomicPlan(artifact, input.reservation)
      break
    case 'workspace-then-publish':
      rebuilt = await createWorkspaceThenPublishPlan(artifact, input.workspace)
      if (input.preparation !== rebuilt.preparation) {
        throw new TypeError('workspace preparation policy is invalid')
      }
      break
    case 'portable-handoff':
      if (input.publicationRoute !== 'browser-handoff' || input.preparation !== 'exact-artifact') {
        throw new TypeError('portable handoff policy is invalid')
      }
      rebuilt = await createPortableHandoffPlan(artifact, input.portable)
      break
    default:
      throw new TypeError('materialization plan kind is invalid')
  }
  return requireSameCanonicalValue(input, rebuilt, 'materialization plan')
}

export function materializationPlanOperationID(plan: MaterializationPlan): string {
  switch (plan.kind) {
    case 'direct-tree':
    case 'direct-atomic':
      return plan.reservation.operationId
    case 'workspace-then-publish':
      return plan.workspace.operationId
    case 'portable-handoff':
      return plan.portable.operationId
  }
}

export function materializationPlanArtifactDigest(plan: MaterializationPlan): string {
  switch (plan.kind) {
    case 'direct-tree':
    case 'direct-atomic':
      return plan.reservation.artifactDigest
    case 'workspace-then-publish':
      return plan.workspace.artifactDigest
    case 'portable-handoff':
      return plan.portable.artifactDigest
  }
}

export function materializationPlanBindingDigest(plan: MaterializationPlan): string {
  switch (plan.kind) {
    case 'direct-tree':
    case 'direct-atomic':
      return plan.reservation.digest
    case 'workspace-then-publish':
      return plan.workspace.digest
    case 'portable-handoff':
      return plan.portable.digest
  }
}

export async function createReceiveIntent(input: {
  readonly selection: SelectionSpec
  readonly artifact: ArtifactSpec
  readonly plan: MaterializationPlan
}): Promise<ReceiveIntent> {
  const selection = await validateSelectionSpec(input.selection)
  const artifact = await validateArtifactSpec(input.artifact)
  const plan = await validateMaterializationPlan(input.plan, artifact)
  if (materializationPlanArtifactDigest(plan) !== artifact.digest) {
    throw new TypeError('materialization plan does not bind the receive artifact')
  }
  const operationId = materializationPlanOperationID(plan)
  const bindingDigest = materializationPlanBindingDigest(plan)
  const canonicalBytes = canonicalReceiveIntentBytes({ selection, artifact, plan })
  return canonicalDigestValue({
    version: RECEIVE_INTENT_VERSION,
    selection,
    artifact,
    plan,
    shareInstance: selection.shareInstance,
    syntheticRoot: selection.syntheticRoot,
    operationId,
    bindingDigest,
  }, await digestText(canonicalBytes), canonicalBytes)
}

export function canonicalReceiveIntentBytes(input: {
  readonly selection: SelectionSpec
  readonly artifact: ArtifactSpec
  readonly plan: MaterializationPlan
}): CanonicalBytes {
  requireCanonicalValueProvenance(input.selection, 'selection spec')
  requireCanonicalValueProvenance(input.artifact, 'artifact spec')
  requireCanonicalValueProvenance(input.plan, 'materialization plan')
  if (materializationPlanArtifactDigest(input.plan) !== input.artifact.digest) {
    throw new TypeError('materialization plan artifact binding is invalid')
  }
  return canonicalRecord(RECEIVE_INTENT_DOMAIN, [
    frame(input.selection.canonicalBytes),
    frame(input.artifact.canonicalBytes),
    frame(input.plan.canonicalBytes),
  ], RECEIVE_INTENT_VERSION)
}

export async function validateReceiveIntent(input: ReceiveIntent): Promise<ReceiveIntent> {
  if (input.version !== RECEIVE_INTENT_VERSION) throw new TypeError('receive intent version is invalid')
  const rebuilt = await createReceiveIntent(input)
  if (input.shareInstance !== rebuilt.shareInstance ||
      input.syntheticRoot !== rebuilt.syntheticRoot ||
      input.operationId !== rebuilt.operationId ||
      input.bindingDigest !== rebuilt.bindingDigest) {
    throw new TypeError('receive intent derived authority fields are invalid')
  }
  return requireSameDigestRecord(input, rebuilt, 'receive intent')
}

export async function receiveIntentDigest(input: ReceiveIntent): Promise<string> {
  return (await validateReceiveIntent(input)).digest
}
