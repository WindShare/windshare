import {
  decodePackagedArtifactV1,
  sealPackagedArtifact,
  sealWorkspaceMaterialization,
  type PackagedArtifactV1,
  type SealedMaterializationV1,
} from '../aggregate'
import { canonicalDigest, snapshotIdentity } from '../canonical'
import {
  createMaterializedManifestPages,
  materializedGenerationTableDigest,
  sealMaterializedManifest,
  type AuthenticatedGenerationReference,
  type FinalCheckpointReader,
  type MaterializedManifestEntry,
  type MaterializedManifestV1,
  type PreparationBinding,
} from '../manifest'
import type { SealedWorkspaceZipPreparationV1 } from '../preparation'
import {
  createPackageReceipt,
  createPackageTemporaryCleanupReceipt,
  createRawWorkspaceReceipt,
  createWorkspaceSealReceipt,
  persistedReceiptRecord,
  type ArtifactVerificationReceiptV1,
  type PackageReceiptV1,
  type WorkspaceSealReceiptV1,
  validateArtifactVerificationReceipt,
} from '../receipts'
import {
  createPersistedReceiveRecord,
  RECEIVE_RECORD_MATERIALIZED_MANIFEST,
  RECEIVE_RECORD_PACKAGE,
  RECEIVE_RECORD_SEALED_MATERIALIZATION,
  type ReceiveOperationHandleRecord,
} from '../records'
import type { PackageFailureReason, ReceiveLifecycleState } from '../state'
import {
  validateSealedZipLayoutPlan,
  type SealedZipLayoutPlanV1,
} from '../../zip-layout/layout'
import {
  WORKSPACE_HANDLE_PACKAGE_OBJECT,
  type PackageTemporaryCleanupEvidence,
} from './contracts'
import { WorkspaceStageRuntime } from './runtime'

export class WorkspaceArtifactStages {
  readonly runtime: WorkspaceStageRuntime

  constructor(runtime: WorkspaceStageRuntime) {
    this.runtime = runtime
  }

  async readRetainedPackage(): Promise<PackagedArtifactV1> {
    const state = await this.runtime.lifecycle()
    let packageDigest: string | undefined
    if (state.kind === 'waiting-to-save' ||
        (state.kind === 'download-started' && state.attemptKind === 'workspace')) {
      packageDigest = state.packageDigest
    }
    if (packageDigest === undefined) {
      throw new TypeError('workspace has no retained package authority')
    }
    const records = await this.runtime.repository.listRecords(
      this.runtime.intent.operationId,
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
    if (artifact.operationId !== this.runtime.intent.operationId ||
        artifact.receiveIntentDigest !== this.runtime.intent.digest ||
        artifact.artifactSpecDigest !== this.runtime.intent.artifact.digest) {
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
    const state = await this.runtime.lifecycle()
    if (state.kind !== 'receiving') throw new TypeError('materialization seal requires receiving state')
    const preparationBinding: PreparationBinding = input.preparation === undefined
      ? Object.freeze({ kind: 'absent' })
      : Object.freeze({ kind: 'present', preparationDigest: input.preparation.manifest.digest })
    if ((this.runtime.intent.plan.preparation === 'exact-zip') !== (input.preparation !== undefined)) {
      throw new TypeError('materialization preparation binding disagrees with the receive intent')
    }
    const manifest = await sealMaterializedManifest({
      operationId: this.runtime.intent.operationId,
      receiveIntentDigest: this.runtime.intent.digest,
      materializationBindingDigest: this.runtime.intent.plan.workspace.digest,
      preparationBinding,
      generations: input.generations,
      entries: input.entries,
      checkpoints: input.checkpoints,
      ...(input.preparation === undefined ? {} : { preparation: input.preparation.manifest }),
    })
    const pages = await createMaterializedManifestPages(manifest)
    const rawReceipt = await createRawWorkspaceReceipt({
      operationId: this.runtime.intent.operationId,
      receiveIntentDigest: this.runtime.intent.digest,
      workspaceBindingDigest: this.runtime.intent.plan.workspace.digest,
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
      operationId: this.runtime.intent.operationId,
      receiveIntentDigest: this.runtime.intent.digest,
      workspaceBindingDigest: this.runtime.intent.plan.workspace.digest,
      preparationBinding,
      materializedManifestDigest: manifest.digest,
      generationTableDigest: await materializedGenerationTableDigest(manifest.generations),
      artifactVersion: this.runtime.intent.artifact.version,
      layoutVersion: 1,
      rawWorkspaceReceiptDigest: rawReceipt.digest,
    })
    const receipt = await createWorkspaceSealReceipt({
      operationId: this.runtime.intent.operationId,
      receiveIntentDigest: this.runtime.intent.digest,
      workspaceBindingDigest: this.runtime.intent.plan.workspace.digest,
      sealedMaterializationDigest: seal.digest,
      rawWorkspaceReceipt: rawReceipt,
    })
    const next = this.runtime.reduce(state, this.runtime.event({
      kind: 'materialization-seal-verified',
      sealedMaterializationDigest: seal.digest,
    }, state))
    const records = await Promise.all([
      createPersistedReceiveRecord({
        operationId: this.runtime.intent.operationId,
        kind: RECEIVE_RECORD_MATERIALIZED_MANIFEST,
        canonicalBytes: manifest.canonicalBytes,
      }),
      persistedReceiptRecord(receipt),
      createPersistedReceiveRecord({
        operationId: this.runtime.intent.operationId,
        kind: RECEIVE_RECORD_SEALED_MATERIALIZATION,
        canonicalBytes: seal.canonicalBytes,
      }),
    ])
    await this.runtime.repository.commitTransition({
      operationId: this.runtime.intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.runtime.leaseId,
      records,
      manifestPages: pages,
      lifecycle: next,
    })
    const transferJobId = snapshotIdentity(input.transferJobId, 16, 'transfer job ID')
    this.runtime.emit({
      name: 'receive.materialization.completed',
      operation_id: this.runtime.intent.operationId,
      receive_intent_digest: this.runtime.intent.digest,
      transfer_job_id: transferJobId,
      entry_count: manifest.entryCount,
      file_count: manifest.fileCount,
      directory_count: manifest.directoryCount,
      raw_bytes: manifest.rawBytes,
    })
    this.runtime.emit({
      name: 'receive.materialization.sealed',
      operation_id: this.runtime.intent.operationId,
      receive_intent_digest: this.runtime.intent.digest,
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
    const state = await this.runtime.lifecycle()
    if (state.kind !== 'materialization-sealed' ||
        state.sealedMaterializationDigest !== sealedMaterialization.digest ||
        packageHandle.kind !== WORKSPACE_HANDLE_PACKAGE_OBJECT ||
        packageHandle.operationId !== this.runtime.intent.operationId ||
        packageHandle.ownedObjectId === undefined) {
      throw new TypeError('package allocation escaped its sealed workspace')
    }
    const next = this.runtime.reduce(state, this.runtime.event({
      kind: 'package-started',
      packageTempObjectId: packageHandle.ownedObjectId,
    }, state))
    await this.runtime.repository.commitTransition({
      operationId: this.runtime.intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.runtime.leaseId,
      handles: [packageHandle],
      lifecycle: next,
    })
    this.runtime.emit({
      name: 'receive.package.started',
      operation_id: this.runtime.intent.operationId,
      sealed_materialization_digest: sealedMaterialization.digest,
      artifact_kind: this.#artifactKind(),
    })
    return next
  }

  async recordRetryablePackageFailure(input: {
    readonly reason: PackageFailureReason
    readonly temporaryCleanup: PackageTemporaryCleanupEvidence
  }): Promise<ReceiveLifecycleState> {
    const state = await this.runtime.lifecycle()
    if (state.kind !== 'packaging') throw new TypeError('package failure requires packaging state')
    if (input.temporaryCleanup.operationId !== this.runtime.intent.operationId ||
        input.temporaryCleanup.packageOwnedObjectId !== state.packageTempObjectId) {
      throw new TypeError('temporary package cleanup escaped its active allocation')
    }
    const cleanupReceipt = await createPackageTemporaryCleanupReceipt({
      operationId: this.runtime.intent.operationId,
      receiveIntentDigest: this.runtime.intent.digest,
      sealedMaterializationDigest: state.sealedMaterializationDigest,
      packageOwnedObjectId: input.temporaryCleanup.packageOwnedObjectId,
      packageHandleId: input.temporaryCleanup.packageHandleId,
      cleanupResult: input.temporaryCleanup.result,
      cleanupProofDigest: input.temporaryCleanup.digest,
    })
    const next = this.runtime.reduce(state, this.runtime.event({
      kind: 'package-retryable-failure',
      reason: input.reason,
      tempCleanupProofDigest: cleanupReceipt.digest,
    }, state))
    await this.runtime.repository.commitTransition({
      operationId: this.runtime.intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.runtime.leaseId,
      records: [await persistedReceiptRecord(cleanupReceipt)],
      deleteHandleIds: [cleanupReceipt.packageHandleId],
      lifecycle: next,
    })
    this.runtime.emit({
      name: 'receive.package.retry_started',
      operation_id: this.runtime.intent.operationId,
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
    const state = await this.runtime.lifecycle()
    const verification = await validateArtifactVerificationReceipt(input.artifactVerification)
    if (state.kind !== 'packaging' ||
        state.sealedMaterializationDigest !== input.sealedMaterialization.digest ||
        input.sealedMaterialization.operationId !== this.runtime.intent.operationId ||
        input.sealedMaterialization.receiveIntentDigest !== this.runtime.intent.digest ||
        input.sealedMaterialization.materializedManifestDigest !== input.materializedManifest.digest ||
        verification.operationId !== this.runtime.intent.operationId ||
        verification.receiveIntentDigest !== this.runtime.intent.digest ||
        verification.sealedMaterializationDigest !== input.sealedMaterialization.digest ||
        state.packageTempObjectId !== verification.packageOwnedObjectId) {
      throw new TypeError('package proof escaped its active allocation')
    }
    await this.#verifyPackageArtifact(input.materializedManifest, verification, input.zipLayout)
    const packaged = await sealPackagedArtifact({
      operationId: this.runtime.intent.operationId,
      receiveIntentDigest: this.runtime.intent.digest,
      sealedMaterializationDigest: input.sealedMaterialization.digest,
      artifactSpecDigest: this.runtime.intent.artifact.digest,
      packageOwnedObjectId: verification.packageOwnedObjectId,
      exactBytes: verification.exactBytes,
      artifactReceiptDigest: verification.digest,
      layoutDigest: verification.layoutDigest,
    })
    const receipt = await createPackageReceipt({
      operationId: this.runtime.intent.operationId,
      receiveIntentDigest: this.runtime.intent.digest,
      packagedArtifactDigest: packaged.digest,
      artifactVerification: verification,
    })
    const artifactSealed = this.runtime.reduce(state, this.runtime.event({
      kind: 'package-seal-verified',
      packageDigest: packaged.digest,
    }, state))
    const records = await Promise.all([
      persistedReceiptRecord(receipt),
      createPersistedReceiveRecord({
        operationId: this.runtime.intent.operationId,
        kind: RECEIVE_RECORD_PACKAGE,
        canonicalBytes: packaged.canonicalBytes,
      }),
    ])
    await this.runtime.repository.commitTransition({
      operationId: this.runtime.intent.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: this.runtime.leaseId,
      records,
      lifecycle: artifactSealed,
    })
    this.runtime.emit({
      name: 'receive.package.sealed',
      operation_id: this.runtime.intent.operationId,
      package_digest: packaged.digest,
      layout_digest: packaged.layoutDigest,
      artifact_bytes: packaged.exactBytes,
    })

    const waiting = this.runtime.reduce(artifactSealed, this.runtime.event({
      kind: 'wait-record-persisted',
    }, artifactSealed))
    if (waiting.kind !== 'waiting-to-save') throw new TypeError('package did not enter WaitingToSave')
    await this.runtime.commitLifecycle(artifactSealed, waiting)
    this.runtime.emit({
      name: 'receive.waiting_to_save',
      operation_id: this.runtime.intent.operationId,
      package_digest: packaged.digest,
      expires_at_ms: waiting.expiresAt,
    })
    return Object.freeze({ receipt, package: packaged, state: waiting })
  }



  async #verifyPackageArtifact(
    manifest: MaterializedManifestV1,
    verification: ArtifactVerificationReceiptV1,
    zipLayout: SealedZipLayoutPlanV1 | undefined,
  ): Promise<void> {
    if (manifest.operationId !== this.runtime.intent.operationId ||
        manifest.receiveIntentDigest !== this.runtime.intent.digest ||
        await canonicalDigest(manifest.canonicalBytes) !== manifest.digest) {
      throw new TypeError('package materialized manifest is not the sealed authority')
    }
    if (this.runtime.intent.artifact.kind === 'zip-archive') {
      if (verification.kind !== 'zip-writer' || zipLayout === undefined) {
        throw new TypeError('ZIP package lacks its writer and layout proof')
      }
      const layout = await validateSealedZipLayoutPlan(zipLayout)
      if (layout.receiveIntentDigest !== this.runtime.intent.digest ||
          layout.artifactDigest !== this.runtime.intent.artifact.digest ||
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
        verification.layoutDigest !== this.runtime.intent.artifact.digest ||
        verification.exactBytes !== entry.exactSize ||
        verification.finalCheckpointDigest !== entry.checkpoint.recordDigest ||
        verification.finalCheckpointGeneration !== entry.checkpoint.checkpointGeneration) {
      throw new TypeError('original-file package is not the sealed raw-object promotion')
    }
  }

  #artifactKind(): 'original-file' | 'zip-archive' {
    if (this.runtime.intent.artifact.kind === 'directory-tree') {
      throw new TypeError('workspace cannot materialize a directory-tree artifact')
    }
    return this.runtime.intent.artifact.kind
  }


}
