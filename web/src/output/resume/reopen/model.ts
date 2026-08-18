import type { ReceiveIntent } from '../../../transfer/intent'
import type {
  PersistedFSAOperationBinding,
  verifyFSAOperationBinding,
} from '../../browser/indexeddb-root-binding'
import type {
  BrowserReceiveOperationLease,
  BrowserReceiveOperationLeaseOptions,
  acquireBrowserReceiveOperationLease,
} from '../../browser/session-lease'
import type {
  OriginPrivateStorageEstimate,
  OriginPrivateWorkspaceBudgetClaim,
} from '../../origin-private/admission'
import type { reopenOriginPrivateWorkspaceNamespace } from '../../origin-private/namespace'
import type { OriginPrivateWorkspaceNamespace } from '../../origin-private/namespace'
import type {
  OriginPrivatePackageContinuationBackend,
  OriginPrivateWorkspaceBackend,
  openOriginPrivateWorkspaceBackend,
} from '../../origin-private/session'
import type { PersistentTreeTrace } from '../../persistent-tree/contracts'
import type { PersistedReceiveRecord, ReceiveOperationV1 } from '../../workspace/records'
import type { ReceiveOperationRepository } from '../../workspace/repository'
import type { PlanKind, ReceiveLifecycleState } from '../../workspace/state'
import type {
  AdmittedWorkspaceContent,
  WorkspaceBudgetClaim,
  WorkspaceBudgetClaimResult,
  WorkspaceContentRequestCounter,
  WorkspaceOperationStages,
} from '../../workspace/stages'
import type { SealedWorkspaceZipPreparationV1 } from '../../workspace/preparation'
import type {
  OpenOriginPrivatePackageContinuation,
  ReopenedWorkspacePackageContinuation,
} from '../workspace-continuation'
import type { ReceiveOperationResumeDescriptor } from '../descriptor'
import type { PreparationAdmissionReceiptV1 } from '../../workspace/receipts'

export type PersistedReceiveOperationReopenPurpose = 'continue' | 'cleanup'

export type StableLifecycleKind =
  | 'resumable-receive'
  | 'resumable-package'
  | 'waiting-to-save'
  | 'download-started'

export type PersistedReceiveOperationReopenTraceEvent =
  | Readonly<{
      name: 'receive.operation.reopen_authorized'
      operation_id: string
      receive_intent_digest: string
      lifecycle_generation: bigint
      continuation: ReceiveOperationResumeDescriptor['continuation']
      lease_id: string
    }>
  | Readonly<{
      name: 'receive.operation.expired'
      operation_id: string
      prior_stable_state: StableLifecycleKind
      expires_at_ms: number
    }>
  | Readonly<{
      name: 'receive.operation.needs_attention'
      operation_id: string
      prior_state: ReceiveLifecycleState['kind']
      needs_attention_reason: 'target-ownership-unknown'
    }>

export type PersistedReceiveOperationReopenTrace = (
  event: PersistedReceiveOperationReopenTraceEvent,
) => void

export interface ReopenedReceiveOperationBase {
  readonly intent: ReceiveIntent
  readonly lifecycle: ReceiveLifecycleState
  readonly lease: BrowserReceiveOperationLease
  readonly repository: ReceiveOperationRepository
  close(): Promise<void>
}

export interface ReopenedDirectTreeOperation extends ReopenedReceiveOperationBase {
  readonly kind: 'direct-tree'
  readonly binding: PersistedFSAOperationBinding
  readonly receiveAdmissionFallback?: Extract<ReceiveLifecycleState, { kind: 'resumable-receive' }>
}

export interface ReopenedWorkspaceOperation extends ReopenedReceiveOperationBase {
  readonly kind: 'workspace'
  readonly namespace: OriginPrivateWorkspaceNamespace
  readonly stages: WorkspaceOperationStages
  /** Present only when resume-receive reclaimed the exact durable admission authority. */
  readonly admittedContent?: AdmittedWorkspaceContent
  readonly preparation?: SealedWorkspaceZipPreparationV1
  readonly receiveContinuation?: ReopenedWorkspaceReceiveContinuation
  readonly receiveAdmissionFallback?: Extract<ReceiveLifecycleState, { kind: 'resumable-receive' }>
  readonly packageContinuation?: ReopenedWorkspacePackageContinuation
}

export interface ReopenedWorkspaceReceiveContinuation {
  readonly preparation?: SealedWorkspaceZipPreparationV1
  openBackend(options?: { readonly onTrace?: PersistentTreeTrace }): Promise<OriginPrivateWorkspaceBackend>
}

export type ReopenedReceiveOperation = ReopenedDirectTreeOperation | ReopenedWorkspaceOperation

export class PersistedReceiveOperationDeadlineElapsedError extends DOMException {
  readonly state: Extract<ReceiveLifecycleState, { kind: 'expired' }>
  readonly receipt: import('../../workspace/receipts').ExpiryReceiptV1

  constructor(
    state: Extract<ReceiveLifecycleState, { kind: 'expired' }>,
    receipt: import('../../workspace/receipts').ExpiryReceiptV1,
  ) {
    super('Receive operation retention deadline elapsed before reopen', 'InvalidStateError')
    this.state = state
    this.receipt = receipt
  }
}

export class PersistedReceiveOperationNeedsAttentionError extends DOMException {
  readonly state: Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>

  constructor(state: Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>) {
    super('Receive operation ownership requires attention', 'InvalidStateError')
    this.state = state
  }
}

export class PersistedWorkspaceBudgetReclaimRejectedError extends DOMException {
  readonly budgetDigest: string
  readonly reason: Extract<
    WorkspaceBudgetClaimResult,
    { kind: 'rejected' }
  >['admission']['reason']

  constructor(result: Extract<WorkspaceBudgetClaimResult, { kind: 'rejected' }>) {
    super('Retained workspace no longer fits the current storage budget', 'QuotaExceededError')
    this.budgetDigest = result.admission.budgetDigest
    this.reason = result.admission.reason
  }
}

export type LeaseOptions = Omit<BrowserReceiveOperationLeaseOptions, 'acquireTransition'>

export interface PersistedWorkspaceBudgetReclaimInput {
  readonly intent: ReceiveIntent
  readonly namespace: OriginPrivateWorkspaceNamespace
  readonly repository: ReceiveOperationRepository
  readonly operationLease: BrowserReceiveOperationLease
  readonly budget: AdmittedWorkspaceContent['budget']
  readonly receipt: PreparationAdmissionReceiptV1
  readonly estimate: () => Promise<OriginPrivateStorageEstimate>
  readonly now: () => number
  readonly databaseName?: string
}

export type PersistedWorkspaceBudgetReclaim = (
  input: PersistedWorkspaceBudgetReclaimInput,
) => Promise<WorkspaceBudgetClaimResult>

export interface PersistedReceiveOperationReopenAuthorityOptions {
  readonly repositoryFactory: () => Promise<ReceiveOperationRepository>
  readonly clock?: { now(): number }
  readonly leaseOptions?: LeaseOptions
  readonly acquireLease?: typeof acquireBrowserReceiveOperationLease
  readonly verifyDirectTreeBinding?: typeof verifyFSAOperationBinding
  readonly reopenWorkspaceNamespace?: typeof reopenOriginPrivateWorkspaceNamespace
  readonly openWorkspaceStages?: typeof WorkspaceOperationStages.open
  readonly reclaimWorkspaceBudget?: PersistedWorkspaceBudgetReclaim
  readonly estimateWorkspaceStorage?: () => Promise<OriginPrivateStorageEstimate>
  readonly workspaceBudgetDatabaseName?: string
  readonly checkpointDatabaseName?: string
  readonly openWorkspacePackageContinuation?: OpenOriginPrivatePackageContinuation
  readonly openWorkspaceReceiveBackend?: typeof openOriginPrivateWorkspaceBackend
  readonly contentRequests?: WorkspaceContentRequestCounter
  readonly trace?: PersistedReceiveOperationReopenTrace
}

export interface PersistedReopenSnapshot {
  readonly operation: ReceiveOperationV1
  readonly operationRecord: PersistedReceiveRecord
  readonly bindingRecord: PersistedReceiveRecord
  readonly lifecycle: ReceiveLifecycleState
  readonly lifecycleRecord: PersistedReceiveRecord
}

export type ReopenedReceiveTarget =
  | Readonly<{ kind: 'direct-tree'; binding: PersistedFSAOperationBinding }>
  | Readonly<{ kind: 'workspace'; namespace: OriginPrivateWorkspaceNamespace }>

export interface ReopenResources {
  lease?: BrowserReceiveOperationLease
  reclaimedClaim?: WorkspaceBudgetClaim
  packageBackend?: OriginPrivatePackageContinuationBackend
  receiveBackend?: OriginPrivateWorkspaceBackend
  receiveBackendOpening?: Promise<OriginPrivateWorkspaceBackend>
  closed?: boolean
}

export interface ReopenLifecycleAuthority {
  readonly lifecycle: ReceiveLifecycleState
  readonly receiveAdmissionFallback?: Extract<ReceiveLifecycleState, { kind: 'resumable-receive' }>
  readonly stages?: WorkspaceOperationStages
  readonly admittedContent?: AdmittedWorkspaceContent
  readonly preparation?: SealedWorkspaceZipPreparationV1
  readonly packageContinuation?: ReopenedWorkspacePackageContinuation
  readonly receiveContinuation?: ReopenedWorkspaceReceiveContinuation
}

export interface ReducerContext {
  readonly planKind: PlanKind
  readonly preparationRequired: boolean
  readonly activeLeaseId: string
  readonly nowMilliseconds: number
}

export type { OriginPrivateWorkspaceBudgetClaim }
