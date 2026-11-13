export const SELECTION_PROJECTION_VERSION = 1 as const

declare const projectionEpochBrand: unique symbol

export type ProjectionEpoch = bigint & { readonly [projectionEpochBrand]: 'ProjectionEpoch' }

export type RetryableDiscoveryReason =
  | 'catalog-temporarily-unavailable'
  | 'receiver-reconnecting'
  | 'generation-replay-interrupted'

export type DiscoveryState =
  | Readonly<{ kind: 'idle' }>
  | Readonly<{ kind: 'discovering' }>
  | Readonly<{ kind: 'bounded' }>
  | Readonly<{ kind: 'retryable-failure'; reason: RetryableDiscoveryReason }>
  | Readonly<{ kind: 'complete' }>

export interface ProjectedFileFact {
  readonly fileId: string
  readonly sourcePath: string
  readonly catalogName: string
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
      catalogName: string
    }>
  | Readonly<{
      kind: 'directory'
      directoryId: string
      sourcePath: string
      catalogName: string
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

/** Native OPFS recommendation costs; payload is already contained in the archive. */
export interface WorkspaceCostObservationV1 {
  readonly version: 1
  readonly archiveBytes: bigint
  readonly durableMetadataBytes: bigint
  readonly peakOwnedBytes: bigint
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
  readonly workspaceCostObservation?: WorkspaceCostObservationV1
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
  /** All selected roots and exclusion paths must be authenticated before settling layout. */
  readonly earlyLayoutBasis?: SettledLayoutBasisProof
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
  | Readonly<{ kind: 'discovery-bounded'; epoch: ProjectionEpoch }>
  | Readonly<{
      kind: 'discovery-completed'
      epoch: ProjectionEpoch
      settledTargets?: readonly UnsettledSelectionTarget[]
      layoutBasis?: SettledLayoutBasisProof
      workspaceCostObservation?: WorkspaceCostObservationV1
    }>
