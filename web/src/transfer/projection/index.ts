export {
  MAX_PROJECTION_GENERATION_REFERENCES,
  MAX_PROJECTION_SELECTED_ROOT_FACTS,
  MAX_PROJECTION_UNSETTLED_TARGETS,
  SelectionProjectionError,
  createAuthenticatedProjectionEvidence,
  nextProjectionEpoch,
  selectionTargetKey,
  unsettledTargetsForSelection,
} from './model'
export {
  SELECTION_PROJECTION_VERSION,
  type ArtifactShapeProof,
  type AuthenticatedProjectionEvidence,
  type DiscoveryState,
  type LayoutBasisProof,
  type ProjectedDirectoryAnchor,
  type ProjectedFileFact,
  type ProjectionEpoch,
  type ProjectionMetrics,
  type RetryableDiscoveryReason,
  type SelectedRootFact,
  type SelectionProjectionEvent,
  type SelectionProjectionState,
  type SelectionProjectionV1,
  type SettledLayoutBasisProof,
  type UnsettledSelectionTarget,
  type WorkspaceCostObservationV1,
} from './contracts'
export {
  type ProjectionTraceEvent,
  type ProjectionTraceSource,
} from './trace'
export {
  type AuthenticatedGenerationReference,
} from '../preparation/generation'
export {
  SelectionProjectionController,
  createSelectionProjectionState,
  reduceSelectionProjection,
} from './reducer'
export {
  RetryableProjectionDiscoveryError,
  discoverAuthenticatedSelection,
  retryAuthenticatedSelectionDiscovery,
  type AuthenticatedDiscoveryCompletion,
  type AuthenticatedDiscoveryRequest,
  type AuthenticatedDiscoverySource,
} from './coordinator'
