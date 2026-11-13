import type { FailureCorrelation } from '../../diagnostics/incident/index'
import type { DomainTraceSource } from '../../diagnostics/trace/ports'
import type {
  ProjectionEpoch,
  RetryableDiscoveryReason,
  DiscoveryState,
  LayoutBasisProof,
  ArtifactShapeProof,
} from './contracts'

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
