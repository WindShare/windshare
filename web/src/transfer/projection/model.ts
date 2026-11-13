import { canonicalizeCatalogPath, isCatalogName } from '../../catalog/path-policy'
import { CATALOG_DIRECTORY_ENTRIES } from '../../catalog/records'
import { decodeBase64Url } from '../../crypto/bytes'
import { MAX_SELECTION_RULES, STABLE_IDENTITY_BYTES, type SelectionSpec } from '../intent'
import type {
  ProjectionEpoch,
  ProjectedFileFact,
  ProjectedDirectoryAnchor,
  SelectedRootFact,
  LayoutBasisProof,
  SettledLayoutBasisProof,
  ProjectionMetrics,
  AuthenticatedGenerationReference,
  UnsettledSelectionTarget,
  AuthenticatedProjectionEvidence,
} from './contracts'

export const MAX_PROJECTION_SELECTED_ROOT_FACTS = MAX_SELECTION_RULES

export const MAX_PROJECTION_GENERATION_REFERENCES = CATALOG_DIRECTORY_ENTRIES

export const MAX_PROJECTION_UNSETTLED_TARGETS = MAX_SELECTION_RULES + 1

const MAXIMUM_PROJECTION_EPOCH = (1n << 64n) - 1n

const MAXIMUM_PROJECTION_BYTES = (1n << 64n) - 1n

const AUTHENTICATED_EVIDENCE = new WeakSet<object>()

const TEXT_ENCODER = new TextEncoder()

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
  readonly earlyLayoutBasis?: SettledLayoutBasisProof
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
      : { earlyLayoutBasis: snapshotSettledLayoutBasis(input.earlyLayoutBasis) }),
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

function snapshotSettledLayoutBasis(basis: SettledLayoutBasisProof): SettledLayoutBasisProof {
  const snapshot = snapshotLayoutBasis(basis)
  if (snapshot.kind === 'unsettled') {
    throw new SelectionProjectionError('early layout evidence must prove a settled basis')
  }
  return snapshot
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
    catalogName: requireCatalogName(fact.catalogName),
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
    catalogName: requireCatalogName(root.catalogName),
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
  const canonical = canonicalizeCatalogPath(path)
  if (canonical !== path) throw new SelectionProjectionError('projection path is not canonical')
  return canonical
}

function requireCatalogName(name: string): string {
  if (!isCatalogName(name)) {
    throw new SelectionProjectionError('projection name violates the catalog path policy')
  }
  return name
}

function requireCount(value: number, label: string): void {
  if (!Number.isSafeInteger(value) || value < 0) {
    throw new SelectionProjectionError(`${label} exceeds exact integer representation`)
  }
}
