import {
  createFailureCorrelation,
  type FailureCorrelation,
} from '../../diagnostics/incident'
import type { SelectionSpec } from '../intent'
import {
  MAX_PROJECTION_GENERATION_REFERENCES,
  MAX_PROJECTION_SELECTED_ROOT_FACTS,
  SELECTION_PROJECTION_VERSION,
  SelectionProjectionError,
  compareGenerationReferences,
  nextProjectionEpoch,
  requireAuthenticatedProjectionEvidence,
  selectionTargetKey,
  snapshotFileFact,
  snapshotLayoutBasis,
  snapshotMetrics,
  snapshotSelectedRoot,
  unsettledTargetsForSelection,
  type ArtifactShapeProof,
  type AuthenticatedGenerationReference,
  type AuthenticatedProjectionEvidence,
  type DiscoveryState,
  type LayoutBasisProof,
  type ProjectedFileFact,
  type ProjectionEpoch,
  type ProjectionMetrics,
  type ProjectionTraceEvent,
  type ProjectionTraceSource,
  type SelectedRootFact,
  type SelectionProjectionEvent,
  type SelectionProjectionState,
  type SelectionProjectionV1,
  type SettledLayoutBasisProof,
  type UnsettledSelectionTarget,
} from './model'

const MAXIMUM_PROJECTION_BYTES = (1n << 64n) - 1n
const IDLE_DISCOVERY: DiscoveryState = Object.freeze({ kind: 'idle' })
const DISCOVERING: DiscoveryState = Object.freeze({ kind: 'discovering' })
const COMPLETE_DISCOVERY: DiscoveryState = Object.freeze({ kind: 'complete' })
const UNKNOWN_PROOF: ArtifactShapeProof = Object.freeze({ kind: 'unknown' })
const UNSETTLED_LAYOUT: LayoutBasisProof = Object.freeze({ kind: 'unsettled' })

interface GenerationMerge {
  readonly generations: readonly AuthenticatedGenerationReference[]
  readonly duplicate: boolean
}

export function createSelectionProjectionState(
  selection: SelectionSpec,
  epoch: ProjectionEpoch,
): SelectionProjectionState {
  if (epoch <= 0n) throw new SelectionProjectionError('projection epoch must be non-zero')
  return Object.freeze({
    projection: Object.freeze({
      version: SELECTION_PROJECTION_VERSION,
      epoch,
      selectionDigest: selection.digest,
      selectedRoots: Object.freeze([]),
      selectedRootCountLowerBound: 0,
      selectedRootsTruncated: false,
      generations: Object.freeze([]),
      metrics: Object.freeze({
        fileCountLowerBound: 0,
        directoryCountLowerBound: 0,
        byteCountLowerBound: 0n,
      }),
      unsettledTargets: unsettledTargetsForSelection(selection),
      proof: UNKNOWN_PROOF,
    }),
    discovery: IDLE_DISCOVERY,
  })
}

export function reduceSelectionProjection(
  state: SelectionProjectionState,
  event: SelectionProjectionEvent,
): SelectionProjectionState {
  if (event.epoch !== state.projection.epoch) return state
  switch (event.kind) {
    case 'discovery-started': return startDiscovery(state)
    case 'authenticated-evidence': return reduceAuthenticatedEvidence(state, event.evidence)
    case 'retryable-failure': return failRetryably(state, event.reason)
    case 'retry-started': return retryDiscovery(state)
    case 'discovery-completed': return completeDiscovery(
      state,
      event.settledTargets ?? [],
      event.layoutBasis,
    )
  }
}

export class SelectionProjectionController {
  readonly #trace: ProjectionTraceSource | undefined
  #lastEpoch = 0n
  #state: SelectionProjectionState | undefined

  constructor(trace?: ProjectionTraceSource) {
    this.#trace = trace
  }

  get state(): SelectionProjectionState {
    if (this.#state === undefined) {
      throw new SelectionProjectionError('selection projection has not started')
    }
    return this.#state
  }

  /** A caller must begin again for every selection mutation, even if rules look equivalent. */
  beginSelection(
    selection: SelectionSpec,
    correlation?: FailureCorrelation,
  ): SelectionProjectionState {
    const epoch = nextProjectionEpoch(this.#lastEpoch)
    this.#lastEpoch = epoch
    this.#state = createSelectionProjectionState(selection, epoch)
    this.#emit(() => Object.freeze({
      name: 'projection_transition',
      transition: 'started',
      projectionEpoch: epoch,
      ...(correlation === undefined
        ? {}
        : { correlation: createFailureCorrelation(correlation) }),
    }))
    return this.#state
  }

  apply(event: SelectionProjectionEvent): SelectionProjectionState {
    const before = this.state
    if (event.epoch !== before.projection.epoch) {
      return this.dropStaleAsyncEvent(
        event.epoch,
        event.kind === 'authenticated-evidence' ? 'catalog-evidence' : 'discovery-result',
      )
    }
    const after = reduceSelectionProjection(before, event)
    this.#state = after
    this.#traceTransition(before, after, event)
    return after
  }

  /**
   * Discovery may notice an epoch change before it can construct a reducer event.
   * Recording that fence here keeps the state no-op and the stale drop observable.
   */
  dropStaleAsyncEvent(
    staleEpoch: ProjectionEpoch,
    eventClass: 'catalog-evidence' | 'discovery-result',
  ): SelectionProjectionState {
    const current = this.state
    if (staleEpoch === current.projection.epoch) {
      throw new SelectionProjectionError('current-epoch events must pass through the reducer')
    }
    this.#emit(() => Object.freeze({
      name: 'projection_transition',
      transition: 'stale_event_dropped',
      currentProjectionEpoch: current.projection.epoch,
      staleProjectionEpoch: staleEpoch,
      eventClass: eventClass === 'catalog-evidence' ? 'catalog_evidence' : 'discovery_result',
    }))
    return current
  }

  #traceTransition(
    before: SelectionProjectionState,
    after: SelectionProjectionState,
    event: SelectionProjectionEvent,
  ): void {
    if (after === before) return
    if (event.kind === 'retryable-failure') {
      this.#emit(() => Object.freeze({
        name: 'projection_transition',
        transition: 'retryable_failure',
        projectionEpoch: after.projection.epoch,
        shapeProof: after.projection.proof.kind,
        retryableDiscoveryReason: event.reason,
      }))
      return
    }
    if (event.kind === 'retry-started') {
      this.#emit(() => Object.freeze({
        name: 'projection_transition',
        transition: 'retry_started',
        projectionEpoch: after.projection.epoch,
        retainedShapeProof: after.projection.proof.kind,
      }))
      return
    }
    this.#emit(() => refinedTrace(after))
    if (before.projection.proof.kind === 'unknown' && after.projection.proof.kind !== 'unknown') {
      this.#emit(() => provenTrace(after))
    }
  }

  #emit(build: () => ProjectionTraceEvent): void {
    try {
      const observer = this.#trace?.current
      if (observer === undefined) return
      observer(build())
    } catch {
      // Projection tracing is passive and cannot alter selection authority.
    }
  }
}

function startDiscovery(state: SelectionProjectionState): SelectionProjectionState {
  if (state.discovery.kind === 'discovering') return state
  if (state.discovery.kind !== 'idle') {
    throw new SelectionProjectionError('discovery can start only from idle state')
  }
  return withDiscovery(state, DISCOVERING)
}

function failRetryably(
  state: SelectionProjectionState,
  reason: Extract<DiscoveryState, { kind: 'retryable-failure' }>['reason'],
): SelectionProjectionState {
  if (state.discovery.kind !== 'discovering') {
    throw new SelectionProjectionError('retryable discovery failure requires active discovery')
  }
  return withDiscovery(state, Object.freeze({ kind: 'retryable-failure', reason }))
}

function retryDiscovery(state: SelectionProjectionState): SelectionProjectionState {
  if (state.discovery.kind !== 'retryable-failure') {
    throw new SelectionProjectionError('discovery retry requires a retryable failure')
  }
  return withDiscovery(state, DISCOVERING)
}

function reduceAuthenticatedEvidence(
  state: SelectionProjectionState,
  evidence: AuthenticatedProjectionEvidence,
): SelectionProjectionState {
  if (state.discovery.kind !== 'discovering') {
    throw new SelectionProjectionError('authenticated evidence requires active discovery')
  }
  requireAuthenticatedProjectionEvidence(evidence)
  const generationMerge = mergeGenerationReferences(state.projection.generations, evidence.generations)
  if (generationMerge.duplicate) return state
  if (state.projection.proof.kind === 'none' || state.projection.proof.kind === 'single-file') {
    throw new SelectionProjectionError('authenticated evidence cannot follow a final scalar proof')
  }

  const metrics = addMetrics(state.projection.metrics, evidence.metrics)
  const unsettledTargets = settleTargets(state.projection.unsettledTargets, evidence.settledTargets)
  const selectedRootCountLowerBound = addCount(
    state.projection.selectedRootCountLowerBound,
    evidence.selectedRootCount,
    'selected root lower bound',
  )
  const selectedRoots = retainSelectedRoots(state.projection.selectedRoots, evidence.selectedRoots)
  const selectedRootsTruncated = state.projection.selectedRootsTruncated ||
    selectedRootCountLowerBound > selectedRoots.length
  const singleFileCandidate = selectSingleFileCandidate(state.projection, evidence, metrics)
  const proof = refineProof({
    previous: state.projection.proof,
    metrics,
    selectedRoots,
    selectedRootCountLowerBound,
    selectedRootsTruncated,
    unsettledTargets,
    ...(singleFileCandidate === undefined ? {} : { singleFileCandidate }),
    ...(evidence.earlyLayoutBasis === undefined
      ? {}
      : { earlyLayoutBasis: evidence.earlyLayoutBasis }),
  })
  return Object.freeze({
    projection: Object.freeze({
      ...state.projection,
      selectedRoots,
      selectedRootCountLowerBound,
      selectedRootsTruncated,
      generations: generationMerge.generations,
      metrics,
      unsettledTargets,
      ...(singleFileCandidate === undefined ? {} : { singleFileCandidate }),
      proof,
    }),
    discovery: state.discovery,
  })
}

function completeDiscovery(
  state: SelectionProjectionState,
  settledTargets: readonly UnsettledSelectionTarget[],
  layoutBasis: SettledLayoutBasisProof | undefined,
): SelectionProjectionState {
  if (state.discovery.kind !== 'discovering') {
    throw new SelectionProjectionError('discovery completion requires active discovery')
  }
  const remainingTargets = settleTargets(state.projection.unsettledTargets, settledTargets)
  if (remainingTargets.length !== 0) {
    throw new SelectionProjectionError('discovery completed with unsettled selection targets')
  }
  const proof = completeProof(state.projection, layoutBasis)
  return Object.freeze({
    projection: Object.freeze({
      ...state.projection,
      unsettledTargets: remainingTargets,
      proof,
    }),
    discovery: COMPLETE_DISCOVERY,
  })
}

function completeProof(
  projection: SelectionProjectionV1,
  layoutBasis: SettledLayoutBasisProof | undefined,
): ArtifactShapeProof {
  if (projection.proof.kind === 'none' || projection.proof.kind === 'single-file') {
    if (layoutBasis !== undefined) {
      throw new SelectionProjectionError('scalar shape proof cannot carry a tree layout basis')
    }
    return projection.proof
  }
  if (projection.proof.kind === 'tree') {
    const basis = mergeLayoutBasis(projection.proof.layoutBasis, layoutBasis)
    if (basis.kind === 'unsettled') {
      throw new SelectionProjectionError('completed tree discovery requires a settled layout basis')
    }
    return treeProof(projection, basis)
  }
  if (projection.metrics.directoryCountLowerBound === 0 &&
      projection.metrics.fileCountLowerBound === 0) {
    if (layoutBasis !== undefined) {
      throw new SelectionProjectionError('empty selection cannot carry a tree layout basis')
    }
    return Object.freeze({ kind: 'none' })
  }
  if (projection.metrics.directoryCountLowerBound === 0 &&
      projection.metrics.fileCountLowerBound === 1) {
    if (layoutBasis !== undefined || projection.singleFileCandidate === undefined) {
      throw new SelectionProjectionError('single-file completion has inconsistent evidence')
    }
    return Object.freeze({ kind: 'single-file', file: projection.singleFileCandidate })
  }
  if (layoutBasis === undefined) {
    throw new SelectionProjectionError('tree completion requires a settled layout basis')
  }
  return treeProof(projection, snapshotLayoutBasis(layoutBasis))
}

function refineProof(input: {
  readonly previous: ArtifactShapeProof
  readonly metrics: ProjectionMetrics
  readonly selectedRoots: readonly SelectedRootFact[]
  readonly selectedRootCountLowerBound: number
  readonly selectedRootsTruncated: boolean
  readonly unsettledTargets: readonly UnsettledSelectionTarget[]
  readonly singleFileCandidate?: ProjectedFileFact
  readonly earlyLayoutBasis?: Readonly<{ kind: 'synthetic-selection' }>
}): ArtifactShapeProof {
  if (input.previous.kind === 'tree') {
    const layoutBasis = mergeLayoutBasis(input.previous.layoutBasis, input.earlyLayoutBasis)
    return Object.freeze({
      kind: 'tree',
      selectedRoots: input.selectedRoots,
      selectedRootCountLowerBound: input.selectedRootCountLowerBound,
      selectedRootsTruncated: input.selectedRootsTruncated,
      layoutBasis,
    })
  }
  const treeForced = input.metrics.directoryCountLowerBound > 0 ||
    input.metrics.fileCountLowerBound > 1 ||
    input.selectedRootCountLowerBound > 1 ||
    input.earlyLayoutBasis !== undefined
  if (treeForced) {
    return Object.freeze({
      kind: 'tree',
      selectedRoots: input.selectedRoots,
      selectedRootCountLowerBound: input.selectedRootCountLowerBound,
      selectedRootsTruncated: input.selectedRootsTruncated,
      layoutBasis: input.earlyLayoutBasis ?? UNSETTLED_LAYOUT,
    })
  }
  if (input.metrics.fileCountLowerBound === 1 &&
      input.unsettledTargets.length === 0 &&
      input.singleFileCandidate !== undefined) {
    return Object.freeze({ kind: 'single-file', file: input.singleFileCandidate })
  }
  return UNKNOWN_PROOF
}

function treeProof(
  projection: SelectionProjectionV1,
  layoutBasis: LayoutBasisProof,
): ArtifactShapeProof {
  return Object.freeze({
    kind: 'tree',
    selectedRoots: projection.selectedRoots,
    selectedRootCountLowerBound: projection.selectedRootCountLowerBound,
    selectedRootsTruncated: projection.selectedRootsTruncated,
    layoutBasis,
  })
}

function mergeLayoutBasis(
  current: LayoutBasisProof,
  next: LayoutBasisProof | undefined,
): LayoutBasisProof {
  if (next === undefined) return current
  const snapshot = snapshotLayoutBasis(next)
  if (current.kind === 'unsettled') return snapshot
  if (!sameLayoutBasis(current, snapshot)) {
    throw new SelectionProjectionError('settled tree layout basis cannot change within one epoch')
  }
  return current
}

function sameLayoutBasis(left: LayoutBasisProof, right: LayoutBasisProof): boolean {
  if (left.kind !== right.kind) return false
  if (left.kind === 'unsettled' || left.kind === 'synthetic-selection') return true
  if (right.kind === 'unsettled' || right.kind === 'synthetic-selection') return false
  return left.anchor.directoryId === right.anchor.directoryId &&
    left.anchor.sourcePath === right.anchor.sourcePath
}

function selectSingleFileCandidate(
  projection: SelectionProjectionV1,
  evidence: AuthenticatedProjectionEvidence,
  metrics: ProjectionMetrics,
): ProjectedFileFact | undefined {
  if (projection.singleFileCandidate !== undefined) return projection.singleFileCandidate
  if (projection.metrics.fileCountLowerBound === 0 &&
      evidence.metrics.fileCountLowerBound === 1 &&
      metrics.fileCountLowerBound === 1) {
    if (evidence.representativeFile === undefined) {
      throw new SelectionProjectionError('single-file lower bound lacks its file fact')
    }
    return snapshotFileFact(evidence.representativeFile)
  }
  return undefined
}

function retainSelectedRoots(
  current: readonly SelectedRootFact[],
  additions: readonly SelectedRootFact[],
): readonly SelectedRootFact[] {
  const remaining = MAX_PROJECTION_SELECTED_ROOT_FACTS - current.length
  if (remaining <= 0 || additions.length === 0) return current
  return Object.freeze([
    ...current,
    ...additions.slice(0, remaining).map(snapshotSelectedRoot),
  ])
}

function settleTargets(
  unsettled: readonly UnsettledSelectionTarget[],
  settled: readonly UnsettledSelectionTarget[],
): readonly UnsettledSelectionTarget[] {
  if (settled.length === 0) return unsettled
  const keys = new Set(unsettled.map(selectionTargetKey))
  for (const target of settled) {
    const key = selectionTargetKey(target)
    if (!keys.delete(key)) {
      throw new SelectionProjectionError('authenticated evidence settles an unknown target')
    }
  }
  return Object.freeze(unsettled.filter((target) => keys.has(selectionTargetKey(target))))
}

function addMetrics(left: ProjectionMetrics, right: ProjectionMetrics): ProjectionMetrics {
  const fileCountLowerBound = addCount(
    left.fileCountLowerBound,
    right.fileCountLowerBound,
    'file lower bound',
  )
  const directoryCountLowerBound = addCount(
    left.directoryCountLowerBound,
    right.directoryCountLowerBound,
    'directory lower bound',
  )
  const byteCountLowerBound = left.byteCountLowerBound + right.byteCountLowerBound
  if (byteCountLowerBound > MAXIMUM_PROJECTION_BYTES) {
    throw new SelectionProjectionError('projection byte lower bound overflowed')
  }
  return snapshotMetrics({ fileCountLowerBound, directoryCountLowerBound, byteCountLowerBound })
}

function addCount(left: number, right: number, label: string): number {
  const sum = left + right
  if (!Number.isSafeInteger(sum)) {
    throw new SelectionProjectionError(`${label} overflowed exact integer representation`)
  }
  return sum
}

function mergeGenerationReferences(
  current: readonly AuthenticatedGenerationReference[],
  additions: readonly AuthenticatedGenerationReference[],
): GenerationMerge {
  const byDirectory = new Map(current.map((reference) => [reference.directoryId, reference.generation]))
  let duplicateCount = 0
  for (const addition of additions) {
    const generation = byDirectory.get(addition.directoryId)
    if (generation === undefined) continue
    if (generation !== addition.generation) {
      throw new SelectionProjectionError('one projection epoch observed two directory generations')
    }
    duplicateCount += 1
  }
  if (duplicateCount !== 0 && duplicateCount !== additions.length) {
    throw new SelectionProjectionError('authenticated evidence mixes replayed and new generations')
  }
  if (duplicateCount === additions.length) return Object.freeze({ generations: current, duplicate: true })
  if (current.length + additions.length > MAX_PROJECTION_GENERATION_REFERENCES) {
    throw new SelectionProjectionError('projection generation-reference bound is exhausted')
  }
  const generations = [...current, ...additions]
    .sort(compareGenerationReferences)
  return Object.freeze({ generations: Object.freeze(generations), duplicate: false })
}

function withDiscovery(
  state: SelectionProjectionState,
  discovery: DiscoveryState,
): SelectionProjectionState {
  return Object.freeze({ projection: state.projection, discovery })
}

function refinedTrace(state: SelectionProjectionState): ProjectionTraceEvent {
  return Object.freeze({
    name: 'projection_transition',
    transition: 'refined',
    projectionEpoch: state.projection.epoch,
    shapeProof: state.projection.proof.kind,
    discoveryState: state.discovery.kind,
    fileCountLowerBound: state.projection.metrics.fileCountLowerBound,
    directoryCountLowerBound: state.projection.metrics.directoryCountLowerBound,
    byteCountLowerBound: state.projection.metrics.byteCountLowerBound,
    unsettledTargetCount: state.projection.unsettledTargets.length,
  })
}

function provenTrace(state: SelectionProjectionState): ProjectionTraceEvent {
  const proof = state.projection.proof
  if (proof.kind === 'unknown') {
    throw new SelectionProjectionError('unknown proof cannot produce a proven trace')
  }
  return Object.freeze({
    name: 'projection_transition',
    transition: 'proven',
    projectionEpoch: state.projection.epoch,
    shapeProof: proof.kind,
    layoutBasisClass: proof.kind === 'tree' ? proof.layoutBasis.kind : 'unsettled',
  })
}
