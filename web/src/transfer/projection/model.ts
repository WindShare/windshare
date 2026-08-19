import {
  canonicalizePortableCatalogPath,
  isPortableCatalogName,
} from '../../catalog/path-policy'
import { V2_CATALOG_DIRECTORY_ENTRIES } from '../../catalog/v2-records'
import { decodeBase64Url } from '../../crypto/bytes'
import type { FailureCorrelation } from '../../diagnostics/incident'
import type { DomainTraceSource } from '../../diagnostics/trace/ports'
import {
  MAX_SELECTION_RULES,
  STABLE_IDENTITY_BYTES,
  type SelectionSpec,
} from '../intent'

export const SELECTION_PROJECTION_VERSION = 1 as const
export const MAX_PROJECTION_SELECTED_ROOT_FACTS = MAX_SELECTION_RULES
export const MAX_PROJECTION_GENERATION_REFERENCES = V2_CATALOG_DIRECTORY_ENTRIES
export const MAX_PROJECTION_UNSETTLED_TARGETS = MAX_SELECTION_RULES + 1

const MAXIMUM_PROJECTION_EPOCH = (1n << 64n) - 1n
const MAXIMUM_PROJECTION_BYTES = (1n << 64n) - 1n
const AUTHENTICATED_EVIDENCE = new WeakSet<object>()
const TEXT_ENCODER = new TextEncoder()
declare const projectionEpochBrand: unique symbol

export type ProjectionEpoch = bigint & { readonly [projectionEpochBrand]: 'ProjectionEpoch' }

export type RetryableDiscoveryReason =
  | 'catalog-temporarily-unavailable'
  | 'receiver-reconnecting'
  | 'generation-replay-interrupted'

export type DiscoveryState =
  | Readonly<{ kind: 'idle' }>
  | Readonly<{ kind: 'discovering' }>
  | Readonly<{ kind: 'retryable-failure'; reason: RetryableDiscoveryReason }>
  | Readonly<{ kind: 'complete' }>

export interface ProjectedFileFact {
  readonly fileId: string
  readonly sourcePath: string
  readonly portableName: string
}

export interface ProjectedDirectoryAnchor {
  readonly directoryId: string
  readonly sourcePath: string
}

export type SelectedRootFact =
  | Readonly<{
      kind: 'file'
      fileId: string
      sourcePath: string
      portableName: string
    }>
  | Readonly<{
      kind: 'directory'
      directoryId: string
      sourcePath: string
      portableName: string
    }>

export type LayoutBasisProof =
  | Readonly<{ kind: 'unsettled' }>
  | Readonly<{ kind: 'complete-directory'; anchor: ProjectedDirectoryAnchor }>
  | Readonly<{ kind: 'directory-selection'; anchor: ProjectedDirectoryAnchor }>
  | Readonly<{ kind: 'synthetic-selection' }>

export type SettledLayoutBasisProof = Exclude<LayoutBasisProof, Readonly<{ kind: 'unsettled' }>>

export type ArtifactShapeProof =
  | Readonly<{ kind: 'unknown' }>
  | Readonly<{ kind: 'none' }>
  | Readonly<{ kind: 'single-file'; file: ProjectedFileFact }>
  | Readonly<{
      kind: 'tree'
      selectedRoots: readonly SelectedRootFact[]
      selectedRootCountLowerBound: number
      selectedRootsTruncated: boolean
      layoutBasis: LayoutBasisProof
    }>

export interface ProjectionMetrics {
  readonly fileCountLowerBound: number
  readonly directoryCountLowerBound: number
  readonly byteCountLowerBound: bigint
}

export interface AuthenticatedGenerationReference {
  readonly directoryId: string
  readonly generation: string
}

export type UnsettledSelectionTarget =
  | Readonly<{ kind: 'synthetic-root'; syntheticRoot: string }>
  | Readonly<{ kind: 'node-id'; nodeKind: 'directory' | 'file'; id: string }>
  | Readonly<{ kind: 'catalog-path'; path: string }>

export interface SelectionProjectionV1 {
  readonly version: typeof SELECTION_PROJECTION_VERSION
  readonly epoch: ProjectionEpoch
  readonly selectionDigest: string
  readonly selectedRoots: readonly SelectedRootFact[]
  readonly selectedRootCountLowerBound: number
  readonly selectedRootsTruncated: boolean
  readonly generations: readonly AuthenticatedGenerationReference[]
  readonly metrics: ProjectionMetrics
  readonly unsettledTargets: readonly UnsettledSelectionTarget[]
  /** Retained only while one authenticated file could still become SingleFile proof. */
  readonly singleFileCandidate?: ProjectedFileFact
  readonly proof: ArtifactShapeProof
}

export interface SelectionProjectionState {
  readonly projection: SelectionProjectionV1
  readonly discovery: DiscoveryState
}

export interface AuthenticatedProjectionEvidence {
  readonly generations: readonly AuthenticatedGenerationReference[]
  readonly metrics: ProjectionMetrics
  readonly representativeFile?: ProjectedFileFact
  readonly selectedRoots: readonly SelectedRootFact[]
  readonly selectedRootCount: number
  readonly settledTargets: readonly UnsettledSelectionTarget[]
  /** Synthetic layout is the only basis that cannot be invalidated by later roots. */
  readonly earlyLayoutBasis?: Readonly<{ kind: 'synthetic-selection' }>
}

export type SelectionProjectionEvent =
  | Readonly<{ kind: 'discovery-started'; epoch: ProjectionEpoch }>
  | Readonly<{
      kind: 'authenticated-evidence'
      epoch: ProjectionEpoch
      evidence: AuthenticatedProjectionEvidence
    }>
  | Readonly<{
      kind: 'retryable-failure'
      epoch: ProjectionEpoch
      reason: RetryableDiscoveryReason
    }>
  | Readonly<{ kind: 'retry-started'; epoch: ProjectionEpoch }>
  | Readonly<{
      kind: 'discovery-completed'
      epoch: ProjectionEpoch
      settledTargets?: readonly UnsettledSelectionTarget[]
      layoutBasis?: SettledLayoutBasisProof
    }>

export type ProjectionTraceEvent =
  | Readonly<{
      name: 'projection_transition'
      transition: 'started'
      projectionEpoch: ProjectionEpoch
      correlation?: FailureCorrelation
    }>
  | Readonly<{
      name: 'projection_transition'
      transition: 'refined'
      projectionEpoch: ProjectionEpoch
      shapeProof: ArtifactShapeProof['kind']
      discoveryState: DiscoveryState['kind']
      fileCountLowerBound: number
      directoryCountLowerBound: number
      byteCountLowerBound: bigint
      unsettledTargetCount: number
    }>
  | Readonly<{
      name: 'projection_transition'
      transition: 'proven'
      projectionEpoch: ProjectionEpoch
      shapeProof: Exclude<ArtifactShapeProof['kind'], 'unknown'>
      layoutBasisClass: LayoutBasisProof['kind']
    }>
  | Readonly<{
      name: 'projection_transition'
      transition: 'retryable_failure'
      projectionEpoch: ProjectionEpoch
      shapeProof: ArtifactShapeProof['kind']
      retryableDiscoveryReason: RetryableDiscoveryReason
    }>
  | Readonly<{
      name: 'projection_transition'
      transition: 'retry_started'
      projectionEpoch: ProjectionEpoch
      retainedShapeProof: ArtifactShapeProof['kind']
    }>
  | Readonly<{
      name: 'projection_transition'
      transition: 'stale_event_dropped'
      currentProjectionEpoch: ProjectionEpoch
      staleProjectionEpoch: ProjectionEpoch
      eventClass: 'catalog_evidence' | 'discovery_result'
    }>

export type ProjectionTraceSource = DomainTraceSource<ProjectionTraceEvent>

export class SelectionProjectionError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'SelectionProjectionError'
  }
}

export function nextProjectionEpoch(previous: bigint): ProjectionEpoch {
  if (previous < 0n || previous >= MAXIMUM_PROJECTION_EPOCH) {
    throw new SelectionProjectionError('projection epoch exceeds its unsigned 64-bit domain')
  }
  return (previous + 1n) as ProjectionEpoch
}

export function unsettledTargetsForSelection(
  selection: SelectionSpec,
): readonly UnsettledSelectionTarget[] {
  const targets: UnsettledSelectionTarget[] = []
  if (selection.rules.mode === 'node-id') {
    if (selection.rules.defaultSelected) {
      targets.push(Object.freeze({
        kind: 'synthetic-root',
        syntheticRoot: requireIdentity(selection.syntheticRoot, 'synthetic root'),
      }))
    }
    for (const rule of selection.rules.rules) {
      if (!rule.selected) continue
      targets.push(Object.freeze({
        kind: 'node-id',
        nodeKind: rule.kind,
        id: requireIdentity(rule.id, 'selection target'),
      }))
    }
  } else {
    for (const path of selection.rules.paths) {
      targets.push(Object.freeze({ kind: 'catalog-path', path: requirePath(path) }))
    }
  }
  if (targets.length > MAX_PROJECTION_UNSETTLED_TARGETS) {
    throw new SelectionProjectionError('selection has too many unsettled projection targets')
  }
  targets.sort((left, right) => compareUtf8(selectionTargetKey(left), selectionTargetKey(right)))
  return Object.freeze(targets)
}

export function selectionTargetKey(target: UnsettledSelectionTarget): string {
  switch (target.kind) {
    case 'synthetic-root': return `1:${target.syntheticRoot}`
    case 'node-id': return `2:${target.nodeKind}:${target.id}`
    case 'catalog-path': return `3:${target.path}`
  }
}

/**
 * Only an adapter that has checked committed catalog authority should call this
 * constructor. The reducer rejects structurally forged evidence so authentication
 * cannot be accidentally bypassed by controller code.
 */
export function createAuthenticatedProjectionEvidence(input: {
  readonly generations: readonly AuthenticatedGenerationReference[]
  readonly metrics: ProjectionMetrics
  readonly representativeFile?: ProjectedFileFact
  readonly selectedRoots?: readonly SelectedRootFact[]
  readonly selectedRootCount?: number
  readonly settledTargets?: readonly UnsettledSelectionTarget[]
  readonly earlyLayoutBasis?: Readonly<{ kind: 'synthetic-selection' }>
}): AuthenticatedProjectionEvidence {
  if (input.generations.length === 0 ||
      input.generations.length > MAX_PROJECTION_GENERATION_REFERENCES) {
    throw new SelectionProjectionError('authenticated evidence has an invalid generation count')
  }
  const generations = snapshotGenerationReferences(input.generations)
  const metrics = snapshotMetrics(input.metrics)
  const representativeFile = input.representativeFile === undefined
    ? undefined
    : snapshotFileFact(input.representativeFile)
  if (metrics.fileCountLowerBound === 1 && representativeFile === undefined) {
    throw new SelectionProjectionError('single-file evidence requires its authenticated file fact')
  }
  const selectedRoots = Object.freeze((input.selectedRoots ?? []).map(snapshotSelectedRoot))
  if (selectedRoots.length > MAX_PROJECTION_SELECTED_ROOT_FACTS) {
    throw new SelectionProjectionError('authenticated evidence retains too many selected roots')
  }
  requireUniqueSelectedRoots(selectedRoots)
  const selectedRootCount = input.selectedRootCount ?? selectedRoots.length
  requireCount(selectedRootCount, 'selected root count')
  if (selectedRootCount < selectedRoots.length) {
    throw new SelectionProjectionError('selected root count is below its retained root facts')
  }
  const settledTargets = snapshotTargets(input.settledTargets ?? [])
  const evidence = Object.freeze({
    generations,
    metrics,
    ...(representativeFile === undefined ? {} : { representativeFile }),
    selectedRoots,
    selectedRootCount,
    settledTargets,
    ...(input.earlyLayoutBasis === undefined
      ? {}
      : { earlyLayoutBasis: Object.freeze({ kind: 'synthetic-selection' as const }) }),
  })
  AUTHENTICATED_EVIDENCE.add(evidence)
  return evidence
}

export function requireAuthenticatedProjectionEvidence(
  evidence: AuthenticatedProjectionEvidence,
): void {
  if (!AUTHENTICATED_EVIDENCE.has(evidence)) {
    throw new SelectionProjectionError('projection evidence did not cross authenticated construction')
  }
}

export function snapshotLayoutBasis(basis: LayoutBasisProof): LayoutBasisProof {
  if (basis.kind === 'unsettled' || basis.kind === 'synthetic-selection') {
    return Object.freeze({ kind: basis.kind })
  }
  return Object.freeze({ kind: basis.kind, anchor: snapshotDirectoryAnchor(basis.anchor) })
}

export function snapshotFileFact(fact: ProjectedFileFact): ProjectedFileFact {
  return Object.freeze({
    fileId: requireIdentity(fact.fileId, 'file'),
    sourcePath: requirePath(fact.sourcePath),
    portableName: requirePortableName(fact.portableName),
  })
}

export function snapshotDirectoryAnchor(
  anchor: ProjectedDirectoryAnchor,
): ProjectedDirectoryAnchor {
  return Object.freeze({
    directoryId: requireIdentity(anchor.directoryId, 'directory'),
    sourcePath: requirePath(anchor.sourcePath),
  })
}

export function snapshotSelectedRoot(root: SelectedRootFact): SelectedRootFact {
  if (root.kind === 'file') return Object.freeze({ kind: 'file', ...snapshotFileFact(root) })
  return Object.freeze({
    kind: 'directory',
    ...snapshotDirectoryAnchor(root),
    portableName: requirePortableName(root.portableName),
  })
}

export function snapshotMetrics(metrics: ProjectionMetrics): ProjectionMetrics {
  requireCount(metrics.fileCountLowerBound, 'file lower bound')
  requireCount(metrics.directoryCountLowerBound, 'directory lower bound')
  if (metrics.byteCountLowerBound < 0n || metrics.byteCountLowerBound > MAXIMUM_PROJECTION_BYTES) {
    throw new SelectionProjectionError('byte lower bound exceeds its unsigned 64-bit domain')
  }
  return Object.freeze({ ...metrics })
}

function snapshotGenerationReferences(
  references: readonly AuthenticatedGenerationReference[],
): readonly AuthenticatedGenerationReference[] {
  const snapshot = references.map((reference) => Object.freeze({
    directoryId: requireIdentity(reference.directoryId, 'generation directory'),
    generation: requireIdentity(reference.generation, 'catalog generation'),
  }))
  snapshot.sort(compareGenerationReferences)
  for (let index = 1; index < snapshot.length; index += 1) {
    if (snapshot[index - 1]?.directoryId === snapshot[index]?.directoryId) {
      throw new SelectionProjectionError('authenticated evidence repeats a directory generation')
    }
  }
  return Object.freeze(snapshot)
}

function snapshotTargets(
  targets: readonly UnsettledSelectionTarget[],
): readonly UnsettledSelectionTarget[] {
  if (targets.length > MAX_PROJECTION_UNSETTLED_TARGETS) {
    throw new SelectionProjectionError('authenticated evidence settles too many targets')
  }
  const snapshot = targets.map((target): UnsettledSelectionTarget => {
    switch (target.kind) {
      case 'synthetic-root': return Object.freeze({
        kind: 'synthetic-root',
        syntheticRoot: requireIdentity(target.syntheticRoot, 'synthetic root'),
      })
      case 'node-id': return Object.freeze({
        kind: 'node-id',
        nodeKind: target.nodeKind,
        id: requireIdentity(target.id, 'selection target'),
      })
      case 'catalog-path': return Object.freeze({
        kind: 'catalog-path',
        path: requirePath(target.path),
      })
    }
  })
  snapshot.sort((left, right) => compareUtf8(selectionTargetKey(left), selectionTargetKey(right)))
  for (let index = 1; index < snapshot.length; index += 1) {
    if (selectionTargetKey(snapshot[index - 1]!) === selectionTargetKey(snapshot[index]!)) {
      throw new SelectionProjectionError('authenticated evidence repeats a settled target')
    }
  }
  return Object.freeze(snapshot)
}

function requireUniqueSelectedRoots(roots: readonly SelectedRootFact[]): void {
  const identities = new Set<string>()
  for (const root of roots) {
    const identity = root.kind === 'file' ? `file:${root.fileId}` : `directory:${root.directoryId}`
    if (identities.has(identity)) {
      throw new SelectionProjectionError('authenticated evidence repeats a selected root')
    }
    identities.add(identity)
  }
}

export function compareGenerationReferences(
  left: AuthenticatedGenerationReference,
  right: AuthenticatedGenerationReference,
): number {
  return compareIdentity(left.directoryId, right.directoryId) ||
    compareIdentity(left.generation, right.generation)
}

function compareIdentity(left: string, right: string): number {
  const leftBytes = decodeBase64Url(left)
  const rightBytes = decodeBase64Url(right)
  if (leftBytes === undefined || rightBytes === undefined) {
    throw new SelectionProjectionError('projection identity is not canonical base64url')
  }
  for (let index = 0; index < leftBytes.length; index += 1) {
    const difference = (leftBytes[index] ?? 0) - (rightBytes[index] ?? 0)
    if (difference !== 0) return difference
  }
  return leftBytes.length - rightBytes.length
}

function compareUtf8(left: string, right: string): number {
  const leftBytes = TEXT_ENCODER.encode(left)
  const rightBytes = TEXT_ENCODER.encode(right)
  const length = Math.min(leftBytes.length, rightBytes.length)
  for (let index = 0; index < length; index += 1) {
    const difference = (leftBytes[index] ?? 0) - (rightBytes[index] ?? 0)
    if (difference !== 0) return difference
  }
  return leftBytes.length - rightBytes.length
}

function requireIdentity(value: string, label: string): string {
  const decoded = decodeBase64Url(value)
  if (decoded?.byteLength !== STABLE_IDENTITY_BYTES || decoded.every((byte) => byte === 0)) {
    throw new SelectionProjectionError(`${label} identity is not canonical`)
  }
  return value
}

function requirePath(path: string): string {
  const canonical = canonicalizePortableCatalogPath(path)
  if (canonical !== path) throw new SelectionProjectionError('projection path is not canonical')
  return canonical
}

function requirePortableName(name: string): string {
  if (!isPortableCatalogName(name)) {
    throw new SelectionProjectionError('projection name violates the portable catalog policy')
  }
  return name
}

function requireCount(value: number, label: string): void {
  if (!Number.isSafeInteger(value) || value < 0) {
    throw new SelectionProjectionError(`${label} exceeds exact integer representation`)
  }
}
