import type { DirectZipCompletionProofV1, DirectZipWriterCheckpointV1 } from '../writer'
import type { DirectZipIntent } from '../../../transfer/direct-zip/model'
import type { ReceiveLifecycleState } from '../../../transfer/lifecycle/state'
import type { MaterializationSummary, PlanPauseRequest, PlanSettlementRequest } from '../../../transfer/output/session'
import type { SuccessfulTransferWorkerSettlement } from '../../../transfer/outcome'

export interface DirectZipStableEvidenceV1 {
  readonly checkpoint: DirectZipWriterCheckpointV1
  readonly materialization: MaterializationSummary
  readonly additionalTemporaryBytesUpperBound: bigint
}

export interface DirectZipPublishedEvidenceV1 extends DirectZipStableEvidenceV1 {
  readonly completion: DirectZipCompletionProofV1
}

export interface DirectZipSettlementAuthorityV1 {
  pause(
    intent: DirectZipIntent,
    request: PlanPauseRequest,
    evidence: DirectZipStableEvidenceV1,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState>
  settle(
    intent: DirectZipIntent,
    request: PlanSettlementRequest<SuccessfulTransferWorkerSettlement>,
    evidence: DirectZipPublishedEvidenceV1,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState>
}
