import { createProgressiveZipWorkspaceBudget } from '../budget'
import { canonicalDigest, canonicalFrame, canonicalRecord, canonicalText } from '../canonical'
import { sealPackagedArtifact } from '../aggregate'
import {
  createPreparationAdmissionReceipt, createZipArtifactVerificationReceipt,
  createPackageReceipt, persistedReceiptRecord,
} from '../receipts'
import { createPersistedReceiveRecord, RECEIVE_RECORD_PACKAGE } from '../records'
import { nextReceiveLifecycleState, type ReceiveLifecycleState } from '../state'
import { taskEntryComplete, type TaskCheckpoint } from '../../origin-private/task-checkpoint/model'
import type { TaskCheckpointStore } from '../../origin-private/task-checkpoint/store'
import { issueContentGate, type AdmittedWorkspaceContent, type WorkspaceBudgetAuthority } from './contracts'
import type { WorkspaceStageRuntime } from './runtime'

const PROGRESSIVE_METADATA_HEADROOM_BYTES = 1024n * 1024n
const ENTRY_PAGE_SIZE = 128

/** Native ZIP checkpoint proof replaces receive-then-package manifests and temporary objects. */
export class WorkspaceProgressiveStages {
  readonly runtime: WorkspaceStageRuntime

  constructor(runtime: WorkspaceStageRuntime) { this.runtime = runtime }

  async admit(authority: WorkspaceBudgetAuthority): Promise<AdmittedWorkspaceContent> {
    const state = await this.runtime.lifecycle()
    requireReceivingState(state, true)
    if (this.runtime.intent.artifact.kind !== 'zip-archive' ||
        this.runtime.intent.plan.preparation !== 'none') throw new TypeError('Progressive admission needs a native ZIP intent')
    const budget = await createProgressiveZipWorkspaceBudget({
      receiveIntent: this.runtime.intent, durableMetadataBytes: PROGRESSIVE_METADATA_HEADROOM_BYTES,
    })
    const result = await authority.claim(budget)
    if (result.kind === 'rejected') throw new DOMException('Browser storage cannot admit ZIP metadata', 'QuotaExceededError')
    const claim = result.claim
    try {
      const receipt = await createPreparationAdmissionReceipt({
        operationId: this.runtime.intent.operationId, receiveIntentDigest: this.runtime.intent.digest,
        workspaceBudget: budget, contentRequestCountAtAdmission: 0n,
        estimatedQuotaBytes: claim.capacity.estimatedQuotaBytes,
        currentUsageBytes: claim.capacity.currentUsageBytes, minimumReserveBytes: claim.capacity.minimumReserveBytes,
        incrementalPhysicalPeakBytes: claim.admission.incrementalPhysicalPeakBytes,
      })
      const receiving = nextReceiveLifecycleState(state, { kind: 'receiving', activeLeaseId: this.runtime.leaseId })
      await this.runtime.repository.commitTransition({
        operationId: this.runtime.intent.operationId, expectedLifecycleGeneration: state.generation,
        expectedLeaseId: this.runtime.leaseId, records: [await persistedReceiptRecord(receipt)], lifecycle: receiving,
      })
      this.runtime.emit({
        name: 'receive.preparation_admission.accepted', operation_id: this.runtime.intent.operationId,
        receive_intent_digest: this.runtime.intent.digest, plan_kind: 'workspace-then-publish',
        admission_kind: 'workspace-budget', artifact_bytes: 0n, metadata_bytes: budget.durableMetadataBytes,
        unique_raw_bytes: 0n,
        durable_metadata_bytes: budget.durableMetadataBytes, peak_owned_bytes: budget.peakOwnedBytes, limit_class: 'none',
      })
      return Object.freeze({
        budget, admissionReceipt: receipt, claim,
        gate: issueContentGate({
          operationId: this.runtime.intent.operationId, receiveIntentDigest: this.runtime.intent.digest,
          workspaceBudgetDigest: budget.digest,
        }),
      })
    } catch (error) {
      await claim.release().catch(() => undefined)
      throw error
    }
  }

  async pause(store: TaskCheckpointStore, checkpoint: TaskCheckpoint, pauseReason?: 'storage-pressure'): Promise<ReceiveLifecycleState> {
    await verifyCurrentCheckpoint(store, checkpoint, this.runtime.intent.operationId)
    const progress = await checkpointProgress(store)
    const state = await this.runtime.lifecycle()
    requireReceivingState(state)
    const next = nextReceiveLifecycleState(state, {
      kind: 'resumable-receive', payloadKind: 'opfs-zip',
      objectId: checkpoint.object.objectId, checkpointGeneration: checkpoint.generation,
      completedFileCount: progress.completedFileCount, completedBytes: progress.completedBytes,
      discoveryComplete: checkpoint.discoveryComplete, occupiedBytes: checkpoint.physicalLength,
      ...(pauseReason === undefined ? {} : { pauseReason }),
    })
    await this.runtime.commitLifecycle(state, next)
    this.runtime.emit({
      name: 'receive.materialization.paused', operation_id: this.runtime.intent.operationId,
      receive_intent_digest: this.runtime.intent.digest, resumable_stage: 'receive',
      completed_file_count: progress.completedFileCount, completed_bytes: progress.completedBytes,
    })
    return next
  }

  async seal(store: TaskCheckpointStore, checkpoint: TaskCheckpoint): Promise<ReceiveLifecycleState> {
    await verifyCurrentCheckpoint(store, checkpoint, this.runtime.intent.operationId)
    if (checkpoint.artifactState !== 'sealed' || checkpoint.sealedLength === undefined ||
        !checkpoint.discoveryComplete) throw new TypeError('ZIP artifact lacks a durable finalization seal')
    const progress = await checkpointProgress(store)
    if (!progress.complete || progress.entryCount !== checkpoint.entryCount) {
      throw new TypeError('ZIP artifact has incomplete selected content')
    }
    const state = await this.runtime.lifecycle()
    requireReceivingState(state)
    const digest = await checkpointDigest(checkpoint)
    const verification = await createZipArtifactVerificationReceipt({
      operationId: this.runtime.intent.operationId, receiveIntentDigest: this.runtime.intent.digest,
      sealedMaterializationDigest: digest, layoutDigest: digest,
      packageOwnedObjectId: checkpoint.object.objectId, exactBytes: checkpoint.sealedLength,
      writerCloseVerified: true,
    })
    const artifact = await sealPackagedArtifact({
      operationId: this.runtime.intent.operationId, receiveIntentDigest: this.runtime.intent.digest,
      sealedMaterializationDigest: digest, artifactSpecDigest: this.runtime.intent.artifact.digest,
      packageOwnedObjectId: checkpoint.object.objectId, exactBytes: checkpoint.sealedLength,
      artifactReceiptDigest: verification.digest, layoutDigest: digest,
    })
    const receipt = await createPackageReceipt({
      operationId: this.runtime.intent.operationId, receiveIntentDigest: this.runtime.intent.digest,
      packagedArtifactDigest: artifact.digest, artifactVerification: verification,
    })
    // The format authority has already sealed the sole object; no packaging mutation exists.
    const next = nextReceiveLifecycleState(state, { kind: 'waiting-to-save', packageDigest: artifact.digest })
    await this.runtime.repository.commitTransition({
      operationId: this.runtime.intent.operationId, expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.runtime.leaseId, lifecycle: next,
      records: [
        await persistedReceiptRecord(receipt),
        await createPersistedReceiveRecord({
          operationId: this.runtime.intent.operationId, kind: RECEIVE_RECORD_PACKAGE,
          canonicalBytes: artifact.canonicalBytes,
        }),
      ],
    })
    this.runtime.emit({
      name: 'receive.package.sealed', operation_id: this.runtime.intent.operationId,
      package_digest: artifact.digest, layout_digest: artifact.layoutDigest, artifact_bytes: artifact.exactBytes,
    })
    this.runtime.emit({
      name: 'receive.waiting_to_save', operation_id: this.runtime.intent.operationId, package_digest: artifact.digest,
    })
    return next
  }
}

function requireReceivingState(state: ReceiveLifecycleState, initial = false): void {
  if (state.kind === 'receiving' || (initial && state.kind === 'intent-frozen') ||
      (state.kind === 'resumable-receive' && state.payloadKind === 'opfs-zip')) return
  throw new TypeError('Native ZIP stage cannot replace a completed or foreign lifecycle')
}

async function checkpointProgress(store: TaskCheckpointStore): Promise<{
  complete: boolean; entryCount: bigint; completedFileCount: bigint; completedBytes: bigint
}> {
  let afterSequence: bigint | undefined
  let complete = true
  let entryCount = 0n
  let completedFileCount = 0n
  let completedBytes = 0n
  for (;;) {
    const entries = await store.readEntries({ ...(afterSequence === undefined ? {} : { afterSequence }), limit: ENTRY_PAGE_SIZE })
    if (entries.length === 0) break
    for (const entry of entries) {
      entryCount++
      const received = taskEntryComplete(entry)
      complete &&= received
      if (received && entry.kind === 'file') {
        completedFileCount++
        completedBytes += entry.revision!.exactSize
      }
      afterSequence = entry.zipLayout!.sequence
    }
  }
  return { complete, entryCount, completedFileCount, completedBytes }
}

async function verifyCurrentCheckpoint(store: TaskCheckpointStore, checkpoint: TaskCheckpoint, operationId: string): Promise<void> {
  const current = await store.readCheckpoint()
  if (current === undefined || current.object.operationId !== operationId ||
      current.generation !== checkpoint.generation ||
      await checkpointDigest(current) !== await checkpointDigest(checkpoint)) {
    throw new TypeError('ZIP settlement has no matching committed checkpoint')
  }
}

function checkpointDigest(checkpoint: TaskCheckpoint): Promise<string> {
  const text = JSON.stringify(checkpoint, (_, value: unknown) => typeof value === 'bigint' ? value.toString() : value)
  return canonicalDigest(canonicalRecord('windshare/opfs-zip-checkpoint-seal/v1', 1, [
    canonicalFrame(canonicalText(text)),
  ]))
}
