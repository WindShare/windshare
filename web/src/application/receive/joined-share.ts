import type { V2SelectionPolicy } from '../../catalog/v2-selection'
import type { V2ProtocolSessionIdentity } from '../../session/v2-identities'
import type { AuthenticatedDiscoverySource } from '../../transfer/projection'
import type { V2ConnectivityActivation } from '../../connectivity/v2-receiver-policy'
import type { V2FrozenSelectionPolicy } from '../../catalog/v2-selection'
import type { ReceiveIntent } from '../../transfer/intent'
import type { V2PlanExecutionAuthority } from '../../transfer/output/session'
import type { TransferJob, TransferJobOptions } from '../../transfer/v2-job'

export interface ActiveReceiveJoinedShare {
  beginDownloadConnectivity(): V2ConnectivityActivation
  transferJob(
    plans: V2PlanExecutionAuthority,
    intent: ReceiveIntent,
    connectivity: V2ConnectivityActivation,
    callbacks?: Partial<Pick<TransferJobOptions,
      'onProgress' | 'onMeasure' | 'trace' | 'transferJobId' | 'incidentScope' | 'outputSettlementDeadline' | 'onCheckpointObservation'>> &
      { readonly selection?: V2FrozenSelectionPolicy },
  ): Pick<TransferJob, 'run'>
}

export interface ReceiveJoinedShare extends ActiveReceiveJoinedShare {
  readonly descriptor: { readonly shareInstanceId: string; readonly syntheticRootId: string }
  readonly protocolSessionId: string
}

export interface ReceivePlanningShare extends ReceiveJoinedShare {
  readonly selection: Pick<V2SelectionPolicy, 'snapshot' | 'intentSummary'>
  readonly protocolSessionIdentity: V2ProtocolSessionIdentity
  projectionSource(selection: V2FrozenSelectionPolicy, explicitRetry?: boolean): AuthenticatedDiscoverySource
}
