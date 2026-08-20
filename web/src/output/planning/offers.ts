import {
  createOriginalFileArtifact,
  createResultRootDirectoryTreeArtifact,
  createSingleFileDirectoryTreeArtifact,
  createZipArchiveArtifact,
} from '../../transfer/intent'
import type { ArtifactSpec } from '../../transfer/intent'
import type {
  ArtifactActionsOffer,
  ArtifactChoice,
  ArtifactOffers,
  BrowserHandoffTargetOffer,
  DirectAtomicMaterializationRoute,
  DiscoveryState,
  EnvironmentOffers,
  FSADirectoryContainerOffer,
  ManagedAtomicTargetOffer,
  NativeDirectoryContainerOffer,
  NoSafeDestinationOffer,
  OfferedArtifactChoice,
  OfferedMaterializationRoute,
  OfferDisabledDecision,
  OfferUnavailableReason,
  PortableHandoffMaterializationRoute,
  PreparationRequirement,
  RecoverySemantics,
  SelectionProjectionV1,
  WorkspaceThenPublishMaterializationRoute,
} from './contracts'
import {
  assertEnvironmentOffers,
  materializationPlanSemantics,
  outputLowerBoundFits,
} from './guarantees'
import {
  artifactRequestedName,
  resultRootLayoutFromProof,
} from './naming'

type CompleteMaterializationRoute =
  | DirectAtomicMaterializationRoute
  | WorkspaceThenPublishMaterializationRoute
  | PortableHandoffMaterializationRoute

interface PlanningArtifactCandidate {
  readonly choice: ArtifactChoice
  readonly route: OfferedMaterializationRoute
  readonly artifact: ArtifactSpec | null
  readonly suggestedName: string | null
}

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
  const candidates = await deriveArtifactCandidates(projection, environment)
  if (candidates.length === 0) return noSafeDestination(projection, environment)
  if (discovery.kind === 'retryable-failure' &&
      candidates.every((candidate) => candidate.artifact === null)) {
    return disabledProjectionOffer(projection, 'retry-confirmation', 'discovery-retry-required')
  }
  return artifactActionsOffer(projection, candidates)
}

async function deriveArtifactCandidates(
  projection: SelectionProjectionV1,
  environment: EnvironmentOffers,
): Promise<readonly PlanningArtifactCandidate[]> {
  switch (projection.proof.kind) {
    case 'single-file':
      return singleFileCandidates(projection, projection.proof, environment)
    case 'tree':
      return treeCandidates(projection, projection.proof, environment)
    case 'unknown':
    case 'none':
      return Object.freeze([])
  }
}

async function singleFileCandidates(
  projection: SelectionProjectionV1,
  proof: Extract<SelectionProjectionV1['proof'], { kind: 'single-file' }>,
  environment: EnvironmentOffers,
): Promise<readonly PlanningArtifactCandidate[]> {
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
  const completeRoute = chooseCompleteRoute(environment, projection.metrics.byteCountLowerBound)
  const candidates: PlanningArtifactCandidate[] = []
  if (completeRoute !== null) {
    candidates.push(candidateForCompleteArtifact(original, completeRoute))
  }
  const directoryTarget = chooseDirectoryTarget(environment, projection.metrics.byteCountLowerBound)
  if (directoryTarget !== null) {
    candidates.push(createCandidate({
      operation: 'save-single-to-folder',
      artifact: directoryTree,
      route: Object.freeze({ kind: 'direct-tree', target: directoryTarget }),
      recovery: 'checkpoint-resumable',
      preparation: noPreparation(),
    }))
  }
  return Object.freeze(candidates)
}

async function treeCandidates(
  projection: SelectionProjectionV1,
  proof: Extract<SelectionProjectionV1['proof'], { kind: 'tree' }>,
  environment: EnvironmentOffers,
): Promise<readonly PlanningArtifactCandidate[]> {
  const layout = resultRootLayoutFromProof(proof)
  const directoryTarget = chooseDirectoryTarget(environment, projection.metrics.byteCountLowerBound)
  const candidates: PlanningArtifactCandidate[] = []
  if (directoryTarget !== null) {
    const artifact = layout === null ? null : await createResultRootDirectoryTreeArtifact(layout)
    candidates.push(createCandidate({
      operation: 'save-directory-tree',
      artifact,
      unresolvedArtifactKind: 'directory-tree',
      route: Object.freeze({ kind: 'direct-tree', target: directoryTarget }),
      recovery: 'checkpoint-resumable',
      preparation: noPreparation(),
    }))
  }
  if (layout !== null) {
    const completeRoute = chooseCompleteRoute(environment, projection.metrics.byteCountLowerBound)
    if (completeRoute !== null) {
      candidates.push(candidateForCompleteArtifact(
        await createZipArchiveArtifact(layout),
        completeRoute,
      ))
    }
  }
  return Object.freeze(candidates)
}

function chooseCompleteRoute(
  environment: EnvironmentOffers,
  byteCountLowerBound: bigint,
): CompleteMaterializationRoute | null {
  // The priority affects presentation only. Reconciliation selects the already
  // chosen family directly, so a later higher-priority route cannot replace it.
  const atomicTarget = chooseAtomicTarget(environment, byteCountLowerBound)
  if (atomicTarget !== null) return Object.freeze({ kind: 'direct-atomic', target: atomicTarget })
  const workspaceRoute = chooseWorkspaceRoute(environment, byteCountLowerBound)
  if (workspaceRoute !== null) return workspaceRoute
  return choosePortableRoute(environment, byteCountLowerBound)
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

function chooseWorkspaceRoute(
  environment: EnvironmentOffers,
  byteCountLowerBound: bigint,
): WorkspaceThenPublishMaterializationRoute | null {
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

function choosePortableRoute(
  environment: EnvironmentOffers,
  byteCountLowerBound: bigint,
): PortableHandoffMaterializationRoute | null {
  const portable = environment.portable
  if (portable === null || byteCountLowerBound > portable.maximumArtifactBytes) return null
  const handoffTarget = environment.targets.find((target): target is BrowserHandoffTargetOffer =>
    target.kind === 'browser-handoff' && target.supportsPortableArtifact &&
    outputLowerBoundFits(target.hardMaximumOutputBytes, byteCountLowerBound)) ?? null
  return handoffTarget === null
    ? null
    : Object.freeze({ kind: 'portable-handoff', portable, handoffTarget })
}

function candidateForCompleteArtifact(
  artifact: Exclude<ArtifactSpec, { kind: 'directory-tree' }>,
  route: CompleteMaterializationRoute,
): PlanningArtifactCandidate {
  return createCandidate({
    operation: completeArtifactOperation(artifact, route),
    artifact,
    route,
    recovery: recoveryForCompleteRoute(route),
    preparation: preparationForCompleteRoute(artifact, route),
  })
}

function completeArtifactOperation(
  artifact: Exclude<ArtifactSpec, { kind: 'directory-tree' }>,
  route: CompleteMaterializationRoute,
): ArtifactChoice['operation'] {
  if (route.kind === 'portable-handoff') return 'check-then-download'
  return artifact.kind === 'original-file' ? 'download-original' : 'download-zip'
}

function recoveryForCompleteRoute(route: CompleteMaterializationRoute): RecoverySemantics {
  switch (route.kind) {
    case 'direct-atomic': return 'restart-required'
    case 'workspace-then-publish': return 'workspace-resumable'
    case 'portable-handoff': return 'none'
  }
}

function preparationForCompleteRoute(
  artifact: Exclude<ArtifactSpec, { kind: 'directory-tree' }>,
  route: CompleteMaterializationRoute,
): PreparationRequirement {
  switch (route.kind) {
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

function createCandidate(input: Readonly<{
  operation: ArtifactChoice['operation']
  artifact: ArtifactSpec | null
  unresolvedArtifactKind?: ArtifactSpec['kind']
  route: OfferedMaterializationRoute
  recovery: RecoverySemantics
  preparation: PreparationRequirement
}>): PlanningArtifactCandidate {
  const artifactKind = input.artifact?.kind ?? input.unresolvedArtifactKind
  if (artifactKind === undefined) throw new TypeError('offered choice requires an artifact kind')
  return Object.freeze({
    choice: Object.freeze({
      kind: 'artifact-choice',
      operation: input.operation,
      artifactKind,
      recovery: input.recovery,
      preparation: input.preparation,
      plan: materializationPlanSemantics(input.route),
    }),
    route: input.route,
    artifact: input.artifact,
    suggestedName: input.artifact === null ? null : artifactRequestedName(input.artifact),
  })
}

function artifactActionsOffer(
  projection: SelectionProjectionV1,
  candidates: readonly PlanningArtifactCandidate[],
): ArtifactActionsOffer {
  const [first, ...rest] = candidates
  if (first === undefined) throw new TypeError('artifact choice list is empty')
  const primary = offeredChoice(first, 'primary')
  const alternatives = Object.freeze(rest.map((candidate) => offeredChoice(candidate, 'secondary')))
  const all = [primary, ...alternatives]
  return Object.freeze({
    kind: 'artifact-actions',
    interactive: true,
    projectionEpoch: projection.epoch,
    selectionDigest: projection.selectionDigest,
    primary,
    alternatives,
    decision: Object.freeze({
      name: 'receive.offer.computed',
      projection_epoch: projection.epoch,
      shape_proof: projection.proof.kind,
      offered_artifact_kinds: Object.freeze(unique(all.map((offered) => offered.choice.artifactKind))),
      offered_plan_kinds: Object.freeze(unique(all.map((offered) => offered.choice.plan.kind))),
      primary_artifact_kind: primary.choice.artifactKind,
    }),
  })
}

function offeredChoice(
  candidate: PlanningArtifactCandidate,
  importance: OfferedArtifactChoice['importance'],
): OfferedArtifactChoice {
  return Object.freeze({
    kind: 'offered-artifact-choice',
    choice: candidate.choice,
    route: candidate.route,
    suggestedName: candidate.suggestedName,
    importance,
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
    selectionDigest: projection.selectionDigest,
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
  return Object.freeze({
    kind: 'no-safe-destination',
    interactive: false,
    projectionEpoch: projection.epoch,
    selectionDigest: projection.selectionDigest,
    reason,
    decision: disabledDecision(projection, reason, hardLimitClass),
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
