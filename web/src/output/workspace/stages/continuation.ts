import type { SealedMaterializationV1 } from '../aggregate'
import {
  createPackageTemporaryCleanupReceipt,
  persistedReceiptRecord,
} from '../receipts'
import type { ReceiveOperationHandleRecord } from '../records'
import type { ReceiveLifecycleState } from '../state'
import {
  WORKSPACE_HANDLE_PACKAGE_OBJECT,
  type PackageTemporaryCleanupEvidence,
} from './contracts'
import { WorkspaceStageRuntime } from './runtime'

export class WorkspaceContinuationStages {
  readonly runtime: WorkspaceStageRuntime

  constructor(runtime: WorkspaceStageRuntime) {
    this.runtime = runtime
  }

  async pauseReceive(input: {
    readonly checkpointSetDigest: string
    readonly completedFileCount: bigint
    readonly completedBytes: bigint
  }): Promise<Extract<ReceiveLifecycleState, { kind: 'resumable-receive' }>> {
    const state = await this.runtime.lifecycle()
    const next = this.runtime.reduce(state, this.runtime.event({
      kind: 'pause-verified',
      stage: 'receive',
      checkpointSetDigest: input.checkpointSetDigest,
      completedFileCount: input.completedFileCount,
      completedBytes: input.completedBytes,
    }, state))
    if (next.kind !== 'resumable-receive') throw new TypeError('workspace pause is not resumable')
    await this.runtime.commitLifecycle(state, next)
    this.runtime.emit({
      name: 'receive.materialization.paused',
      operation_id: this.runtime.intent.operationId,
      receive_intent_digest: this.runtime.intent.digest,
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
    const state = await this.runtime.lifecycle()
    if (state.kind !== 'packaging' ||
        state.sealedMaterializationDigest !== input.sealedMaterializationDigest ||
        input.temporaryCleanup.operationId !== this.runtime.intent.operationId ||
        input.temporaryCleanup.packageOwnedObjectId !== state.packageTempObjectId) {
      throw new TypeError('package pause cleanup escaped its active allocation')
    }
    const cleanupReceipt = await createPackageTemporaryCleanupReceipt({
      operationId: this.runtime.intent.operationId,
      receiveIntentDigest: this.runtime.intent.digest,
      sealedMaterializationDigest: input.sealedMaterializationDigest,
      packageOwnedObjectId: input.temporaryCleanup.packageOwnedObjectId,
      packageHandleId: input.temporaryCleanup.packageHandleId,
      cleanupResult: input.temporaryCleanup.result,
      cleanupProofDigest: input.temporaryCleanup.digest,
    })
    const next = this.runtime.reduce(state, this.runtime.event({
      kind: 'pause-verified',
      stage: 'package',
      sealedMaterializationDigest: input.sealedMaterializationDigest,
      tempCleanupProofDigest: cleanupReceipt.digest,
    }, state))
    if (next.kind !== 'resumable-package') throw new TypeError('package pause is not resumable')
    await this.runtime.repository.commitTransition({
      operationId: this.runtime.intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.runtime.leaseId,
      records: [await persistedReceiptRecord(cleanupReceipt)],
      deleteHandleIds: [cleanupReceipt.packageHandleId],
      lifecycle: next,
    })
    return next
  }

  async resumeReceive(): Promise<Extract<ReceiveLifecycleState, { kind: 'receiving' }>> {
    const state = await this.runtime.lifecycle()
    this.runtime.requireContinuationUnexpired(state)
    const next = this.runtime.reduce(state, this.runtime.event({ kind: 'resume-started' }, state))
    if (next.kind !== 'receiving') throw new TypeError('receive resume entered the wrong stage')
    await this.runtime.commitLifecycle(state, next)
    return next
  }

  async restoreReceiveContinuation(
    fallback: Extract<ReceiveLifecycleState, { kind: 'resumable-receive' }>,
  ): Promise<Extract<ReceiveLifecycleState, { kind: 'resumable-receive' }>> {
    const state = await this.runtime.lifecycle()
    if (state.kind !== 'receiving' || state.generation !== fallback.generation + 1n ||
        fallback.operationId !== this.runtime.intent.operationId ||
        fallback.receiveIntentDigest !== this.runtime.intent.digest) {
      throw new TypeError('receive admission fallback no longer matches the active continuation')
    }
    const next = this.runtime.reduce(state, this.runtime.event({
      kind: 'resume-admission-failed',
      checkpointSetDigest: fallback.checkpointSetDigest,
      completedFileCount: fallback.completedFileCount,
      completedBytes: fallback.completedBytes,
      expiresAt: fallback.expiresAt,
      ...(fallback.partialReceiptDigest === undefined
        ? {}
        : { partialReceiptDigest: fallback.partialReceiptDigest }),
    }, state))
    if (next.kind !== 'resumable-receive') {
      throw new TypeError('receive admission failure did not restore a stable continuation')
    }
    await this.runtime.commitLifecycle(state, next)
    this.runtime.emit({
      name: 'receive.continuation.admission_failed',
      operation_id: this.runtime.intent.operationId,
      receive_intent_digest: this.runtime.intent.digest,
      restored_checkpoint_set_digest: next.checkpointSetDigest,
      restored_completed_file_count: next.completedFileCount,
      restored_completed_bytes: next.completedBytes,
      restored_expires_at_ms: next.expiresAt,
    })
    return next
  }

  async resumePackage(
    sealedMaterialization: SealedMaterializationV1,
    packageHandle: ReceiveOperationHandleRecord,
  ): Promise<Extract<ReceiveLifecycleState, { kind: 'packaging' }>> {
    const state = await this.runtime.lifecycle()
    this.runtime.requireContinuationUnexpired(state)
    if (state.kind !== 'resumable-package' ||
        state.sealedMaterializationDigest !== sealedMaterialization.digest ||
        packageHandle.kind !== WORKSPACE_HANDLE_PACKAGE_OBJECT ||
        packageHandle.operationId !== this.runtime.intent.operationId ||
        packageHandle.ownedObjectId === undefined) {
      throw new TypeError('package resume escaped its sealed workspace')
    }
    const next = this.runtime.reduce(state, this.runtime.event({
      kind: 'resume-started',
      packageTempObjectId: packageHandle.ownedObjectId,
    }, state))
    if (next.kind !== 'packaging') throw new TypeError('package resume entered the wrong stage')
    await this.runtime.repository.commitTransition({
      operationId: this.runtime.intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.runtime.leaseId,
      handles: [packageHandle],
      lifecycle: next,
    })
    return next
  }
}
