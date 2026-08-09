import {
  admitWorkspaceBudget,
  createPreparedZipWorkspaceBudget,
  createSingleFileWorkspaceBudget,
  validateWorkspaceBudget,
  type WorkspaceBudgetAdmission,
  type WorkspaceBudgetV1,
  type WorkspaceCapacitySnapshot,
} from '../budget'
import {
  createPreparationAdmissionReceipt,
  decodePreparationAdmissionReceipt,
  persistedReceiptRecord,
  type PreparationAdmissionReceiptV1,
} from '../receipts'
import {
  createPersistedReceiveRecord,
  receiveOperationHandleRecord,
  RECEIVE_RECORD_RECEIPT,
} from '../records'
import { storedReceiveLifecycleState } from '../state-codec'
import type { PreparationAdmissionReason, ReceiveLifecycleState } from '../state'
import type { SealedWorkspaceZipPreparationV1 } from '../preparation'
import {
  WORKSPACE_HANDLE_ZIP_LAYOUT,
  checkedAdd,
  issueContentGate,
  workspaceZipLayoutHandleId,
  type AdmittedWorkspaceContent,
  type WorkspaceBudgetAuthority,
  type WorkspaceBudgetClaim,
  type WorkspaceCleanupRequest,
} from './contracts'
import { WorkspaceCleanupStages } from './cleanup'
import { WorkspaceStageRuntime } from './runtime'

export class WorkspaceAdmissionStages {
  readonly runtime: WorkspaceStageRuntime
  readonly cleanup: WorkspaceCleanupStages

  constructor(
    runtime: WorkspaceStageRuntime,
    cleanup: WorkspaceCleanupStages,
  ) {
    this.runtime = runtime
    this.cleanup = cleanup
  }

  async beginReceive(preparationId?: string): Promise<ReceiveLifecycleState> {
    const state = await this.runtime.lifecycle()
    const requiresPreparation = this.runtime.intent.plan.preparation === 'exact-zip'
    const event = this.runtime.event({
      kind: 'receive-started',
      ...(preparationId === undefined ? {} : { preparationId }),
    }, state)
    const next = this.runtime.reduce(state, event)
    await this.runtime.commitLifecycle(state, next)
    if (next.kind === 'preparing') {
      this.runtime.emit({
        name: 'receive.preparation.started',
        operation_id: this.runtime.intent.operationId,
        receive_intent_digest: this.runtime.intent.digest,
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
    const state = await this.runtime.lifecycle()
    if (state.kind !== 'preparing') throw new TypeError('preparation admission requires preparing state')
    this.runtime.requireZeroContentRequests()
    const next = this.runtime.reduce(state, this.runtime.event({ kind: 'preparation-admitted' }, state))
    const preparationRecord = await createPersistedReceiveRecord({
      operationId: this.runtime.intent.operationId,
      kind: 4,
      canonicalBytes: input.preparation.manifest.canonicalBytes,
    })
    const layoutHandle = receiveOperationHandleRecord({
      id: workspaceZipLayoutHandleId(
        this.runtime.intent.operationId,
        input.preparation.manifest.preparationId,
      ),
      operationId: this.runtime.intent.operationId,
      kind: WORKSPACE_HANDLE_ZIP_LAYOUT,
      authorityRef: this.runtime.intent.plan.workspace.repositoryRef,
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
      const cleanup = await this.cleanup.finishCleanup(
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
    this.runtime.requireZeroContentRequests()
    try {
      await this.runtime.repository.commitTransition({
        operationId: this.runtime.intent.operationId,
        expectedLifecycleGeneration: state.generation,
        expectedLeaseId: this.runtime.leaseId,
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
      operationId: this.runtime.intent.operationId,
      receiveIntentDigest: this.runtime.intent.digest,
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
    const state = await this.runtime.lifecycle()
    if (state.kind !== 'intent-frozen') {
      throw new TypeError('single-file admission requires frozen intent state')
    }
    this.runtime.requireZeroContentRequests()
    const next = this.runtime.reduce(state, this.runtime.event({ kind: 'receive-started' }, state))
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
      const cleanup = await this.cleanup.finishCleanup(state, input.rejectionCleanup, [])
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
    this.runtime.requireZeroContentRequests()
    try {
      await this.runtime.repository.commitTransition({
        operationId: this.runtime.intent.operationId,
        expectedLifecycleGeneration: state.generation,
        expectedLeaseId: this.runtime.leaseId,
        records: [await persistedReceiptRecord(receipt)],
        lifecycle: next,
      })
    } catch (error) {
      await claimResult.claim.release().catch(() => undefined)
      throw error
    }
    this.#emitPreparationAccepted(undefined, budget)
    const gate = issueContentGate({
      operationId: this.runtime.intent.operationId,
      receiveIntentDigest: this.runtime.intent.digest,
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
    const state = await this.runtime.lifecycle()
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
    const state = await this.runtime.lifecycle()
    if (state.kind !== 'resumable-package') {
      throw new TypeError('workspace package authority requires resumable-package state')
    }
    this.runtime.requireContinuationUnexpired(state)
    return this.#reopenAdmittedWorkspaceAuthority(input)
  }

  async #reopenAdmittedWorkspaceAuthority(input: {
    readonly budget: WorkspaceBudgetV1
    readonly claim: WorkspaceBudgetClaim
  }): Promise<AdmittedWorkspaceContent> {
    try {
      this.runtime.requireZeroContentRequests()
      const budget = await validateWorkspaceBudget(input.budget, this.runtime.intent)
      const expectedAdmission = admitWorkspaceBudget(budget, input.claim.capacity)
      if (expectedAdmission.kind !== 'accepted' ||
          input.claim.budgetDigest !== budget.digest ||
          input.claim.admission.kind !== 'accepted' ||
          input.claim.admission.budgetDigest !== budget.digest ||
          input.claim.admission.incrementalPhysicalPeakBytes !==
            expectedAdmission.incrementalPhysicalPeakBytes) {
        throw new TypeError('recovered workspace budget claim is invalid')
      }
      const receiptRecords = await this.runtime.repository.listRecords(
        this.runtime.intent.operationId,
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
      this.runtime.requireZeroContentRequests()
      const gate = issueContentGate({
        operationId: this.runtime.intent.operationId,
        receiveIntentDigest: this.runtime.intent.digest,
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
      receiveIntent: this.runtime.intent,
      preparation: input.preparation,
      durableMetadataBytes: base,
    })
    const lifecycleRecord = await storedReceiveLifecycleState(input.next)
    const provisionalReceipt = await createPreparationAdmissionReceipt({
      operationId: this.runtime.intent.operationId,
      receiveIntentDigest: this.runtime.intent.digest,
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
      receiveIntent: this.runtime.intent,
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
      receiveIntent: this.runtime.intent,
      fileId: input.fileId,
      containingDirectoryId: input.containingDirectoryId,
      generation: input.generation,
      catalogSize: input.catalogSize,
      durableMetadataBytes: input.durableMetadataBytesExcludingAdmissionRecords,
    })
    const lifecycleRecord = await storedReceiveLifecycleState(input.next)
    const provisionalReceipt = await createPreparationAdmissionReceipt({
      operationId: this.runtime.intent.operationId,
      receiveIntentDigest: this.runtime.intent.digest,
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
      receiveIntent: this.runtime.intent,
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
      operationId: this.runtime.intent.operationId,
      receiveIntentDigest: this.runtime.intent.digest,
      ...(preparation === undefined
        ? {}
        : {
            preparationManifestDigest: preparation.manifest.digest,
            sealedZipLayoutDigest: preparation.zipLayout.digest,
          }),
      workspaceBudget: budget,
      contentRequestCountAtAdmission: this.runtime.contentRequests.count(),
      jobLimitBytes: capacity.jobLimitBytes,
      processLimitBytes: capacity.processLimitBytes,
      estimatedQuotaBytes: capacity.estimatedQuotaBytes,
      currentUsageBytes: capacity.currentUsageBytes,
      minimumReserveBytes: capacity.minimumReserveBytes,
      incrementalPhysicalPeakBytes: admission.incrementalPhysicalPeakBytes,
    })
  }



  #emitPreparationAccepted(
    preparation: SealedWorkspaceZipPreparationV1 | undefined,
    budget: WorkspaceBudgetV1,
  ): void {
    if (preparation !== undefined) {
      this.runtime.emit({
        name: 'receive.preparation.sealed',
        operation_id: this.runtime.intent.operationId,
        receive_intent_digest: this.runtime.intent.digest,
        preparation_digest: preparation.manifest.digest,
        entry_count: preparation.manifest.entryCount,
        file_count: preparation.manifest.fileCount,
        directory_count: preparation.manifest.directoryCount,
        selected_bytes: preparation.manifest.selectedRawBytes,
        metadata_bytes: preparation.manifest.canonicalMetadataBytes,
      })
    }
    this.runtime.emit({
      name: 'receive.preparation_admission.accepted',
      operation_id: this.runtime.intent.operationId,
      receive_intent_digest: this.runtime.intent.digest,
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
    this.runtime.emit({
      name: 'receive.preparation_admission.rejected',
      operation_id: this.runtime.intent.operationId,
      receive_intent_digest: this.runtime.intent.digest,
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


}
