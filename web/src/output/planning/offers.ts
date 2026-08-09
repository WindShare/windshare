import {
  createOriginalFileArtifact,
  createResultRootDirectoryTreeArtifact,
  createSingleFileDirectoryTreeArtifact,
  createZipArchiveArtifact,
} from '../../transfer/intent'
import type { ArtifactSpec } from '../../transfer/intent'
import type {
  ArtifactAction,
  ArtifactActionsOffer,
  ArtifactOffers,
  BrowserHandoffTargetOffer,
  DirectAtomicPlanOffer,
  EnvironmentOffers,
  FSADirectoryContainerOffer,
  ManagedAtomicTargetOffer,
  NativeDirectoryContainerOffer,
  NoSafeDestinationOffer,
  OfferedMaterializationPlan,
  OfferDisabledDecision,
  OfferUnavailableReason,
  PortableHandoffPlanOffer,
  PreparationRequirement,
  RecoverySemantics,
  SelectionProjectionV1,
  DiscoveryState,
  WorkspaceThenPublishPlanOffer,
} from './contracts'
import {
  assertEnvironmentOffers,
  outputLowerBoundFits,
} from './guarantees'
import {
  artifactRequestedName,
  resultRootLayoutFromProof,
} from './naming'

type CompletePlanOffer =
  | DirectAtomicPlanOffer
  | WorkspaceThenPublishPlanOffer
  | PortableHandoffPlanOffer

export async function offerArtifacts(
  projection: SelectionProjectionV1,
  discovery: DiscoveryState,
  environment: EnvironmentOffers,
): Promise<ArtifactOffers> {
  assertPlanningProjection(projection)
  assertEnvironmentOffers(environment)
  if (projection.proof.kind === 'unknown') {
    if (discovery.kind === 'complete') {
      throw new TypeError('complete discovery cannot retain unknown artifact proof')
    }
    return discovery.kind === 'retryable-failure'
      ? disabledProjectionOffer(projection, 'retry-confirmation', 'discovery-retry-required')
      : disabledProjectionOffer(projection, 'confirming-selected-content', 'shape-unsettled')
  }
  if (projection.proof.kind === 'none') {
    if (discovery.kind !== 'complete') {
      throw new TypeError('none proof requires complete discovery')
    }
    return disabledProjectionOffer(projection, 'selection-empty', 'selection-empty')
  }
  const actions = projection.proof.kind === 'single-file'
    ? await singleFileActions(projection, projection.proof, environment)
    : await treeActions(projection, projection.proof, environment)
  if (actions.length === 0) return noSafeDestination(projection, environment)
  return artifactActionsOffer(projection, actions)
}

async function singleFileActions(
  projection: SelectionProjectionV1,
  proof: Extract<SelectionProjectionV1['proof'], { kind: 'single-file' }>,
  environment: EnvironmentOffers,
): Promise<readonly ArtifactAction[]> {
  const file = proof.file
  const original = await createOriginalFileArtifact({
    fileId: file.fileId,
    sourcePath: file.sourcePath,
    suggestedName: file.portableName,
  })
  const directoryTree = await createSingleFileDirectoryTreeArtifact({
    fileId: file.fileId,
    sourcePath: file.sourcePath,
    outputName: file.portableName,
  })
  const completePlan = chooseCompletePlan(environment, projection.metrics.byteCountLowerBound)
  const actions: ArtifactAction[] = []
  if (completePlan !== null) {
    actions.push(actionForCompleteArtifact(projection.epoch, original, completePlan))
  }
  const directoryTarget = chooseDirectoryTarget(environment, projection.metrics.byteCountLowerBound)
  if (directoryTarget !== null) {
    actions.push(createAction({
      projectionEpoch: projection.epoch,
      operation: 'save-single-to-folder',
      artifact: directoryTree,
      plan: Object.freeze({ kind: 'direct-tree', target: directoryTarget }),
      recovery: 'checkpoint-resumable',
      preparation: noPreparation(),
    }))
  }
  return actions
}

async function treeActions(
  projection: SelectionProjectionV1,
  proof: Extract<SelectionProjectionV1['proof'], { kind: 'tree' }>,
  environment: EnvironmentOffers,
): Promise<readonly ArtifactAction[]> {
  const layout = resultRootLayoutFromProof(proof)
  const directoryTarget = chooseDirectoryTarget(environment, projection.metrics.byteCountLowerBound)
  const actions: ArtifactAction[] = []
  if (directoryTarget !== null) {
    const artifact = layout === null ? null : await createResultRootDirectoryTreeArtifact(layout)
    actions.push(createAction({
      projectionEpoch: projection.epoch,
      operation: 'save-directory-tree',
      artifact,
      unresolvedArtifactKind: 'directory-tree',
      plan: Object.freeze({ kind: 'direct-tree', target: directoryTarget }),
      recovery: 'checkpoint-resumable',
      preparation: noPreparation(),
    }))
  }
  if (layout !== null) {
    const completePlan = chooseCompletePlan(environment, projection.metrics.byteCountLowerBound)
    if (completePlan !== null) {
      const archive = await createZipArchiveArtifact(layout)
      actions.push(actionForCompleteArtifact(projection.epoch, archive, completePlan))
    }
  }
  return actions
}

function chooseCompletePlan(
  environment: EnvironmentOffers,
  byteCountLowerBound: bigint,
): DirectAtomicPlanOffer | WorkspaceThenPublishPlanOffer | PortableHandoffPlanOffer | null {
  // One displayed operation freezes delivery and recovery semantics. Selecting a
  // deterministic plan family here prevents later capability probes from changing them.
  const atomicTarget = chooseAtomicTarget(environment, byteCountLowerBound)
  if (atomicTarget !== null) return Object.freeze({ kind: 'direct-atomic', target: atomicTarget })
  const workspacePlan = chooseWorkspacePlan(environment, byteCountLowerBound)
  if (workspacePlan !== null) return workspacePlan
  return choosePortablePlan(environment, byteCountLowerBound)
}

function chooseDirectoryTarget(
  environment: EnvironmentOffers,
  byteCountLowerBound: bigint,
): NativeDirectoryContainerOffer | FSADirectoryContainerOffer | null {
  for (const target of environment.targets) {
    if ((target.kind === 'native-directory-container' || target.kind === 'fsa-parent-directory') &&
        outputLowerBoundFits(target.hardMaximumOutputBytes, byteCountLowerBound)) return target
  }
  return null
}

function chooseAtomicTarget(
  environment: EnvironmentOffers,
  byteCountLowerBound: bigint,
): ManagedAtomicTargetOffer | null {
  return environment.targets.find((target): target is ManagedAtomicTargetOffer =>
    target.kind === 'managed-atomic-file-target' &&
      outputLowerBoundFits(target.hardMaximumOutputBytes, byteCountLowerBound)) ?? null
}

function chooseWorkspacePlan(
  environment: EnvironmentOffers,
  byteCountLowerBound: bigint,
): WorkspaceThenPublishPlanOffer | null {
  const workspace = environment.workspace
  if (workspace === null || byteCountLowerBound > workspace.jobHardLimitBytes ||
      byteCountLowerBound > workspace.processHardLimitBytes) return null
  const publicationTarget = chooseAtomicTarget(environment, byteCountLowerBound) ??
    environment.targets.find((target): target is BrowserHandoffTargetOffer =>
      target.kind === 'browser-handoff' && target.supportsWorkspacePackage &&
      outputLowerBoundFits(target.hardMaximumOutputBytes, byteCountLowerBound)) ?? null
  return publicationTarget === null
    ? null
    : Object.freeze({ kind: 'workspace-then-publish', workspace, publicationTarget })
}

function choosePortablePlan(
  environment: EnvironmentOffers,
  byteCountLowerBound: bigint,
): PortableHandoffPlanOffer | null {
  const portable = environment.portable
  if (portable === null || byteCountLowerBound > portable.maximumArtifactBytes) return null
  const handoffTarget = environment.targets.find((target): target is BrowserHandoffTargetOffer =>
    target.kind === 'browser-handoff' && target.supportsPortableArtifact &&
    outputLowerBoundFits(target.hardMaximumOutputBytes, byteCountLowerBound)) ?? null
  return handoffTarget === null
    ? null
    : Object.freeze({ kind: 'portable-handoff', portable, handoffTarget })
}

function actionForCompleteArtifact(
  projectionEpoch: SelectionProjectionV1['epoch'],
  artifact: Exclude<ArtifactSpec, { kind: 'directory-tree' }>,
  plan: CompletePlanOffer,
): ArtifactAction {
  return createAction({
    projectionEpoch,
    operation: completeArtifactOperation(artifact, plan),
    artifact,
    plan,
    recovery: recoveryForCompletePlan(plan),
    preparation: preparationForCompletePlan(artifact, plan),
  })
}

function completeArtifactOperation(
  artifact: Exclude<ArtifactSpec, { kind: 'directory-tree' }>,
  plan: CompletePlanOffer,
): ArtifactAction['operation'] {
  if (plan.kind === 'portable-handoff') return 'check-then-download'
  return artifact.kind === 'original-file' ? 'download-original' : 'download-zip'
}

function recoveryForCompletePlan(
  plan: CompletePlanOffer,
): RecoverySemantics {
  switch (plan.kind) {
    case 'direct-atomic': return 'restart-required'
    case 'workspace-then-publish': return 'workspace-resumable'
    case 'portable-handoff': return 'none'
  }
}

function preparationForCompletePlan(
  artifact: Exclude<ArtifactSpec, { kind: 'directory-tree' }>,
  plan: CompletePlanOffer,
): PreparationRequirement {
  switch (plan.kind) {
    case 'direct-atomic':
      return noPreparation()
    case 'workspace-then-publish':
      return Object.freeze({
        manifest: artifact.kind === 'zip-archive' ? 'exact-zip' : 'none',
        hardAdmission: 'workspace-budget',
      })
    case 'portable-handoff':
      return Object.freeze({ manifest: 'exact-artifact', hardAdmission: 'portable-artifact' })
  }
}

function createAction(input: Readonly<{
  projectionEpoch: SelectionProjectionV1['epoch']
  operation: ArtifactAction['operation']
  artifact: ArtifactSpec | null
  unresolvedArtifactKind?: ArtifactSpec['kind']
  plan: OfferedMaterializationPlan
  recovery: RecoverySemantics
  preparation: PreparationRequirement
}>): ArtifactAction {
  const artifactKind = input.artifact?.kind ?? input.unresolvedArtifactKind
  if (artifactKind === undefined) throw new TypeError('offered action requires an artifact kind')
  return Object.freeze({
    kind: 'artifact-action',
    projectionEpoch: input.projectionEpoch,
    operation: input.operation,
    artifactKind,
    artifact: input.artifact,
    suggestedName: input.artifact === null ? null : artifactRequestedName(input.artifact),
    importance: 'secondary',
    recovery: input.recovery,
    preparation: input.preparation,
    plan: input.plan,
  })
}

function artifactActionsOffer(
  projection: SelectionProjectionV1,
  actions: readonly ArtifactAction[],
): ArtifactActionsOffer {
  const [first, ...rest] = actions
  if (first === undefined) throw new TypeError('artifact action list is empty')
  const primary = Object.freeze({ ...first, importance: 'primary' as const })
  const alternatives = Object.freeze(rest)
  const all = [primary, ...alternatives]
  return Object.freeze({
    kind: 'artifact-actions',
    interactive: true,
    projectionEpoch: projection.epoch,
    primary,
    alternatives,
    decision: Object.freeze({
      name: 'receive.offer.computed',
      projection_epoch: projection.epoch,
      shape_proof: projection.proof.kind,
      offered_artifact_kinds: Object.freeze(unique(all.map((action) => action.artifactKind))),
      offered_plan_kinds: Object.freeze(unique(all.map((action) => action.plan.kind))),
      primary_artifact_kind: primary.artifactKind,
    }),
  })
}

function disabledProjectionOffer(
  projection: SelectionProjectionV1,
  kind: 'confirming-selected-content' | 'retry-confirmation' | 'selection-empty',
  reason: 'shape-unsettled' | 'discovery-retry-required' | 'selection-empty',
): Extract<ArtifactOffers, { kind: typeof kind }> {
  return Object.freeze({
    kind,
    interactive: kind === 'retry-confirmation',
    projectionEpoch: projection.epoch,
    reason,
    decision: disabledDecision(projection, reason),
  }) as Extract<ArtifactOffers, { kind: typeof kind }>
}

function noSafeDestination(
  projection: SelectionProjectionV1,
  environment: EnvironmentOffers,
): NoSafeDestinationOffer {
  const reason = unavailableReason(projection, environment)
  const hardLimitClass = hardLimitClassForReason(reason)
  const decision = disabledDecision(projection, reason, hardLimitClass)
  return Object.freeze({
    kind: 'no-safe-destination',
    interactive: false,
    projectionEpoch: projection.epoch,
    reason,
    decision,
  })
}

function hardLimitClassForReason(
  reason: NoSafeDestinationOffer['reason'],
): OfferDisabledDecision['hard_limit_class'] {
  if (reason === 'portable-limit-exceeded') return 'portable-artifact'
  if (reason === 'workspace-limit-exceeded') return 'workspace-job'
  return undefined
}

function unavailableReason(
  projection: SelectionProjectionV1,
  environment: EnvironmentOffers,
): NoSafeDestinationOffer['reason'] {
  const lowerBound = projection.metrics.byteCountLowerBound
  if (environment.portable !== null && hasPortableHandoffTarget(environment) &&
      lowerBound > environment.portable.maximumArtifactBytes) {
    return 'portable-limit-exceeded'
  }
  if (environment.workspace !== null && hasWorkspacePublicationTarget(environment) &&
      (lowerBound > environment.workspace.jobHardLimitBytes ||
       lowerBound > environment.workspace.processHardLimitBytes)) {
    return 'workspace-limit-exceeded'
  }
  return 'no-safe-destination'
}

function hasPortableHandoffTarget(environment: EnvironmentOffers): boolean {
  return environment.targets.some((target) =>
    target.kind === 'browser-handoff' && target.supportsPortableArtifact)
}

function hasWorkspacePublicationTarget(environment: EnvironmentOffers): boolean {
  return environment.targets.some((target) =>
    target.kind === 'managed-atomic-file-target' ||
    (target.kind === 'browser-handoff' && target.supportsWorkspacePackage))
}

function disabledDecision(
  projection: SelectionProjectionV1,
  reason: OfferUnavailableReason,
  hardLimitClass?: OfferDisabledDecision['hard_limit_class'],
): OfferDisabledDecision {
  return Object.freeze({
    name: 'receive.offer.disabled',
    projection_epoch: projection.epoch,
    shape_proof: projection.proof.kind,
    offer_unavailable_reason: reason,
    ...(hardLimitClass === undefined ? {} : { hard_limit_class: hardLimitClass }),
  })
}

function noPreparation(): PreparationRequirement {
  return Object.freeze({ manifest: 'none', hardAdmission: 'none' })
}

function assertPlanningProjection(projection: SelectionProjectionV1): void {
  if (projection.version !== 1 || typeof projection.epoch !== 'bigint' || projection.epoch <= 0n ||
      projection.metrics.byteCountLowerBound < 0n ||
      !Number.isSafeInteger(projection.metrics.fileCountLowerBound) ||
      projection.metrics.fileCountLowerBound < 0 ||
      !Number.isSafeInteger(projection.metrics.directoryCountLowerBound) ||
      projection.metrics.directoryCountLowerBound < 0) {
    throw new TypeError('selection projection is not valid planning evidence')
  }
}

function unique<T>(values: readonly T[]): T[] {
  return [...new Set(values)]
}
