import { BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS } from '../../transfer/intent'
import {
  validateDestinationReservation,
  validateReceiveIntent,
  type AtomicTargetReservation,
  type ReceiveIntent,
} from '../../transfer/intent'
import {
  createPublicationAttempt,
  decodePackagedArtifactV1,
  sealPackagedArtifact,
  sealWorkspaceMaterialization,
  type PackagedArtifactV1,
  type PublicationAttemptV1,
  type SealedMaterializationV1,
} from './aggregate'
import {
  admitWorkspaceBudget,
  createPreparedZipWorkspaceBudget,
  createSingleFileWorkspaceBudget,
  validateWorkspaceBudget,
  type WorkspaceBudgetAdmission,
  type WorkspaceBudgetV1,
  type WorkspaceCapacitySnapshot,
} from './budget'
import {
  executeWorkspaceCleanup,
  type WorkspaceOwnedCleanupPort,
  type WorkspaceOwnedObjectCleanupTarget,
} from './cleanup'
import {
  canonicalDigest,
  snapshotIdentity,
} from './canonical'
import { reduceReceiveLifecycle, type LifecycleEvent } from './lifecycle'
import {
  createMaterializedManifestPages,
  materializedGenerationTableDigest,
  sealMaterializedManifest,
  type AuthenticatedGenerationReference,
  type FinalCheckpointReader,
  type MaterializedManifestEntry,
  type MaterializedManifestV1,
  type PreparationBinding,
} from './manifest'
import {
  type SealedWorkspaceZipPreparationV1,
} from './preparation'
import {
  createHandoffReceipt,
  createCleanupReceipt,
  createExpiryReceipt,
  createManagedPublicationReceipt,
  createPackageReceipt,
  createPackageTemporaryCleanupReceipt,
  createPreparationAdmissionReceipt,
  createRawWorkspaceReceipt,
  createWorkspaceSealReceipt,
  decodePreparationAdmissionReceipt,
  persistedReceiptRecord,
  type HandoffReceiptV1,
  type CleanupReceiptV1,
  type ExpiryReceiptV1,
  type ManagedPublicationReceiptV1,
  type PackageReceiptV1,
  type PreparationAdmissionReceiptV1,
  type WorkspaceSealReceiptV1,
  type ArtifactVerificationReceiptV1,
  validateArtifactVerificationReceipt,
} from './receipts'
import {
  validateSealedZipLayoutPlan,
  type SealedZipLayoutPlanV1,
} from '../zip-layout/layout'
import {
  createPersistedReceiveRecord,
  createReceiveOperationV1,
  receiveOperationHandleRecord,
  RECEIVE_RECORD_MATERIALIZED_MANIFEST,
  RECEIVE_RECORD_LIFECYCLE_STATE,
  RECEIVE_RECORD_OPERATION,
  RECEIVE_RECORD_PACKAGE,
  RECEIVE_RECORD_PUBLICATION_ATTEMPT,
  RECEIVE_RECORD_RECEIPT,
  RECEIVE_RECORD_RESERVATION,
  RECEIVE_RECORD_SEALED_MATERIALIZATION,
  RECEIVE_RECORD_WORKSPACE_BINDING,
  storedReceiveOperationRecord,
  type ReceiveOperationHandleRecord,
} from './records'
import type { ReceiveOperationRepository } from './repository'
import { decodeStoredReceiveLifecycleState, storedReceiveLifecycleState } from './state-codec'
import {
  initialReceiveLifecycleState,
  lifecycleDeadline,
  type ExternalAttemptReason,
  type PackageFailureReason,
  type PreparationAdmissionReason,
  type ReceiveLifecycleState,
} from './state'

export const WORKSPACE_HANDLE_ROOT = 16 as const
export const WORKSPACE_HANDLE_RAW_OBJECT = 17 as const
export const WORKSPACE_HANDLE_PACKAGE_OBJECT = 18 as const
export const WORKSPACE_HANDLE_ZIP_LAYOUT = 19 as const
export const WORKSPACE_HANDLE_PUBLICATION_TARGET = 20 as const

const CONTENT_GATES = new WeakSet<object>()
const U64_MAXIMUM = 0xffff_ffff_ffff_ffffn
const WORKSPACE_ZIP_LAYOUT_HANDLE_DOMAIN = 'windshare/workspace-zip-layout/v1'

export function workspaceZipLayoutHandleId(operationId: string, preparationId: string): string {
  return `${WORKSPACE_ZIP_LAYOUT_HANDLE_DOMAIN}/${snapshotIdentity(
    operationId,
    16,
    'operation ID',
  )}/${snapshotIdentity(preparationId, 16, 'preparation ID')}`
}

export interface WorkspaceContentRequestCounter {
  count(): bigint
}

export interface WorkspaceBudgetClaim {
  readonly budgetDigest: string
  readonly capacity: WorkspaceCapacitySnapshot
  readonly admission: Extract<WorkspaceBudgetAdmission, { kind: 'accepted' }>
  release(): Promise<void>
}

export type WorkspaceBudgetClaimResult =
  | Readonly<{ kind: 'accepted'; claim: WorkspaceBudgetClaim }>
  | Readonly<{
      kind: 'rejected'
      capacity: WorkspaceCapacitySnapshot
      admission: Extract<WorkspaceBudgetAdmission, { kind: 'rejected' }>
    }>

export interface WorkspaceBudgetAuthority {
  claim(budget: WorkspaceBudgetV1): Promise<WorkspaceBudgetClaimResult>
}

export interface WorkspaceContentGate {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly workspaceBudgetDigest: string
  readonly preparationManifestDigest?: string
}

export interface PackageTemporaryCleanupEvidence {
  readonly operationId: string
  readonly packageOwnedObjectId: string
  readonly packageHandleId: string
  readonly result: 'removed' | 'already-absent'
  readonly digest: string
}

type WorkspaceReceiveIntent = ReceiveIntent & {
  readonly plan: Extract<ReceiveIntent['plan'], { kind: 'workspace-then-publish' }>
}

export type WorkspaceStageTraceEvent =
  | Readonly<{
      name: 'receive.preparation.started'
      operation_id: string
      receive_intent_digest: string
      preparation_id: string
      artifact_kind: 'zip-archive'
    }>
  | Readonly<{
      name: 'receive.preparation.sealed'
      operation_id: string
      receive_intent_digest: string
      preparation_digest: string
      entry_count: bigint
      file_count: bigint
      directory_count: bigint
      selected_bytes: bigint
      metadata_bytes: bigint
    }>
  | Readonly<{
      name: 'receive.preparation_admission.accepted'
      operation_id: string
      receive_intent_digest: string
      plan_kind: 'workspace-then-publish'
      admission_kind: 'workspace-budget'
      artifact_bytes: bigint
      metadata_bytes: bigint
      unique_raw_bytes: bigint
      package_bytes: bigint
      peak_temporary_bytes: bigint
      durable_metadata_bytes: bigint
      peak_owned_bytes: bigint
      limit_class: 'none'
    }>
  | Readonly<{
      name: 'receive.preparation_admission.rejected'
      operation_id: string
      receive_intent_digest: string
      plan_kind: 'workspace-then-publish'
      admission_kind: 'workspace-budget'
      artifact_bytes: bigint
      metadata_bytes: bigint
      unique_raw_bytes: bigint
      package_bytes: bigint
      peak_temporary_bytes: bigint
      durable_metadata_bytes: bigint
      peak_owned_bytes: bigint
      limit_class: 'workspace-job' | 'workspace-process' | 'workspace-quota'
      preparation_admission_reason: PreparationAdmissionReason
    }>
  | Readonly<{
      name: 'receive.materialization.completed'
      operation_id: string
      receive_intent_digest: string
      transfer_job_id: string
      entry_count: bigint
      file_count: bigint
      directory_count: bigint
      raw_bytes: bigint
    }>
  | Readonly<{
      name: 'receive.materialization.sealed'
      operation_id: string
      receive_intent_digest: string
      sealed_materialization_digest: string
      entry_count: bigint
      file_count: bigint
      directory_count: bigint
      raw_bytes: bigint
    }>
  | Readonly<{
      name: 'receive.package.started'
      operation_id: string
      sealed_materialization_digest: string
      artifact_kind: 'original-file' | 'zip-archive'
    }>
  | Readonly<{
      name: 'receive.package.sealed'
      operation_id: string
      package_digest: string
      layout_digest: string
      artifact_bytes: bigint
    }>
  | Readonly<{
      name: 'receive.package.retry_started'
      operation_id: string
      sealed_materialization_digest: string
      package_failure_reason: PackageFailureReason
    }>
  | Readonly<{
      name: 'receive.waiting_to_save'
      operation_id: string
      package_digest: string
      expires_at_ms: number
    }>
  | Readonly<{
      name: 'receive.publication.started'
      operation_id: string
      package_digest: string
      publication_attempt_id: string
      publication_route: 'managed' | 'handoff'
    }>
  | Readonly<{
      name: 'receive.publication.committed'
      operation_id: string
      package_digest: string
      publication_attempt_id: string
      artifact_bytes: bigint
    }>
  | Readonly<{
      name: 'receive.publication.not_committed'
      operation_id: string
      package_digest: string
      publication_attempt_id: string
      external_attempt_reason: ExternalAttemptReason
    }>
  | Readonly<{
      name: 'receive.publication.unknown'
      operation_id: string
      package_digest: string
      publication_attempt_id: string
      needs_attention_reason: 'publication-unknown'
    }>
  | Readonly<{
      name: 'receive.operation.needs_attention'
      operation_id: string
      prior_state: ReceiveLifecycleState['kind']
      needs_attention_reason: 'cleanup-unknown' | 'publication-unknown' | 'target-ownership-unknown'
    }>
  | Readonly<{
      name: 'receive.handoff.started'
      operation_id: string
      attempt_kind: 'workspace'
      attempt_id: string
      package_digest_present: true
      package_digest: string
      object_url_lease_ms: bigint
    }>
  | Readonly<{
      name: 'receive.handoff.download_started'
      operation_id: string
      attempt_kind: 'workspace'
      attempt_id: string
      package_digest_present: true
      package_digest: string
      retryable_until_present: true
      retryable_until_ms: number
    }>
  | Readonly<{
      name: 'receive.handoff.not_started'
      operation_id: string
      attempt_kind: 'workspace'
      attempt_id: string
      external_attempt_reason: ExternalAttemptReason
    }>
  | Readonly<{
      name: 'receive.handoff.unknown'
      operation_id: string
      attempt_kind: 'workspace'
      attempt_id: string
      needs_attention_reason: 'publication-unknown'
    }>
  | Readonly<{
      name: 'receive.materialization.paused'
      operation_id: string
      receive_intent_digest: string
      resumable_stage: 'receive'
      completed_file_count: bigint
      completed_bytes: bigint
      expires_at_ms: number
    }>
  | Readonly<{
      name: 'receive.operation.expired'
      operation_id: string
      prior_stable_state: ExpiryReceiptV1['priorStableState']
      expires_at_ms: number
    }>
  | Readonly<{
      name: 'receive.operation.discarded' | 'receive.operation.cleanup_completed'
      operation_id: string
      cleanup_generation: bigint
      removed_object_count: bigint
      removed_record_count: bigint
    }>

export type WorkspaceStageTraceListener = (event: WorkspaceStageTraceEvent) => void

export interface AdmittedWorkspaceContent {
  readonly gate: WorkspaceContentGate
  readonly budget: WorkspaceBudgetV1
  readonly admissionReceipt: PreparationAdmissionReceiptV1
  readonly claim: WorkspaceBudgetClaim
}

export interface WorkspaceCleanupRequest {
  readonly targets: readonly WorkspaceOwnedObjectCleanupTarget[]
  readonly metadataHandleIds?: readonly string[]
  readonly port: WorkspaceOwnedCleanupPort
}

export type WorkspaceCleanupResult =
  | Readonly<{
      kind: 'clean'
      receipt: CleanupReceiptV1
      state: ReceiveLifecycleState
    }>
  | Readonly<{
      kind: 'retryable-failure'
      state: ReceiveLifecycleState
    }>
  | Readonly<{
      kind: 'needs-attention'
      receipt: CleanupReceiptV1
      state: Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>
    }>

export async function persistWorkspaceOperation(input: {
  readonly repository: ReceiveOperationRepository
  readonly receiveIntent: ReceiveIntent
  readonly workspaceRootHandleId: string
  readonly workspaceOwnedObjectId: string
  readonly workspaceRootHandle: FileSystemDirectoryHandle
}): Promise<void> {
  const intent = await requireWorkspaceIntent(input.receiveIntent)
  const operation = await createReceiveOperationV1({ receiveIntent: intent })
  const workspaceRecord = await createPersistedReceiveRecord({
    operationId: intent.operationId,
    kind: RECEIVE_RECORD_WORKSPACE_BINDING,
    canonicalBytes: intent.plan.workspace.canonicalBytes,
  })
  const lifecycle = initialReceiveLifecycleState({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
  })
  const root = receiveOperationHandleRecord({
    id: input.workspaceRootHandleId,
    operationId: intent.operationId,
    kind: WORKSPACE_HANDLE_ROOT,
    authorityRef: intent.plan.workspace.repositoryRef,
    ownedObjectId: input.workspaceOwnedObjectId,
    handle: input.workspaceRootHandle,
  })
  await input.repository.commitTransition({
    operationId: intent.operationId,
    records: [storedReceiveOperationRecord(operation), workspaceRecord],
    handles: [root],
    lifecycle,
  })
}

export class WorkspaceOperationStages {
  readonly #repository: ReceiveOperationRepository
  readonly #intent: WorkspaceReceiveIntent
  readonly #leaseId: string
  readonly #clock: () => number
  readonly #contentRequests: WorkspaceContentRequestCounter
  readonly #trace: WorkspaceStageTraceListener | undefined

  private constructor(input: {
    readonly repository: ReceiveOperationRepository
    readonly intent: WorkspaceReceiveIntent
    readonly leaseId: string
    readonly clock: () => number
    readonly contentRequests: WorkspaceContentRequestCounter
    readonly trace?: WorkspaceStageTraceListener
  }) {
    this.#repository = input.repository
    this.#intent = input.intent
    this.#leaseId = snapshotIdentity(input.leaseId, 16, 'lease ID')
    this.#clock = input.clock
    this.#contentRequests = input.contentRequests
    this.#trace = input.trace
  }

  static async open(input: {
    readonly repository: ReceiveOperationRepository
    readonly receiveIntent: ReceiveIntent
    readonly leaseId: string
    readonly clock: () => number
    readonly contentRequests: WorkspaceContentRequestCounter
    readonly onTrace?: WorkspaceStageTraceListener
  }): Promise<WorkspaceOperationStages> {
    return new WorkspaceOperationStages({
      repository: input.repository,
      intent: await requireWorkspaceIntent(input.receiveIntent),
      leaseId: input.leaseId,
      clock: input.clock,
      contentRequests: input.contentRequests,
      ...(input.onTrace === undefined ? {} : { trace: input.onTrace }),
    })
  }

  async beginReceive(preparationId?: string): Promise<ReceiveLifecycleState> {
    const state = await this.#lifecycle()
    const requiresPreparation = this.#intent.plan.preparation === 'exact-zip'
    const event = this.#event({
      kind: 'receive-started',
      ...(preparationId === undefined ? {} : { preparationId }),
    }, state)
    const next = this.#reduce(state, event)
    await this.#commitLifecycle(state, next)
    if (next.kind === 'preparing') {
      this.#emit({
        name: 'receive.preparation.started',
        operation_id: this.#intent.operationId,
        receive_intent_digest: this.#intent.digest,
        preparation_id: next.preparationId,
        artifact_kind: 'zip-archive',
      })
    } else if (requiresPreparation) {
      throw new TypeError('workspace ZIP bypassed preparation')
    }
    return next
  }

  async admitPreparedZip(input: {
    readonly preparation: SealedWorkspaceZipPreparationV1
    readonly authority: WorkspaceBudgetAuthority
    readonly durableMetadataBytesExcludingAdmissionRecords: bigint
    readonly rejectionCleanup: WorkspaceCleanupRequest
  }): Promise<
    | Readonly<{ kind: 'accepted'; content: AdmittedWorkspaceContent }>
    | Readonly<{
        kind: 'rejected'
        reason: PreparationAdmissionReason
        admission: Extract<WorkspaceBudgetAdmission, { kind: 'rejected' }>
        state: ReceiveLifecycleState
      }>
  > {
    const state = await this.#lifecycle()
    if (state.kind !== 'preparing') throw new TypeError('preparation admission requires preparing state')
    this.#requireZeroContentRequests()
    const next = this.#reduce(state, this.#event({ kind: 'preparation-admitted' }, state))
    const preparationRecord = await createPersistedReceiveRecord({
      operationId: this.#intent.operationId,
      kind: 4,
      canonicalBytes: input.preparation.manifest.canonicalBytes,
    })
    const layoutHandle = receiveOperationHandleRecord({
      id: workspaceZipLayoutHandleId(
        this.#intent.operationId,
        input.preparation.manifest.preparationId,
      ),
      operationId: this.#intent.operationId,
      kind: WORKSPACE_HANDLE_ZIP_LAYOUT,
      authorityRef: this.#intent.plan.workspace.repositoryRef,
      handle: input.preparation.zipLayout,
    })
    const budget = await this.#preparedBudgetWithExactMetadata({
      preparation: input.preparation,
      next,
      baseMetadataBytes: input.durableMetadataBytesExcludingAdmissionRecords,
    })
    const claimResult = await input.authority.claim(budget)
    const expectedAdmission = admitWorkspaceBudget(
      budget,
      claimResult.kind === 'accepted' ? claimResult.claim.capacity : claimResult.capacity,
    )
    if (claimResult.kind === 'rejected') {
      if (expectedAdmission.kind !== 'rejected' ||
          expectedAdmission.reason !== claimResult.admission.reason ||
          expectedAdmission.limitClass !== claimResult.admission.limitClass) {
        throw new TypeError('workspace budget authority returned a dishonest rejection')
      }
      this.#emitAdmissionRejected(budget, input.preparation, expectedAdmission)
      const cleanup = await this.#finishCleanup(
        state,
        input.rejectionCleanup,
        [],
        expectedAdmission.reason,
      )
      if (cleanup.kind === 'retryable-failure') {
        throw new DOMException('Rejected workspace cleanup must be retried', 'OperationError')
      }
      return Object.freeze({
        kind: 'rejected',
        reason: expectedAdmission.reason,
        admission: expectedAdmission,
        state: cleanup.state,
      })
    }
    if (expectedAdmission.kind !== 'accepted' ||
        claimResult.claim.budgetDigest !== budget.digest ||
        claimResult.claim.admission.budgetDigest !== budget.digest ||
        claimResult.claim.admission.incrementalPhysicalPeakBytes !==
          expectedAdmission.incrementalPhysicalPeakBytes) {
      await claimResult.claim.release()
      throw new TypeError('workspace budget authority returned a dishonest claim')
    }
    const receipt = await this.#preparationAdmissionReceipt(
      budget,
      claimResult.claim.capacity,
      expectedAdmission,
      input.preparation,
    )
    const receiptRecord = await persistedReceiptRecord(receipt)
    this.#requireZeroContentRequests()
    try {
      await this.#repository.commitTransition({
        operationId: this.#intent.operationId,
        expectedLifecycleGeneration: state.generation,
        expectedLeaseId: this.#leaseId,
        records: [preparationRecord, receiptRecord],
        manifestPages: input.preparation.pages,
        handles: [layoutHandle],
        lifecycle: next,
      })
    } catch (error) {
      await claimResult.claim.release().catch(() => undefined)
      throw error
    }
    this.#emitPreparationAccepted(input.preparation, budget)
    const gate = issueContentGate({
      operationId: this.#intent.operationId,
      receiveIntentDigest: this.#intent.digest,
      workspaceBudgetDigest: budget.digest,
      preparationManifestDigest: input.preparation.manifest.digest,
    })
    return Object.freeze({
      kind: 'accepted',
      content: Object.freeze({ gate, budget, admissionReceipt: receipt, claim: claimResult.claim }),
    })
  }

  async admitSingleFile(input: {
    readonly fileId: string
    readonly containingDirectoryId: string
    readonly generation: string
    readonly catalogSize: bigint
    readonly authority: WorkspaceBudgetAuthority
    readonly durableMetadataBytesExcludingAdmissionRecords: bigint
    readonly rejectionCleanup: WorkspaceCleanupRequest
  }): Promise<
    | Readonly<{ kind: 'accepted'; content: AdmittedWorkspaceContent }>
    | Readonly<{
        kind: 'rejected'
        reason: PreparationAdmissionReason
        admission: Extract<WorkspaceBudgetAdmission, { kind: 'rejected' }>
        state: ReceiveLifecycleState
      }>
  > {
    const state = await this.#lifecycle()
    if (state.kind !== 'intent-frozen') {
      throw new TypeError('single-file admission requires frozen intent state')
    }
    this.#requireZeroContentRequests()
    const next = this.#reduce(state, this.#event({ kind: 'receive-started' }, state))
    const budget = await this.#singleFileBudgetWithExactMetadata({ ...input, next })
    const claimResult = await input.authority.claim(budget)
    const capacity = claimResult.kind === 'accepted' ? claimResult.claim.capacity : claimResult.capacity
    const expectedAdmission = admitWorkspaceBudget(budget, capacity)
    if (claimResult.kind === 'rejected') {
      if (expectedAdmission.kind !== 'rejected' ||
          expectedAdmission.reason !== claimResult.admission.reason) {
        throw new TypeError('workspace budget authority returned a dishonest rejection')
      }
      this.#emitAdmissionRejected(budget, undefined, expectedAdmission)
      const cleanup = await this.#finishCleanup(state, input.rejectionCleanup, [])
      if (cleanup.kind === 'retryable-failure') {
        throw new DOMException('Rejected workspace cleanup must be retried', 'OperationError')
      }
      return Object.freeze({
        kind: 'rejected',
        reason: expectedAdmission.reason,
        admission: expectedAdmission,
        state: cleanup.state,
      })
    }
    if (expectedAdmission.kind !== 'accepted' ||
        claimResult.claim.budgetDigest !== budget.digest) {
      await claimResult.claim.release()
      throw new TypeError('workspace budget authority returned a dishonest claim')
    }
    const receipt = await this.#preparationAdmissionReceipt(
      budget,
      capacity,
      expectedAdmission,
    )
    this.#requireZeroContentRequests()
    try {
      await this.#repository.commitTransition({
        operationId: this.#intent.operationId,
        expectedLifecycleGeneration: state.generation,
        expectedLeaseId: this.#leaseId,
        records: [await persistedReceiptRecord(receipt)],
        lifecycle: next,
      })
    } catch (error) {
      await claimResult.claim.release().catch(() => undefined)
      throw error
    }
    this.#emitPreparationAccepted(undefined, budget)
    const gate = issueContentGate({
      operationId: this.#intent.operationId,
      receiveIntentDigest: this.#intent.digest,
      workspaceBudgetDigest: budget.digest,
    })
    return Object.freeze({
      kind: 'accepted',
      content: Object.freeze({ gate, budget, admissionReceipt: receipt, claim: claimResult.claim }),
    })
  }

  /** Reissues only the ephemeral gate whose exact admitted budget receipt survived the crash. */
  async reopenAdmittedContent(input: {
    readonly budget: WorkspaceBudgetV1
    readonly claim: WorkspaceBudgetClaim
  }): Promise<AdmittedWorkspaceContent> {
    const state = await this.#lifecycle()
    if (state.kind !== 'receiving') {
      throw new TypeError('workspace content can reopen only in receiving state')
    }
    return this.#reopenAdmittedWorkspaceAuthority(input)
  }

  /** Reissues allocation authority without reopening the sealed raw-materialization surface. */
  async reopenAdmittedPackage(input: {
    readonly budget: WorkspaceBudgetV1
    readonly claim: WorkspaceBudgetClaim
  }): Promise<AdmittedWorkspaceContent> {
    const state = await this.#lifecycle()
    if (state.kind !== 'resumable-package') {
      throw new TypeError('workspace package authority requires resumable-package state')
    }
    this.#requireContinuationUnexpired(state)
    return this.#reopenAdmittedWorkspaceAuthority(input)
  }

  async #reopenAdmittedWorkspaceAuthority(input: {
    readonly budget: WorkspaceBudgetV1
    readonly claim: WorkspaceBudgetClaim
  }): Promise<AdmittedWorkspaceContent> {
    try {
      this.#requireZeroContentRequests()
      const budget = await validateWorkspaceBudget(input.budget, this.#intent)
      const expectedAdmission = admitWorkspaceBudget(budget, input.claim.capacity)
      if (expectedAdmission.kind !== 'accepted' ||
          input.claim.budgetDigest !== budget.digest ||
          input.claim.admission.kind !== 'accepted' ||
          input.claim.admission.budgetDigest !== budget.digest ||
          input.claim.admission.incrementalPhysicalPeakBytes !==
            expectedAdmission.incrementalPhysicalPeakBytes) {
        throw new TypeError('recovered workspace budget claim is invalid')
      }
      const receiptRecords = await this.#repository.listRecords(
        this.#intent.operationId,
        RECEIVE_RECORD_RECEIPT,
      )
      const receipts: PreparationAdmissionReceiptV1[] = []
      for (const record of receiptRecords) {
        const receipt = await decodePreparationAdmissionReceipt(record, budget)
        if (receipt !== undefined) receipts.push(receipt)
      }
      if (receipts.length !== 1) {
        throw new TypeError('workspace admission has no unique durable receipt')
      }
      const receipt = receipts[0]!
      this.#requireZeroContentRequests()
      const gate = issueContentGate({
        operationId: this.#intent.operationId,
        receiveIntentDigest: this.#intent.digest,
        workspaceBudgetDigest: budget.digest,
        ...(receipt.preparationManifestDigest === undefined
          ? {}
          : { preparationManifestDigest: receipt.preparationManifestDigest }),
      })
      return Object.freeze({ budget, claim: input.claim, admissionReceipt: receipt, gate })
    } catch (error) {
      await input.claim.release().catch(() => undefined)
      throw error
    }
  }

  async readRetainedPackage(): Promise<PackagedArtifactV1> {
    const state = await this.#lifecycle()
    let packageDigest: string | undefined
    if (state.kind === 'waiting-to-save' ||
        (state.kind === 'download-started' && state.attemptKind === 'workspace')) {
      packageDigest = state.packageDigest
    }
    if (packageDigest === undefined) {
      throw new TypeError('workspace has no retained package authority')
    }
    const records = await this.#repository.listRecords(
      this.#intent.operationId,
      RECEIVE_RECORD_PACKAGE,
    )
    const matches: PackagedArtifactV1[] = []
    for (const record of records) {
      const artifact = await decodePackagedArtifactV1(record.canonicalBytes)
      if (record.operationId !== artifact.operationId || record.digest !== artifact.digest) {
        throw new TypeError('persisted package record changed its canonical authority')
      }
      if (artifact.digest === packageDigest) matches.push(artifact)
    }
    if (matches.length !== 1) {
      throw new TypeError('workspace retained package record is missing or ambiguous')
    }
    const artifact = matches[0]!
    if (artifact.operationId !== this.#intent.operationId ||
        artifact.receiveIntentDigest !== this.#intent.digest ||
        artifact.artifactSpecDigest !== this.#intent.artifact.digest) {
      throw new TypeError('retained package escaped its receive intent')
    }
    return artifact
  }

  async sealMaterialization(input: {
    readonly transferJobId: string
    readonly generations: readonly AuthenticatedGenerationReference[]
    readonly entries: readonly MaterializedManifestEntry[]
    readonly checkpoints: FinalCheckpointReader
    readonly preparation?: SealedWorkspaceZipPreparationV1
  }): Promise<Readonly<{
    manifest: MaterializedManifestV1
    receipt: WorkspaceSealReceiptV1
    seal: SealedMaterializationV1
  }>> {
    const state = await this.#lifecycle()
    if (state.kind !== 'receiving') throw new TypeError('materialization seal requires receiving state')
    const preparationBinding: PreparationBinding = input.preparation === undefined
      ? Object.freeze({ kind: 'absent' })
      : Object.freeze({ kind: 'present', preparationDigest: input.preparation.manifest.digest })
    if ((this.#intent.plan.preparation === 'exact-zip') !== (input.preparation !== undefined)) {
      throw new TypeError('materialization preparation binding disagrees with the receive intent')
    }
    const manifest = await sealMaterializedManifest({
      operationId: this.#intent.operationId,
      receiveIntentDigest: this.#intent.digest,
      materializationBindingDigest: this.#intent.plan.workspace.digest,
      preparationBinding,
      generations: input.generations,
      entries: input.entries,
      checkpoints: input.checkpoints,
      ...(input.preparation === undefined ? {} : { preparation: input.preparation.manifest }),
    })
    const pages = await createMaterializedManifestPages(manifest)
    const rawReceipt = await createRawWorkspaceReceipt({
      operationId: this.#intent.operationId,
      receiveIntentDigest: this.#intent.digest,
      workspaceBindingDigest: this.#intent.plan.workspace.digest,
      materializedManifestDigest: manifest.digest,
      ownedObjects: manifest.entries.map((entry) => Object.freeze({
        ownedObjectId: entry.ownedObjectId,
        exactBytes: entry.kind === 'file' ? entry.exactSize : 0n,
      })),
    })
    if (rawReceipt.uniqueRawBytes !== manifest.rawBytes) {
      throw new TypeError('raw workspace receipt disagrees with materialized bytes')
    }
    const seal = await sealWorkspaceMaterialization({
      operationId: this.#intent.operationId,
      receiveIntentDigest: this.#intent.digest,
      workspaceBindingDigest: this.#intent.plan.workspace.digest,
      preparationBinding,
      materializedManifestDigest: manifest.digest,
      generationTableDigest: await materializedGenerationTableDigest(manifest.generations),
      artifactVersion: this.#intent.artifact.version,
      layoutVersion: 1,
      rawWorkspaceReceiptDigest: rawReceipt.digest,
    })
    const receipt = await createWorkspaceSealReceipt({
      operationId: this.#intent.operationId,
      receiveIntentDigest: this.#intent.digest,
      workspaceBindingDigest: this.#intent.plan.workspace.digest,
      sealedMaterializationDigest: seal.digest,
      rawWorkspaceReceipt: rawReceipt,
    })
    const next = this.#reduce(state, this.#event({
      kind: 'materialization-seal-verified',
      sealedMaterializationDigest: seal.digest,
    }, state))
    const records = await Promise.all([
      createPersistedReceiveRecord({
        operationId: this.#intent.operationId,
        kind: RECEIVE_RECORD_MATERIALIZED_MANIFEST,
        canonicalBytes: manifest.canonicalBytes,
      }),
      persistedReceiptRecord(receipt),
      createPersistedReceiveRecord({
        operationId: this.#intent.operationId,
        kind: RECEIVE_RECORD_SEALED_MATERIALIZATION,
        canonicalBytes: seal.canonicalBytes,
      }),
    ])
    await this.#repository.commitTransition({
      operationId: this.#intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.#leaseId,
      records,
      manifestPages: pages,
      lifecycle: next,
    })
    const transferJobId = snapshotIdentity(input.transferJobId, 16, 'transfer job ID')
    this.#emit({
      name: 'receive.materialization.completed',
      operation_id: this.#intent.operationId,
      receive_intent_digest: this.#intent.digest,
      transfer_job_id: transferJobId,
      entry_count: manifest.entryCount,
      file_count: manifest.fileCount,
      directory_count: manifest.directoryCount,
      raw_bytes: manifest.rawBytes,
    })
    this.#emit({
      name: 'receive.materialization.sealed',
      operation_id: this.#intent.operationId,
      receive_intent_digest: this.#intent.digest,
      sealed_materialization_digest: seal.digest,
      entry_count: manifest.entryCount,
      file_count: manifest.fileCount,
      directory_count: manifest.directoryCount,
      raw_bytes: manifest.rawBytes,
    })
    return Object.freeze({ manifest, receipt, seal })
  }

  async startPackage(
    sealedMaterialization: SealedMaterializationV1,
    packageHandle: ReceiveOperationHandleRecord,
  ): Promise<ReceiveLifecycleState> {
    const state = await this.#lifecycle()
    if (state.kind !== 'materialization-sealed' ||
        state.sealedMaterializationDigest !== sealedMaterialization.digest ||
        packageHandle.kind !== WORKSPACE_HANDLE_PACKAGE_OBJECT ||
        packageHandle.operationId !== this.#intent.operationId ||
        packageHandle.ownedObjectId === undefined) {
      throw new TypeError('package allocation escaped its sealed workspace')
    }
    const next = this.#reduce(state, this.#event({
      kind: 'package-started',
      packageTempObjectId: packageHandle.ownedObjectId,
    }, state))
    await this.#repository.commitTransition({
      operationId: this.#intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.#leaseId,
      handles: [packageHandle],
      lifecycle: next,
    })
    this.#emit({
      name: 'receive.package.started',
      operation_id: this.#intent.operationId,
      sealed_materialization_digest: sealedMaterialization.digest,
      artifact_kind: this.#artifactKind(),
    })
    return next
  }

  async recordRetryablePackageFailure(input: {
    readonly reason: PackageFailureReason
    readonly temporaryCleanup: PackageTemporaryCleanupEvidence
  }): Promise<ReceiveLifecycleState> {
    const state = await this.#lifecycle()
    if (state.kind !== 'packaging') throw new TypeError('package failure requires packaging state')
    if (input.temporaryCleanup.operationId !== this.#intent.operationId ||
        input.temporaryCleanup.packageOwnedObjectId !== state.packageTempObjectId) {
      throw new TypeError('temporary package cleanup escaped its active allocation')
    }
    const cleanupReceipt = await createPackageTemporaryCleanupReceipt({
      operationId: this.#intent.operationId,
      receiveIntentDigest: this.#intent.digest,
      sealedMaterializationDigest: state.sealedMaterializationDigest,
      packageOwnedObjectId: input.temporaryCleanup.packageOwnedObjectId,
      packageHandleId: input.temporaryCleanup.packageHandleId,
      cleanupResult: input.temporaryCleanup.result,
      cleanupProofDigest: input.temporaryCleanup.digest,
    })
    const next = this.#reduce(state, this.#event({
      kind: 'package-retryable-failure',
      reason: input.reason,
      tempCleanupProofDigest: cleanupReceipt.digest,
    }, state))
    await this.#repository.commitTransition({
      operationId: this.#intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.#leaseId,
      records: [await persistedReceiptRecord(cleanupReceipt)],
      deleteHandleIds: [cleanupReceipt.packageHandleId],
      lifecycle: next,
    })
    this.#emit({
      name: 'receive.package.retry_started',
      operation_id: this.#intent.operationId,
      sealed_materialization_digest: state.sealedMaterializationDigest,
      package_failure_reason: input.reason,
    })
    return next
  }

  async sealPackage(input: {
    readonly sealedMaterialization: SealedMaterializationV1
    readonly materializedManifest: MaterializedManifestV1
    readonly artifactVerification: ArtifactVerificationReceiptV1
    readonly zipLayout?: SealedZipLayoutPlanV1
  }): Promise<Readonly<{
    receipt: PackageReceiptV1
    package: PackagedArtifactV1
    state: Extract<ReceiveLifecycleState, { kind: 'waiting-to-save' }>
  }>> {
    const state = await this.#lifecycle()
    const verification = await validateArtifactVerificationReceipt(input.artifactVerification)
    if (state.kind !== 'packaging' ||
        state.sealedMaterializationDigest !== input.sealedMaterialization.digest ||
        input.sealedMaterialization.operationId !== this.#intent.operationId ||
        input.sealedMaterialization.receiveIntentDigest !== this.#intent.digest ||
        input.sealedMaterialization.materializedManifestDigest !== input.materializedManifest.digest ||
        verification.operationId !== this.#intent.operationId ||
        verification.receiveIntentDigest !== this.#intent.digest ||
        verification.sealedMaterializationDigest !== input.sealedMaterialization.digest ||
        state.packageTempObjectId !== verification.packageOwnedObjectId) {
      throw new TypeError('package proof escaped its active allocation')
    }
    await this.#verifyPackageArtifact(input.materializedManifest, verification, input.zipLayout)
    const packaged = await sealPackagedArtifact({
      operationId: this.#intent.operationId,
      receiveIntentDigest: this.#intent.digest,
      sealedMaterializationDigest: input.sealedMaterialization.digest,
      artifactSpecDigest: this.#intent.artifact.digest,
      packageOwnedObjectId: verification.packageOwnedObjectId,
      exactBytes: verification.exactBytes,
      artifactReceiptDigest: verification.digest,
      layoutDigest: verification.layoutDigest,
    })
    const receipt = await createPackageReceipt({
      operationId: this.#intent.operationId,
      receiveIntentDigest: this.#intent.digest,
      packagedArtifactDigest: packaged.digest,
      artifactVerification: verification,
    })
    const artifactSealed = this.#reduce(state, this.#event({
      kind: 'package-seal-verified',
      packageDigest: packaged.digest,
    }, state))
    const records = await Promise.all([
      persistedReceiptRecord(receipt),
      createPersistedReceiveRecord({
        operationId: this.#intent.operationId,
        kind: RECEIVE_RECORD_PACKAGE,
        canonicalBytes: packaged.canonicalBytes,
      }),
    ])
    await this.#repository.commitTransition({
      operationId: this.#intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.#leaseId,
      records,
      lifecycle: artifactSealed,
    })
    this.#emit({
      name: 'receive.package.sealed',
      operation_id: this.#intent.operationId,
      package_digest: packaged.digest,
      layout_digest: packaged.layoutDigest,
      artifact_bytes: packaged.exactBytes,
    })

    const waiting = this.#reduce(artifactSealed, this.#event({
      kind: 'wait-record-persisted',
    }, artifactSealed))
    if (waiting.kind !== 'waiting-to-save') throw new TypeError('package did not enter WaitingToSave')
    await this.#commitLifecycle(artifactSealed, waiting)
    this.#emit({
      name: 'receive.waiting_to_save',
      operation_id: this.#intent.operationId,
      package_digest: packaged.digest,
      expires_at_ms: waiting.expiresAt,
    })
    return Object.freeze({ receipt, package: packaged, state: waiting })
  }

  async startManagedPublication(input: {
    readonly package: PackagedArtifactV1
    readonly publicationAttemptId: string
    readonly reservation: AtomicTargetReservation
    readonly targetHandle: ReceiveOperationHandleRecord
  }): Promise<PublicationAttemptV1> {
    const state = await this.#lifecycle()
    this.#requireContinuationUnexpired(state)
    const reservation = await validateDestinationReservation(
      input.reservation,
      this.#intent.artifact,
    )
    if (reservation.kind !== 'atomic-target' || reservation.operationId !== this.#intent.operationId ||
        reservation.artifactDigest !== this.#intent.artifact.digest ||
        reservation.guarantees.profile !== 'managed-atomic' ||
        input.targetHandle.kind !== WORKSPACE_HANDLE_PUBLICATION_TARGET ||
        input.targetHandle.authorityRef !== reservation.authorityRef) {
      throw new TypeError('managed publication target is not an atomic package authority')
    }
    this.#assertRetainedPackage(state, input.package)
    const attempt = await createPublicationAttempt({
      publicationAttemptId: input.publicationAttemptId,
      operationId: this.#intent.operationId,
      receiveIntentDigest: this.#intent.digest,
      packagedArtifactDigest: input.package.digest,
      route: Object.freeze({ kind: 'managed', reservationDigest: reservation.digest }),
    })
    const next = this.#reduce(state, this.#event({
      kind: 'save-requested',
      publicationAttemptId: attempt.publicationAttemptId,
    }, state))
    const records = await Promise.all([
      createPersistedReceiveRecord({
        operationId: this.#intent.operationId,
        kind: RECEIVE_RECORD_RESERVATION,
        canonicalBytes: reservation.canonicalBytes,
      }),
      createPersistedReceiveRecord({
        operationId: this.#intent.operationId,
        kind: RECEIVE_RECORD_PUBLICATION_ATTEMPT,
        canonicalBytes: attempt.canonicalBytes,
      }),
    ])
    await this.#repository.commitTransition({
      operationId: this.#intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.#leaseId,
      records,
      handles: [input.targetHandle],
      lifecycle: next,
    })
    this.#emit({
      name: 'receive.publication.started',
      operation_id: this.#intent.operationId,
      package_digest: input.package.digest,
      publication_attempt_id: attempt.publicationAttemptId,
      publication_route: 'managed',
    })
    return attempt
  }

  async recordManagedPublicationCommitted(input: {
    readonly package: PackagedArtifactV1
    readonly attempt: PublicationAttemptV1
    readonly targetAuthorityRef: string
  }): Promise<Readonly<{
    receipt: ManagedPublicationReceiptV1
    state: Extract<ReceiveLifecycleState, { kind: 'published' }>
  }>> {
    const state = await this.#lifecycle()
    if (state.kind !== 'publishing-managed' ||
        state.publicationAttemptId !== input.attempt.publicationAttemptId ||
        state.packageDigest !== input.package.digest || input.attempt.route.kind !== 'managed') {
      throw new TypeError('publication commit proof escaped its active attempt')
    }
    const receipt = await createManagedPublicationReceipt({
      operationId: this.#intent.operationId,
      receiveIntentDigest: this.#intent.digest,
      publicationAttemptId: input.attempt.publicationAttemptId,
      packagedArtifactDigest: input.package.digest,
      reservationDigest: input.attempt.route.reservationDigest,
      targetAuthorityRef: input.targetAuthorityRef,
      exactBytes: input.package.exactBytes,
      commitConfirmed: true,
    })
    const next = this.#reduce(state, this.#event({
      kind: 'publication-committed',
      receiptDigest: receipt.digest,
    }, state))
    if (next.kind !== 'published') throw new TypeError('confirmed publication did not become Published')
    await this.#repository.commitTransition({
      operationId: this.#intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.#leaseId,
      records: [await persistedReceiptRecord(receipt)],
      lifecycle: next,
    })
    this.#emit({
      name: 'receive.publication.committed',
      operation_id: this.#intent.operationId,
      package_digest: input.package.digest,
      publication_attempt_id: input.attempt.publicationAttemptId,
      artifact_bytes: input.package.exactBytes,
    })
    return Object.freeze({ receipt, state: next })
  }

  async recordManagedPublicationNotCommitted(input: {
    readonly package: PackagedArtifactV1
    readonly attempt: PublicationAttemptV1
    readonly reason: ExternalAttemptReason
  }): Promise<Extract<ReceiveLifecycleState, { kind: 'waiting-to-save' }>> {
    const state = await this.#lifecycle()
    this.#assertActiveManagedAttempt(state, input.package, input.attempt)
    const next = this.#reduce(state, this.#event({
      kind: 'publication-not-committed',
      reason: input.reason,
    }, state))
    if (next.kind !== 'waiting-to-save') throw new TypeError('non-commit did not restore WaitingToSave')
    await this.#commitLifecycle(state, next)
    this.#emit({
      name: 'receive.publication.not_committed',
      operation_id: this.#intent.operationId,
      package_digest: input.package.digest,
      publication_attempt_id: input.attempt.publicationAttemptId,
      external_attempt_reason: input.reason,
    })
    return next
  }

  async recordManagedPublicationUnknown(input: {
    readonly package: PackagedArtifactV1
    readonly attempt: PublicationAttemptV1
    readonly lastVerifiedRecordDigest: string
  }): Promise<Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>> {
    const state = await this.#lifecycle()
    this.#assertActiveManagedAttempt(state, input.package, input.attempt)
    const next = this.#reduce(state, this.#event({
      kind: 'publication-unknown',
      lastVerifiedRecordDigest: input.lastVerifiedRecordDigest,
    }, state))
    if (next.kind !== 'needs-attention') throw new TypeError('unknown publication was retried')
    await this.#commitLifecycle(state, next)
    this.#emit({
      name: 'receive.publication.unknown',
      operation_id: this.#intent.operationId,
      package_digest: input.package.digest,
      publication_attempt_id: input.attempt.publicationAttemptId,
      needs_attention_reason: 'publication-unknown',
    })
    this.#emit({
      name: 'receive.operation.needs_attention',
      operation_id: this.#intent.operationId,
      prior_state: state.kind,
      needs_attention_reason: 'publication-unknown',
    })
    return next
  }

  async recordTargetOwnershipUnknown(
    lastVerifiedRecordDigest: string,
  ): Promise<Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>> {
    const state = await this.#lifecycle()
    const next = this.#reduce(state, this.#event({
      kind: 'ownership-unknown',
      lastVerifiedRecordDigest,
    }, state))
    if (next.kind !== 'needs-attention') {
      throw new TypeError('unknown target ownership did not become NeedsAttention')
    }
    await this.#commitLifecycle(state, next)
    this.#emit({
      name: 'receive.operation.needs_attention',
      operation_id: this.#intent.operationId,
      prior_state: state.kind,
      needs_attention_reason: 'target-ownership-unknown',
    })
    return next
  }

  async startHandoff(input: {
    readonly package: PackagedArtifactV1
    readonly publicationAttemptId: string
    readonly suggestedName: string
    readonly packagedFileSupported: boolean
  }): Promise<PublicationAttemptV1> {
    const state = await this.#lifecycle()
    this.#requireContinuationUnexpired(state)
    this.#assertRetainedPackage(state, input.package)
    const attempt = await createPublicationAttempt({
      publicationAttemptId: input.publicationAttemptId,
      operationId: this.#intent.operationId,
      receiveIntentDigest: this.#intent.digest,
      packagedArtifactDigest: input.package.digest,
      route: Object.freeze({
        kind: 'handoff',
        suggestedName: input.suggestedName,
        packagedFileSupported: input.packagedFileSupported,
        objectUrlLeaseMilliseconds: BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
      }),
    })
    const next = this.#reduce(state, this.#event({
      kind: 'handoff-requested',
      attemptKind: 'workspace',
      attemptId: attempt.publicationAttemptId,
    }, state))
    await this.#repository.commitTransition({
      operationId: this.#intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.#leaseId,
      records: [await createPersistedReceiveRecord({
        operationId: this.#intent.operationId,
        kind: RECEIVE_RECORD_PUBLICATION_ATTEMPT,
        canonicalBytes: attempt.canonicalBytes,
      })],
      lifecycle: next,
    })
    this.#emit({
      name: 'receive.publication.started',
      operation_id: this.#intent.operationId,
      package_digest: input.package.digest,
      publication_attempt_id: attempt.publicationAttemptId,
      publication_route: 'handoff',
    })
    this.#emit({
      name: 'receive.handoff.started',
      operation_id: this.#intent.operationId,
      attempt_kind: 'workspace',
      attempt_id: attempt.publicationAttemptId,
      package_digest_present: true,
      package_digest: input.package.digest,
      object_url_lease_ms: BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
    })
    return attempt
  }

  async recordHandoffStarted(input: {
    readonly package: PackagedArtifactV1
    readonly attempt: PublicationAttemptV1
    readonly urlLeaseStartedAt: number
    readonly urlLeaseEndsAt: number
  }): Promise<Readonly<{
    receipt: HandoffReceiptV1
    state: Extract<ReceiveLifecycleState, { kind: 'download-started' }>
  }>> {
    const state = await this.#lifecycle()
    if (state.kind !== 'handing-off' || state.attemptKind !== 'workspace' ||
        state.attemptId !== input.attempt.publicationAttemptId ||
        state.packageDigest !== input.package.digest || input.attempt.route.kind !== 'handoff') {
      throw new TypeError('handoff start proof escaped its active attempt')
    }
    const now = this.#now()
    if (input.urlLeaseStartedAt > now ||
        input.urlLeaseEndsAt !== checkedLeaseEnd(input.urlLeaseStartedAt)) {
      throw new TypeError('handoff URL lease does not use the exact finite duration')
    }
    const receipt = await createHandoffReceipt({
      operationId: this.#intent.operationId,
      receiveIntentDigest: this.#intent.digest,
      publicationAttemptId: input.attempt.publicationAttemptId,
      packagedArtifactDigest: input.package.digest,
      suggestedName: input.attempt.route.suggestedName,
      urlLeaseEndsAt: input.urlLeaseEndsAt,
      handoffStarted: true,
    })
    const next = this.#reduce(state, this.#event({ kind: 'handoff-started' }, state))
    if (next.kind !== 'download-started' || next.attemptKind !== 'workspace') {
      throw new TypeError('started handoff lacks retry state')
    }
    await this.#repository.commitTransition({
      operationId: this.#intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.#leaseId,
      records: [await persistedReceiptRecord(receipt)],
      lifecycle: next,
    })
    this.#emit({
      name: 'receive.handoff.download_started',
      operation_id: this.#intent.operationId,
      attempt_kind: 'workspace',
      attempt_id: input.attempt.publicationAttemptId,
      package_digest_present: true,
      package_digest: input.package.digest,
      retryable_until_present: true,
      retryable_until_ms: next.retryableUntil,
    })
    return Object.freeze({ receipt, state: next })
  }

  async recordHandoffNotStarted(input: {
    readonly package: PackagedArtifactV1
    readonly attempt: PublicationAttemptV1
    readonly reason: ExternalAttemptReason
  }): Promise<ReceiveLifecycleState> {
    const state = await this.#lifecycle()
    this.#assertActiveHandoff(state, input.package, input.attempt)
    const now = this.#now()
    const expired = now >= state.retainedDeadline
    const expiryReceipt = expired
      ? await createExpiryReceipt({
          operationId: this.#intent.operationId,
          receiveIntentDigest: this.#intent.digest,
          priorStableState: 'waiting-to-save',
          expiresAt: state.retainedDeadline,
          cleanupState: 'cleanup-pending',
        })
      : undefined
    const next = this.#reduceAt(state, this.#event({
      kind: 'handoff-not-started',
      reason: input.reason,
      ...(expiryReceipt === undefined ? {} : { expiryReceiptDigest: expiryReceipt.digest }),
    }, state), now)
    await this.#repository.commitTransition({
      operationId: this.#intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.#leaseId,
      ...(expiryReceipt === undefined ? {} : { records: [await persistedReceiptRecord(expiryReceipt)] }),
      lifecycle: next,
    })
    this.#emit({
      name: 'receive.handoff.not_started',
      operation_id: this.#intent.operationId,
      attempt_kind: 'workspace',
      attempt_id: input.attempt.publicationAttemptId,
      external_attempt_reason: input.reason,
    })
    if (next.kind === 'expired') {
      this.#emit({
        name: 'receive.operation.expired',
        operation_id: this.#intent.operationId,
        prior_stable_state: next.priorStableState,
        expires_at_ms: next.expiresAt,
      })
    }
    return next
  }

  async recordHandoffUnknown(input: {
    readonly package: PackagedArtifactV1
    readonly attempt: PublicationAttemptV1
    readonly lastVerifiedRecordDigest: string
  }): Promise<Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>> {
    const state = await this.#lifecycle()
    this.#assertActiveHandoff(state, input.package, input.attempt)
    const next = this.#reduce(state, this.#event({
      kind: 'handoff-unknown',
      lastVerifiedRecordDigest: input.lastVerifiedRecordDigest,
    }, state))
    if (next.kind !== 'needs-attention') throw new TypeError('unknown handoff was retried')
    await this.#commitLifecycle(state, next)
    this.#emit({
      name: 'receive.handoff.unknown',
      operation_id: this.#intent.operationId,
      attempt_kind: 'workspace',
      attempt_id: input.attempt.publicationAttemptId,
      needs_attention_reason: 'publication-unknown',
    })
    this.#emit({
      name: 'receive.operation.needs_attention',
      operation_id: this.#intent.operationId,
      prior_state: state.kind,
      needs_attention_reason: 'publication-unknown',
    })
    return next
  }

  async pauseReceive(input: {
    readonly checkpointSetDigest: string
    readonly completedFileCount: bigint
    readonly completedBytes: bigint
  }): Promise<Extract<ReceiveLifecycleState, { kind: 'resumable-receive' }>> {
    const state = await this.#lifecycle()
    const next = this.#reduce(state, this.#event({
      kind: 'pause-verified',
      stage: 'receive',
      checkpointSetDigest: input.checkpointSetDigest,
      completedFileCount: input.completedFileCount,
      completedBytes: input.completedBytes,
    }, state))
    if (next.kind !== 'resumable-receive') throw new TypeError('workspace pause is not resumable')
    await this.#commitLifecycle(state, next)
    this.#emit({
      name: 'receive.materialization.paused',
      operation_id: this.#intent.operationId,
      receive_intent_digest: this.#intent.digest,
      resumable_stage: 'receive',
      completed_file_count: next.completedFileCount,
      completed_bytes: next.completedBytes,
      expires_at_ms: next.expiresAt,
    })
    return next
  }

  async pausePackage(input: {
    readonly sealedMaterializationDigest: string
    readonly temporaryCleanup: PackageTemporaryCleanupEvidence
  }): Promise<Extract<ReceiveLifecycleState, { kind: 'resumable-package' }>> {
    const state = await this.#lifecycle()
    if (state.kind !== 'packaging' ||
        state.sealedMaterializationDigest !== input.sealedMaterializationDigest ||
        input.temporaryCleanup.operationId !== this.#intent.operationId ||
        input.temporaryCleanup.packageOwnedObjectId !== state.packageTempObjectId) {
      throw new TypeError('package pause cleanup escaped its active allocation')
    }
    const cleanupReceipt = await createPackageTemporaryCleanupReceipt({
      operationId: this.#intent.operationId,
      receiveIntentDigest: this.#intent.digest,
      sealedMaterializationDigest: input.sealedMaterializationDigest,
      packageOwnedObjectId: input.temporaryCleanup.packageOwnedObjectId,
      packageHandleId: input.temporaryCleanup.packageHandleId,
      cleanupResult: input.temporaryCleanup.result,
      cleanupProofDigest: input.temporaryCleanup.digest,
    })
    const next = this.#reduce(state, this.#event({
      kind: 'pause-verified',
      stage: 'package',
      sealedMaterializationDigest: input.sealedMaterializationDigest,
      tempCleanupProofDigest: cleanupReceipt.digest,
    }, state))
    if (next.kind !== 'resumable-package') throw new TypeError('package pause is not resumable')
    await this.#repository.commitTransition({
      operationId: this.#intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.#leaseId,
      records: [await persistedReceiptRecord(cleanupReceipt)],
      deleteHandleIds: [cleanupReceipt.packageHandleId],
      lifecycle: next,
    })
    return next
  }

  async resumeReceive(): Promise<Extract<ReceiveLifecycleState, { kind: 'receiving' }>> {
    const state = await this.#lifecycle()
    this.#requireContinuationUnexpired(state)
    const next = this.#reduce(state, this.#event({ kind: 'resume-started' }, state))
    if (next.kind !== 'receiving') throw new TypeError('receive resume entered the wrong stage')
    await this.#commitLifecycle(state, next)
    return next
  }

  async resumePackage(
    sealedMaterialization: SealedMaterializationV1,
    packageHandle: ReceiveOperationHandleRecord,
  ): Promise<Extract<ReceiveLifecycleState, { kind: 'packaging' }>> {
    const state = await this.#lifecycle()
    this.#requireContinuationUnexpired(state)
    if (state.kind !== 'resumable-package' ||
        state.sealedMaterializationDigest !== sealedMaterialization.digest ||
        packageHandle.kind !== WORKSPACE_HANDLE_PACKAGE_OBJECT ||
        packageHandle.operationId !== this.#intent.operationId ||
        packageHandle.ownedObjectId === undefined) {
      throw new TypeError('package resume escaped its sealed workspace')
    }
    const next = this.#reduce(state, this.#event({
      kind: 'resume-started',
      packageTempObjectId: packageHandle.ownedObjectId,
    }, state))
    if (next.kind !== 'packaging') throw new TypeError('package resume entered the wrong stage')
    await this.#repository.commitTransition({
      operationId: this.#intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.#leaseId,
      handles: [packageHandle],
      lifecycle: next,
    })
    return next
  }

  async discard(request: WorkspaceCleanupRequest): Promise<WorkspaceCleanupResult> {
    const state = await this.#lifecycle()
    if (state.kind === 'published' || state.kind === 'partial-directory' ||
        state.kind === 'restart-required' || state.kind === 'discarded' ||
        state.kind === 'expired') {
      throw new TypeError('workspace state cannot be discarded')
    }
    this.#requireContinuationUnexpired(state)
    return this.#finishCleanup(state, request, [])
  }

  async expireIfDue(request: WorkspaceCleanupRequest): Promise<Readonly<{
    kind: 'not-due'
    state: ReceiveLifecycleState
  }> | Readonly<{
    kind: 'expired'
    expiryReceipt: ExpiryReceiptV1
    cleanup: WorkspaceCleanupResult
  }>> {
    const state = await this.#lifecycle()
    const expiresAt = lifecycleDeadline(state)
    if (expiresAt === undefined) throw new TypeError('workspace state has no stable deadline')
    const now = this.#now()
    if (now < expiresAt) return Object.freeze({ kind: 'not-due', state })
    const priorStableState = stableStateKind(state)
    const expiryReceipt = await createExpiryReceipt({
      operationId: this.#intent.operationId,
      receiveIntentDigest: this.#intent.digest,
      priorStableState,
      expiresAt,
      cleanupState: 'cleanup-pending',
    })
    const expired = this.#reduceAt(state, this.#event({
      kind: 'expiry-observed',
      expiryReceiptDigest: expiryReceipt.digest,
      cleanupState: 'cleanup-pending',
    }, state), now)
    if (expired.kind !== 'expired') throw new TypeError('elapsed workspace did not become Expired')
    await this.#repository.commitTransition({
      operationId: this.#intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.#leaseId,
      records: [await persistedReceiptRecord(expiryReceipt)],
      lifecycle: expired,
    })
    this.#emit({
      name: 'receive.operation.expired',
      operation_id: this.#intent.operationId,
      prior_stable_state: expiryReceipt.priorStableState,
      expires_at_ms: expiryReceipt.expiresAt,
    })
    const cleanup = await this.#finishCleanup(expired, request, [expiryReceipt.digest])
    return Object.freeze({ kind: 'expired', expiryReceipt, cleanup })
  }

  async retryTerminalCleanup(request: WorkspaceCleanupRequest): Promise<WorkspaceCleanupResult> {
    const state = await this.#lifecycle()
    let keepReceiptDigests: readonly string[] | undefined
    if (state.kind === 'published' && state.cleanupState === 'cleanup-pending') {
      keepReceiptDigests = [state.receiptDigest]
    } else if (state.kind === 'expired' && state.cleanupState === 'cleanup-pending') {
      keepReceiptDigests = [state.expiryReceiptDigest]
    }
    if (keepReceiptDigests === undefined) {
      throw new TypeError('workspace has no retryable terminal cleanup')
    }
    return this.#finishCleanup(state, request, keepReceiptDigests)
  }

  async #finishCleanup(
    state: ReceiveLifecycleState,
    request: WorkspaceCleanupRequest,
    keepReceiptDigests: readonly string[],
    preparationRejectionReason?: PreparationAdmissionReason,
  ): Promise<WorkspaceCleanupResult> {
    const execution = await executeWorkspaceCleanup({
      operationId: this.#intent.operationId,
      receiveIntentDigest: this.#intent.digest,
      targets: request.targets,
      port: request.port,
    })
    const cleanedHandleIds = mergeHandleIds(
      execution.cleanedHandleIds,
      execution.kind === 'clean' ? request.metadataHandleIds ?? [] : [],
    )
    if (execution.kind === 'retryable-failure') {
      await this.#persistPartialCleanup(state, execution, cleanedHandleIds)
      return Object.freeze({ kind: 'retryable-failure', state })
    }
    if (execution.kind === 'ownership-unknown') {
      const receipt = await this.#cleanupReceipt(
        state.generation + 1n,
        execution.removedObjectIds,
        execution.removedCheckpointRecordDigests,
      )
      const next = this.#reduce(state, this.#event({
        kind: 'cleanup-unknown',
        lastVerifiedRecordDigest: receipt.digest,
      }, state))
      if (next.kind !== 'needs-attention') {
        throw new TypeError('unknown workspace cleanup did not require attention')
      }
      await this.#repository.commitTransition({
        operationId: this.#intent.operationId,
        expectedLifecycleGeneration: state.generation,
        expectedLeaseId: this.#leaseId,
        records: [await persistedReceiptRecord(receipt)],
        deleteHandleIds: cleanedHandleIds,
        lifecycle: next,
      })
      this.#emit({
        name: 'receive.operation.needs_attention',
        operation_id: this.#intent.operationId,
        prior_state: state.kind,
        needs_attention_reason: 'cleanup-unknown',
      })
      return Object.freeze({ kind: 'needs-attention', receipt, state: next })
    }

    const keep = new Set(keepReceiptDigests)
    const [records, pages] = await Promise.all([
      this.#repository.listRecords(this.#intent.operationId),
      this.#repository.listManifestPages(this.#intent.operationId),
    ])
    const removedRecords = records.filter((record) =>
      record.kind !== RECEIVE_RECORD_OPERATION &&
      record.kind !== RECEIVE_RECORD_LIFECYCLE_STATE &&
      !keep.has(record.digest))
    const receipt = await this.#cleanupReceipt(
      state.generation + 1n,
      execution.removedObjectIds,
      [
        ...removedRecords.map((record) => record.digest),
        ...execution.removedCheckpointRecordDigests,
      ],
    )
    const next = preparationRejectionReason === undefined
      ? this.#reduce(state, this.#event({
          kind: 'cleanup-verified',
          cleanupReceiptDigest: receipt.digest,
        }, state))
      : this.#reduce(state, this.#event({
          kind: 'preparation-rejected',
          reason: preparationRejectionReason,
          cleanupReceiptDigest: receipt.digest,
        }, state))
    await this.#repository.commitTransition({
      operationId: this.#intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.#leaseId,
      records: [await persistedReceiptRecord(receipt)],
      deleteRecordIds: removedRecords.map((record) => record.id),
      deleteManifestPageIds: pages.map((page) => page.id),
      deleteHandleIds: cleanedHandleIds,
      lifecycle: next,
    })
    this.#emit({
      name: next.kind === 'discarded'
        ? 'receive.operation.discarded'
        : 'receive.operation.cleanup_completed',
      operation_id: this.#intent.operationId,
      cleanup_generation: receipt.cleanupGeneration,
      removed_object_count: BigInt(receipt.removedObjectIds.length),
      removed_record_count: BigInt(receipt.removedRecordDigests.length),
    })
    return Object.freeze({ kind: 'clean', receipt, state: next })
  }

  async #persistPartialCleanup(
    state: ReceiveLifecycleState,
    execution: Readonly<{
      removedObjectIds: readonly string[]
      removedCheckpointRecordDigests: readonly string[]
    }>,
    cleanedHandleIds: readonly string[],
  ): Promise<void> {
    if (execution.removedObjectIds.length === 0 &&
        execution.removedCheckpointRecordDigests.length === 0 &&
        cleanedHandleIds.length === 0) return
    const receipt = await this.#cleanupReceipt(
      state.generation,
      execution.removedObjectIds,
      execution.removedCheckpointRecordDigests,
    )
    await this.#repository.commitTransition({
      operationId: this.#intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.#leaseId,
      records: [await persistedReceiptRecord(receipt)],
      deleteHandleIds: cleanedHandleIds,
    })
  }

  #cleanupReceipt(
    cleanupGeneration: bigint,
    removedObjectIds: readonly string[],
    removedRecordDigests: readonly string[],
  ): Promise<CleanupReceiptV1> {
    return createCleanupReceipt({
      operationId: this.#intent.operationId,
      receiveIntentDigest: this.#intent.digest,
      removedObjectIds,
      removedRecordDigests,
      cleanupGeneration,
    })
  }

  async #preparedBudgetWithExactMetadata(input: {
    readonly preparation: SealedWorkspaceZipPreparationV1
    readonly next: ReceiveLifecycleState
    readonly baseMetadataBytes: bigint
  }): Promise<WorkspaceBudgetV1> {
    const base = checkedAdd(
      input.baseMetadataBytes,
      input.preparation.manifest.canonicalMetadataBytes,
    )
    const provisional = await createPreparedZipWorkspaceBudget({
      receiveIntent: this.#intent,
      preparation: input.preparation,
      durableMetadataBytes: base,
    })
    const lifecycleRecord = await storedReceiveLifecycleState(input.next)
    const provisionalReceipt = await createPreparationAdmissionReceipt({
      operationId: this.#intent.operationId,
      receiveIntentDigest: this.#intent.digest,
      preparationManifestDigest: input.preparation.manifest.digest,
      sealedZipLayoutDigest: input.preparation.zipLayout.digest,
      workspaceBudget: provisional,
      contentRequestCountAtAdmission: 0n,
      jobLimitBytes: 0n,
      processLimitBytes: 0n,
      estimatedQuotaBytes: 0n,
      currentUsageBytes: 0n,
      minimumReserveBytes: 0n,
      incrementalPhysicalPeakBytes: 0n,
    })
    return createPreparedZipWorkspaceBudget({
      receiveIntent: this.#intent,
      preparation: input.preparation,
      durableMetadataBytes: checkedAdd(
        base,
        BigInt(provisionalReceipt.canonicalBytes.byteLength),
        BigInt(lifecycleRecord.canonicalBytes.byteLength),
      ),
    })
  }

  async #singleFileBudgetWithExactMetadata(input: {
    readonly fileId: string
    readonly containingDirectoryId: string
    readonly generation: string
    readonly catalogSize: bigint
    readonly durableMetadataBytesExcludingAdmissionRecords: bigint
    readonly next: ReceiveLifecycleState
  }): Promise<WorkspaceBudgetV1> {
    const provisional = await createSingleFileWorkspaceBudget({
      receiveIntent: this.#intent,
      fileId: input.fileId,
      containingDirectoryId: input.containingDirectoryId,
      generation: input.generation,
      catalogSize: input.catalogSize,
      durableMetadataBytes: input.durableMetadataBytesExcludingAdmissionRecords,
    })
    const lifecycleRecord = await storedReceiveLifecycleState(input.next)
    const provisionalReceipt = await createPreparationAdmissionReceipt({
      operationId: this.#intent.operationId,
      receiveIntentDigest: this.#intent.digest,
      workspaceBudget: provisional,
      contentRequestCountAtAdmission: 0n,
      jobLimitBytes: 0n,
      processLimitBytes: 0n,
      estimatedQuotaBytes: 0n,
      currentUsageBytes: 0n,
      minimumReserveBytes: 0n,
      incrementalPhysicalPeakBytes: 0n,
    })
    return createSingleFileWorkspaceBudget({
      receiveIntent: this.#intent,
      fileId: input.fileId,
      containingDirectoryId: input.containingDirectoryId,
      generation: input.generation,
      catalogSize: input.catalogSize,
      durableMetadataBytes: checkedAdd(
        input.durableMetadataBytesExcludingAdmissionRecords,
        BigInt(provisionalReceipt.canonicalBytes.byteLength),
        BigInt(lifecycleRecord.canonicalBytes.byteLength),
      ),
    })
  }

  #preparationAdmissionReceipt(
    budget: WorkspaceBudgetV1,
    capacity: WorkspaceCapacitySnapshot,
    admission: Extract<WorkspaceBudgetAdmission, { kind: 'accepted' }>,
    preparation?: SealedWorkspaceZipPreparationV1,
  ): Promise<PreparationAdmissionReceiptV1> {
    return createPreparationAdmissionReceipt({
      operationId: this.#intent.operationId,
      receiveIntentDigest: this.#intent.digest,
      ...(preparation === undefined
        ? {}
        : {
            preparationManifestDigest: preparation.manifest.digest,
            sealedZipLayoutDigest: preparation.zipLayout.digest,
          }),
      workspaceBudget: budget,
      contentRequestCountAtAdmission: this.#contentRequests.count(),
      jobLimitBytes: capacity.jobLimitBytes,
      processLimitBytes: capacity.processLimitBytes,
      estimatedQuotaBytes: capacity.estimatedQuotaBytes,
      currentUsageBytes: capacity.currentUsageBytes,
      minimumReserveBytes: capacity.minimumReserveBytes,
      incrementalPhysicalPeakBytes: admission.incrementalPhysicalPeakBytes,
    })
  }

  async #lifecycle(): Promise<ReceiveLifecycleState> {
    const record = await this.#repository.readLifecycle(this.#intent.operationId)
    if (record === undefined) throw new TypeError('workspace lifecycle record is missing')
    const state = decodeStoredReceiveLifecycleState(record)
    if (state.receiveIntentDigest !== this.#intent.digest) {
      throw new TypeError('workspace lifecycle escaped its receive intent')
    }
    return state
  }

  #event<T extends Omit<LifecycleEvent, 'expectedGeneration' | 'leaseId'>>(
    event: T,
    state: ReceiveLifecycleState,
  ): T & { readonly expectedGeneration: bigint; readonly leaseId: string } {
    return Object.freeze({
      ...event,
      expectedGeneration: state.generation,
      leaseId: this.#leaseId,
    })
  }

  #reduce(state: ReceiveLifecycleState, event: LifecycleEvent): ReceiveLifecycleState {
    return this.#reduceAt(state, event, this.#now())
  }

  #reduceAt(
    state: ReceiveLifecycleState,
    event: LifecycleEvent,
    nowMilliseconds: number,
  ): ReceiveLifecycleState {
    const reduction = reduceReceiveLifecycle(state, event, {
      planKind: 'workspace-then-publish',
      preparationRequired: this.#intent.plan.preparation === 'exact-zip',
      activeLeaseId: this.#leaseId,
      nowMilliseconds,
    })
    if (reduction.status !== 'applied' || reduction.state === state) {
      throw new TypeError('workspace lifecycle transition was stale or side-effect free')
    }
    return reduction.state
  }

  #now(): number {
    const value = this.#clock()
    if (!Number.isSafeInteger(value) || value < 0) throw new TypeError('workspace clock is invalid')
    return value
  }

  #commitLifecycle(current: ReceiveLifecycleState, next: ReceiveLifecycleState): Promise<void> {
    return this.#repository.commitTransition({
      operationId: this.#intent.operationId,
      expectedLifecycleGeneration: current.generation,
      expectedLeaseId: this.#leaseId,
      lifecycle: next,
    })
  }

  #requireZeroContentRequests(): void {
    if (this.#contentRequests.count() !== 0n) {
      throw new TypeError('workspace content was requested before durable budget admission')
    }
  }

  #requireContinuationUnexpired(state: ReceiveLifecycleState): void {
    const deadline = lifecycleDeadline(state)
    if (deadline !== undefined && this.#now() >= deadline) {
      throw new DOMException(
        'Workspace deadline elapsed; expiry must be reduced before continuation',
        'InvalidStateError',
      )
    }
  }

  #assertRetainedPackage(state: ReceiveLifecycleState, packaged: PackagedArtifactV1): void {
    if (packaged.operationId !== this.#intent.operationId ||
        packaged.receiveIntentDigest !== this.#intent.digest ||
        ((state.kind !== 'waiting-to-save' || state.packageDigest !== packaged.digest) &&
         (state.kind !== 'download-started' || state.attemptKind !== 'workspace' ||
          state.packageDigest !== packaged.digest))) {
      throw new TypeError('publication package is not the retained workspace artifact')
    }
  }

  #assertActiveManagedAttempt(
    state: ReceiveLifecycleState,
    packaged: PackagedArtifactV1,
    attempt: PublicationAttemptV1,
  ): asserts state is Extract<ReceiveLifecycleState, { kind: 'publishing-managed' }> {
    if (state.kind !== 'publishing-managed' ||
        state.publicationAttemptId !== attempt.publicationAttemptId ||
        state.packageDigest !== packaged.digest || attempt.route.kind !== 'managed') {
      throw new TypeError('managed publication observation escaped its active attempt')
    }
  }

  #assertActiveHandoff(
    state: ReceiveLifecycleState,
    packaged: PackagedArtifactV1,
    attempt: PublicationAttemptV1,
  ): asserts state is Extract<
    ReceiveLifecycleState,
    { kind: 'handing-off'; attemptKind: 'workspace' }
  > {
    if (state.kind !== 'handing-off' || state.attemptKind !== 'workspace' ||
        state.attemptId !== attempt.publicationAttemptId ||
        state.packageDigest !== packaged.digest || attempt.route.kind !== 'handoff') {
      throw new TypeError('handoff observation escaped its active attempt')
    }
  }

  async #verifyPackageArtifact(
    manifest: MaterializedManifestV1,
    verification: ArtifactVerificationReceiptV1,
    zipLayout: SealedZipLayoutPlanV1 | undefined,
  ): Promise<void> {
    if (manifest.operationId !== this.#intent.operationId ||
        manifest.receiveIntentDigest !== this.#intent.digest ||
        await canonicalDigest(manifest.canonicalBytes) !== manifest.digest) {
      throw new TypeError('package materialized manifest is not the sealed authority')
    }
    if (this.#intent.artifact.kind === 'zip-archive') {
      if (verification.kind !== 'zip-writer' || zipLayout === undefined) {
        throw new TypeError('ZIP package lacks its writer and layout proof')
      }
      const layout = await validateSealedZipLayoutPlan(zipLayout)
      if (layout.receiveIntentDigest !== this.#intent.digest ||
          layout.artifactDigest !== this.#intent.artifact.digest ||
          layout.evidence.kind !== 'prepared' ||
          manifest.preparationBinding.kind !== 'present' ||
          layout.evidence.preparationManifestDigest !== manifest.preparationBinding.preparationDigest ||
          verification.layoutDigest !== layout.digest ||
          verification.exactBytes !== layout.exactArchiveBytes) {
        throw new TypeError('ZIP package observations disagree with the sealed layout')
      }
      return
    }
    const entry = manifest.entries[0]
    if (verification.kind !== 'original-file-promotion' || zipLayout !== undefined ||
        manifest.entries.length !== 1 || entry?.kind !== 'file' ||
        manifest.preparationBinding.kind !== 'absent' ||
        verification.layoutDigest !== this.#intent.artifact.digest ||
        verification.exactBytes !== entry.exactSize ||
        verification.finalCheckpointDigest !== entry.checkpoint.recordDigest ||
        verification.finalCheckpointGeneration !== entry.checkpoint.checkpointGeneration) {
      throw new TypeError('original-file package is not the sealed raw-object promotion')
    }
  }

  #artifactKind(): 'original-file' | 'zip-archive' {
    if (this.#intent.artifact.kind === 'directory-tree') {
      throw new TypeError('workspace cannot materialize a directory-tree artifact')
    }
    return this.#intent.artifact.kind
  }

  #emitPreparationAccepted(
    preparation: SealedWorkspaceZipPreparationV1 | undefined,
    budget: WorkspaceBudgetV1,
  ): void {
    if (preparation !== undefined) {
      this.#emit({
        name: 'receive.preparation.sealed',
        operation_id: this.#intent.operationId,
        receive_intent_digest: this.#intent.digest,
        preparation_digest: preparation.manifest.digest,
        entry_count: preparation.manifest.entryCount,
        file_count: preparation.manifest.fileCount,
        directory_count: preparation.manifest.directoryCount,
        selected_bytes: preparation.manifest.selectedRawBytes,
        metadata_bytes: preparation.manifest.canonicalMetadataBytes,
      })
    }
    this.#emit({
      name: 'receive.preparation_admission.accepted',
      operation_id: this.#intent.operationId,
      receive_intent_digest: this.#intent.digest,
      plan_kind: 'workspace-then-publish',
      admission_kind: 'workspace-budget',
      artifact_bytes: budget.evidence.kind === 'prepared-zip'
        ? budget.packageBytes
        : budget.uniqueRawBytes,
      metadata_bytes: preparation?.manifest.canonicalMetadataBytes ?? 0n,
      unique_raw_bytes: budget.uniqueRawBytes,
      package_bytes: budget.packageBytes,
      peak_temporary_bytes: budget.peakTemporaryBytes,
      durable_metadata_bytes: budget.durableMetadataBytes,
      peak_owned_bytes: budget.peakOwnedBytes,
      limit_class: 'none',
    })
  }

  #emitAdmissionRejected(
    budget: WorkspaceBudgetV1,
    preparation: SealedWorkspaceZipPreparationV1 | undefined,
    admission: Extract<WorkspaceBudgetAdmission, { kind: 'rejected' }>,
  ): void {
    this.#emit({
      name: 'receive.preparation_admission.rejected',
      operation_id: this.#intent.operationId,
      receive_intent_digest: this.#intent.digest,
      plan_kind: 'workspace-then-publish',
      admission_kind: 'workspace-budget',
      artifact_bytes: budget.evidence.kind === 'prepared-zip'
        ? budget.packageBytes
        : budget.uniqueRawBytes,
      metadata_bytes: preparation?.manifest.canonicalMetadataBytes ?? 0n,
      unique_raw_bytes: budget.uniqueRawBytes,
      package_bytes: budget.packageBytes,
      peak_temporary_bytes: budget.peakTemporaryBytes,
      durable_metadata_bytes: budget.durableMetadataBytes,
      peak_owned_bytes: budget.peakOwnedBytes,
      limit_class: admission.limitClass,
      preparation_admission_reason: admission.reason,
    })
  }

  #emit(event: WorkspaceStageTraceEvent): void {
    try {
      this.#trace?.(Object.freeze(event))
    } catch {
      // Diagnostics cannot roll back or counterfeit a durable ownership decision.
    }
  }
}

export function assertWorkspaceContentGate(
  gate: WorkspaceContentGate,
  expected: {
    readonly operationId: string
    readonly receiveIntentDigest: string
    readonly workspaceBudgetDigest: string
  },
): void {
  if (!CONTENT_GATES.has(gate as object) ||
      gate.operationId !== expected.operationId ||
      gate.receiveIntentDigest !== expected.receiveIntentDigest ||
      gate.workspaceBudgetDigest !== expected.workspaceBudgetDigest) {
    throw new TypeError('workspace content gate was not issued by committed admission')
  }
}

function issueContentGate(input: WorkspaceContentGate): WorkspaceContentGate {
  const gate = Object.freeze({ ...input })
  CONTENT_GATES.add(gate)
  return gate
}

async function requireWorkspaceIntent(
  candidate: ReceiveIntent,
): Promise<ReceiveIntent & {
  readonly plan: Extract<ReceiveIntent['plan'], { kind: 'workspace-then-publish' }>
}> {
  const intent = await validateReceiveIntent(candidate)
  if (intent.plan.kind !== 'workspace-then-publish' || intent.artifact.kind === 'directory-tree') {
    throw new TypeError('workspace stages require a complete-artifact workspace intent')
  }
  return intent as ReceiveIntent & {
    readonly plan: Extract<ReceiveIntent['plan'], { kind: 'workspace-then-publish' }>
  }
}

function checkedAdd(...values: readonly bigint[]): bigint {
  let total = 0n
  for (const value of values) {
    if (typeof value !== 'bigint' || value < 0n) throw new TypeError('metadata byte count is invalid')
    total += value
    if (total > U64_MAXIMUM) throw new RangeError('durable metadata byte count overflow')
  }
  return total
}

function checkedLeaseEnd(startedAt: number): number {
  const duration = Number(BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS)
  if (!Number.isSafeInteger(startedAt) || startedAt < 0 ||
      startedAt > Number.MAX_SAFE_INTEGER - duration) {
    throw new TypeError('handoff URL lease start is invalid')
  }
  return startedAt + duration
}

function mergeHandleIds(...groups: readonly (readonly string[])[]): readonly string[] {
  const values = groups.flatMap((group) => [...group])
  if (values.some((value) => typeof value !== 'string' || value.length === 0) ||
      new Set(values).size !== values.length) {
    throw new TypeError('workspace cleanup handle inventory is invalid')
  }
  values.sort()
  return Object.freeze(values)
}

function stableStateKind(
  state: ReceiveLifecycleState,
): ExpiryReceiptV1['priorStableState'] {
  switch (state.kind) {
    case 'resumable-receive':
    case 'resumable-package':
    case 'waiting-to-save':
      return state.kind
    case 'download-started':
      if (state.attemptKind === 'workspace') return state.kind
      break
  }
  throw new TypeError('workspace state is not durably expirable')
}
