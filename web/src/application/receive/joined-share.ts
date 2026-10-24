import type { SelectionPolicy } from '../../catalog/selection'
import type { ProtocolSessionIdentity } from '../../session/identities'
import type { AuthenticatedDiscoverySource } from '../../transfer/projection'
import type { ConnectivityActivation } from '../../connectivity/receiver-policy'
import type { FrozenSelectionPolicy } from '../../catalog/selection'
import type { ReceiveIntent } from '../../transfer/intent'
import type { PlanExecutionAuthority } from '../../transfer/output/session'
import type { TransferJob, TransferJobOptions } from '../../transfer/job'

export interface ActiveReceiveJoinedShare {
  beginDownloadConnectivity(): ConnectivityActivation
  transferJob(
    plans: PlanExecutionAuthority,
    intent: ReceiveIntent,
    connectivity: ConnectivityActivation,
    callbacks?: Partial<Pick<TransferJobOptions,
      'onProgress' | 'onMeasure' | 'trace' | 'transferJobId' | 'incidentScope' | 'outputSettlementDeadline' | 'onCheckpointObservation'>> &
      { readonly selection?: FrozenSelectionPolicy },
  ): Pick<TransferJob, 'run'>
}

export interface ReceiveJoinedShare extends ActiveReceiveJoinedShare {
  readonly descriptor: { readonly shareInstanceId: string; readonly syntheticRootId: string }
  readonly protocolSessionId: string
}

export interface ReceivePlanningShare extends ReceiveJoinedShare {
  readonly selection: Pick<SelectionPolicy, 'snapshot' | 'intentSummary'>
  readonly protocolSessionIdentity: ProtocolSessionIdentity
  projectionSource(selection: FrozenSelectionPolicy, explicitRetry?: boolean): AuthenticatedDiscoverySource
}
