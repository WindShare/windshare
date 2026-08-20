import {
  createOriginalFileArtifact,
  createResultRootDirectoryTreeArtifact,
  createSingleFileDirectoryTreeArtifact,
  createZipArchiveArtifact,
} from '../../transfer/intent'
import type { ArtifactSpec } from '../../transfer/intent'
import type {
  ArtifactChoice,
  ArtifactResolutionObservation,
  DiscoveryState,
  EnvironmentOffers,
  MaterializationRouteIdentity,
  OfferedMaterializationPlanSemantics,
  OfferedMaterializationRoute,
  ResolvedArtifactAction,
  RetryableDiscoveryReason,
  SelectionProjectionV1,
} from './contracts'
import {
  assertEnvironmentOffers,
  materializationPlanSemantics,
  materializationRouteIdentity,
  outputLowerBoundFits,
  sameMaterializationPlanSemantics,
  sameMaterializationRouteIdentity,
  sameTargetSemantics,
} from './guarantees'
import { resultRootLayoutFromProof } from './naming'

export type ArtifactChoiceInvalidationReason =
  | 'selection-changed'
  | 'selection-empty'
  | 'artifact-shape-incompatible'
  | 'semantic-route-unavailable'
  | 'hard-limit-exceeded'

export type ArtifactChoiceReconcileOutcome =
  | Readonly<{
      kind: 'waiting'
      reason: 'shape-unsettled'
      observation: ArtifactResolutionObservation
    }>
  | Readonly<{
      kind: 'retry-required'
      reason: RetryableDiscoveryReason
      observation: ArtifactResolutionObservation
    }>
  | Readonly<{
      kind: 'resolved'
      action: ResolvedArtifactAction
      observation: ArtifactResolutionObservation
    }>
  | Readonly<{
      kind: 'invalidated'
      reason: ArtifactChoiceInvalidationReason
      observation: ArtifactResolutionObservation
    }>

export type ArtifactPlanningContractCode =
  | 'same-epoch-selection-digest-changed'
  | 'same-epoch-resolved-artifact-digest-changed'
  | 'complete-projection-left-choice-unresolved'

export class ArtifactPlanningContractError extends Error {
  readonly code: ArtifactPlanningContractCode

  constructor(code: ArtifactPlanningContractCode) {
    super(contractMessage(code))
    this.name = 'ArtifactPlanningContractError'
    this.code = code
  }
}

export async function reconcileArtifactChoice(input: Readonly<{
  choice: ArtifactChoice
  preferredRoute: MaterializationRouteIdentity
  expectedSelectionDigest: string
  projection: SelectionProjectionV1
  discovery: DiscoveryState
  environment: EnvironmentOffers
  previousObservation: ArtifactResolutionObservation | null
}>): Promise<ArtifactChoiceReconcileOutcome> {
  assertEnvironmentOffers(input.environment)
  const derivation = await deriveArtifact(input.choice, input.projection)
  const artifact = derivation.kind === 'resolved' ? derivation.artifact : null
  const observation = observeResolution(input.projection, artifact, input.previousObservation)

  if (input.projection.selectionDigest !== input.expectedSelectionDigest) {
    return invalidated('selection-changed', observation)
  }
  if (derivation.kind === 'selection-empty') return invalidated('selection-empty', observation)
  if (derivation.kind === 'incompatible') {
    return invalidated('artifact-shape-incompatible', observation)
  }
  if (derivation.kind === 'waiting') {
    if (input.discovery.kind === 'complete') {
      throw new ArtifactPlanningContractError('complete-projection-left-choice-unresolved')
    }
    if (input.discovery.kind === 'retryable-failure') {
      return Object.freeze({
        kind: 'retry-required',
        reason: input.discovery.reason,
        observation,
      })
    }
    return Object.freeze({ kind: 'waiting', reason: 'shape-unsettled', observation })
  }

  const semanticRoutes = routesWithSemantics(input.choice.plan, input.environment)
  if (semanticRoutes.length === 0) {
    return invalidated('semantic-route-unavailable', observation)
  }
  const eligibleRoutes = semanticRoutes.filter((route) =>
    routeFitsLowerBound(route, input.projection.metrics.byteCountLowerBound))
  if (eligibleRoutes.length === 0) return invalidated('hard-limit-exceeded', observation)
  const route = preferRoute(eligibleRoutes, input.preferredRoute)
  const action: ResolvedArtifactAction = Object.freeze({
    kind: 'resolved-artifact-action',
    projectionEpoch: input.projection.epoch,
    selectionDigest: input.projection.selectionDigest,
    resolvedArtifactDigest: derivation.artifact.digest,
    choice: input.choice,
    route,
    artifact: derivation.artifact,
  })
  return Object.freeze({ kind: 'resolved', action, observation })
}

type ArtifactDerivation =
  | Readonly<{ kind: 'waiting' }>
  | Readonly<{ kind: 'selection-empty' }>
  | Readonly<{ kind: 'incompatible' }>
  | Readonly<{ kind: 'resolved'; artifact: ArtifactSpec }>

async function deriveArtifact(
  choice: ArtifactChoice,
  projection: SelectionProjectionV1,
): Promise<ArtifactDerivation> {
  switch (projection.proof.kind) {
    case 'unknown':
      return Object.freeze({ kind: 'waiting' })
    case 'none':
      return Object.freeze({ kind: 'selection-empty' })
    case 'single-file':
      return deriveSingleFileArtifact(choice, projection.proof.file)
    case 'tree':
      return deriveTreeArtifact(choice, projection.proof)
  }
}

async function deriveSingleFileArtifact(
  choice: ArtifactChoice,
  file: Extract<SelectionProjectionV1['proof'], { kind: 'single-file' }>['file'],
): Promise<ArtifactDerivation> {
  if (choice.artifactKind === 'original-file' &&
      (choice.operation === 'download-original' || choice.operation === 'check-then-download')) {
    return Object.freeze({
      kind: 'resolved',
      artifact: await createOriginalFileArtifact({
        fileId: file.fileId,
        sourcePath: file.sourcePath,
        suggestedName: file.portableName,
      }),
    })
  }
  if (choice.artifactKind === 'directory-tree' && choice.operation === 'save-single-to-folder') {
    return Object.freeze({
      kind: 'resolved',
      artifact: await createSingleFileDirectoryTreeArtifact({
        fileId: file.fileId,
        sourcePath: file.sourcePath,
        outputName: file.portableName,
      }),
    })
  }
  return Object.freeze({ kind: 'incompatible' })
}

async function deriveTreeArtifact(
  choice: ArtifactChoice,
  proof: Extract<SelectionProjectionV1['proof'], { kind: 'tree' }>,
): Promise<ArtifactDerivation> {
  const directoryChoice = choice.artifactKind === 'directory-tree' &&
    choice.operation === 'save-directory-tree'
  const archiveChoice = choice.artifactKind === 'zip-archive' &&
    (choice.operation === 'download-zip' || choice.operation === 'check-then-download')
  if (!directoryChoice && !archiveChoice) return Object.freeze({ kind: 'incompatible' })
  const layout = resultRootLayoutFromProof(proof)
  if (layout === null) return Object.freeze({ kind: 'waiting' })
  return Object.freeze({
    kind: 'resolved',
    artifact: directoryChoice
      ? await createResultRootDirectoryTreeArtifact(layout)
      : await createZipArchiveArtifact(layout),
  })
}

function observeResolution(
  projection: SelectionProjectionV1,
  artifact: ArtifactSpec | null,
  previous: ArtifactResolutionObservation | null,
): ArtifactResolutionObservation {
  const observation = Object.freeze({
    projectionEpoch: projection.epoch,
    selectionDigest: projection.selectionDigest,
    resolvedArtifactDigest: artifact?.digest ?? null,
  })
  if (previous === null || previous.projectionEpoch !== observation.projectionEpoch) return observation
  if (previous.selectionDigest !== observation.selectionDigest) {
    throw new ArtifactPlanningContractError('same-epoch-selection-digest-changed')
  }
  if (previous.resolvedArtifactDigest !== null &&
      previous.resolvedArtifactDigest !== observation.resolvedArtifactDigest) {
    throw new ArtifactPlanningContractError('same-epoch-resolved-artifact-digest-changed')
  }
  return observation
}

function routesWithSemantics(
  semantics: OfferedMaterializationPlanSemantics,
  environment: EnvironmentOffers,
): readonly OfferedMaterializationRoute[] {
  switch (semantics.kind) {
    case 'direct-tree':
      return directTreeRoutes(semantics, environment)
    case 'direct-atomic':
      return directAtomicRoutes(semantics, environment)
    case 'workspace-then-publish':
      return workspaceRoutes(semantics, environment)
    case 'portable-handoff':
      return portableRoutes(semantics, environment)
  }
}

function directTreeRoutes(
  semantics: Extract<OfferedMaterializationPlanSemantics, { kind: 'direct-tree' }>,
  environment: EnvironmentOffers,
): readonly OfferedMaterializationRoute[] {
  return Object.freeze(environment.targets.flatMap((target) =>
    (target.kind === 'native-directory-container' || target.kind === 'fsa-parent-directory') &&
      sameTargetSemantics(semantics.target, target)
      ? [Object.freeze({ kind: 'direct-tree' as const, target })]
      : []))
}

function directAtomicRoutes(
  semantics: Extract<OfferedMaterializationPlanSemantics, { kind: 'direct-atomic' }>,
  environment: EnvironmentOffers,
): readonly OfferedMaterializationRoute[] {
  return Object.freeze(environment.targets.flatMap((target) =>
    target.kind === 'managed-atomic-file-target' && sameTargetSemantics(semantics.target, target)
      ? [Object.freeze({ kind: 'direct-atomic' as const, target })]
      : []))
}

function workspaceRoutes(
  semantics: Extract<OfferedMaterializationPlanSemantics, { kind: 'workspace-then-publish' }>,
  environment: EnvironmentOffers,
): readonly OfferedMaterializationRoute[] {
  const workspace = environment.workspace
  if (workspace === null) return Object.freeze([])
  return Object.freeze(environment.targets.flatMap((publicationTarget) => {
    if (publicationTarget.kind !== 'managed-atomic-file-target' &&
        publicationTarget.kind !== 'browser-handoff') return []
    const route = Object.freeze({
      kind: 'workspace-then-publish' as const,
      workspace,
      publicationTarget,
    })
    return sameMaterializationPlanSemantics(semantics, materializationPlanSemantics(route))
      ? [route]
      : []
  }))
}

function portableRoutes(
  semantics: Extract<OfferedMaterializationPlanSemantics, { kind: 'portable-handoff' }>,
  environment: EnvironmentOffers,
): readonly OfferedMaterializationRoute[] {
  const portable = environment.portable
  if (portable === null) return Object.freeze([])
  return Object.freeze(environment.targets.flatMap((handoffTarget) => {
    if (handoffTarget.kind !== 'browser-handoff') return []
    const route = Object.freeze({ kind: 'portable-handoff' as const, portable, handoffTarget })
    return sameMaterializationPlanSemantics(semantics, materializationPlanSemantics(route))
      ? [route]
      : []
  }))
}

function routeFitsLowerBound(route: OfferedMaterializationRoute, lowerBound: bigint): boolean {
  switch (route.kind) {
    case 'direct-tree':
    case 'direct-atomic':
      return outputLowerBoundFits(route.target.hardMaximumOutputBytes, lowerBound)
    case 'workspace-then-publish':
      return lowerBound <= route.workspace.jobHardLimitBytes &&
        lowerBound <= route.workspace.processHardLimitBytes &&
        outputLowerBoundFits(route.publicationTarget.hardMaximumOutputBytes, lowerBound)
    case 'portable-handoff':
      return lowerBound <= route.portable.maximumArtifactBytes &&
        outputLowerBoundFits(route.handoffTarget.hardMaximumOutputBytes, lowerBound)
  }
}

function preferRoute(
  routes: readonly OfferedMaterializationRoute[],
  preferred: MaterializationRouteIdentity,
): OfferedMaterializationRoute {
  return routes.find((route) =>
    sameMaterializationRouteIdentity(materializationRouteIdentity(route), preferred)) ?? routes[0]!
}

function invalidated(
  reason: ArtifactChoiceInvalidationReason,
  observation: ArtifactResolutionObservation,
): Extract<ArtifactChoiceReconcileOutcome, { kind: 'invalidated' }> {
  return Object.freeze({ kind: 'invalidated', reason, observation })
}

function contractMessage(code: ArtifactPlanningContractCode): string {
  switch (code) {
    case 'same-epoch-selection-digest-changed':
      return 'selection digest changed within one projection epoch'
    case 'same-epoch-resolved-artifact-digest-changed':
      return 'resolved artifact digest changed within one projection epoch'
    case 'complete-projection-left-choice-unresolved':
      return 'complete projection left the artifact choice unresolved'
  }
}
