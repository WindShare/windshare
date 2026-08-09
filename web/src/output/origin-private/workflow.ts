import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import type {
  PackagedArtifactV1,
  SealedMaterializationV1,
} from '../workspace/aggregate'
import type { MaterializedManifestV1 } from '../workspace/manifest'
import type { ArtifactVerificationReceiptV1 } from '../workspace/receipts'
import type { PackageFailureReason, ReceiveLifecycleState } from '../workspace/state'
import type { WorkspaceOperationStages } from '../workspace/stages'
import type { SealedZipLayoutPlanV1 } from '../zip-layout/layout'
import {
  OriginPrivatePackageStore,
  type OriginPrivatePackageAllocation,
} from './package-store'
import {
  OriginPrivateZipPackageBuilder,
  type OriginPrivateZipPackageResult,
} from './zip-exporter'

export type OriginPrivatePackageAttemptResult =
  | Readonly<{
      kind: 'sealed'
      package: PackagedArtifactV1
      state: Extract<ReceiveLifecycleState, { kind: 'waiting-to-save' }>
    }>
  | Readonly<{
      kind: 'cleanup-pending'
      retryCleanup(): Promise<OriginPrivatePackageAttemptResult>
    }>
  | Readonly<{
      kind: 'retryable-failure'
      reason: PackageFailureReason
      state: Extract<ReceiveLifecycleState, { kind: 'resumable-package' }>
    }>
  | Readonly<{
      kind: 'needs-attention'
      state: Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>
    }>

/** Coordinates only package attempts; receive and publication remain independent durable stages. */
export class OriginPrivatePackageWorkflow {
  readonly #stages: WorkspaceOperationStages
  readonly #store: OriginPrivatePackageStore
  readonly #createZipBuilder: () => OriginPrivateZipPackageBuilder

  constructor(input: {
    readonly stages: WorkspaceOperationStages
    readonly store: OriginPrivatePackageStore
    readonly createZipBuilder?: () => OriginPrivateZipPackageBuilder
  }) {
    this.#stages = input.stages
    this.#store = input.store
    this.#createZipBuilder = input.createZipBuilder ?? (() => new OriginPrivateZipPackageBuilder())
  }

  async buildZip(input: {
    readonly receiveIntentDigest: string
    readonly sealedMaterialization: SealedMaterializationV1
    readonly materializedManifest: MaterializedManifestV1
    readonly layout: SealedZipLayoutPlanV1
    readonly signal: AbortSignal
    readonly retry?: boolean
  }): Promise<OriginPrivatePackageAttemptResult> {
    const allocation = await this.#allocateAndStart(input.sealedMaterialization, input.retry === true)
    const builder = this.#createZipBuilder()
    try {
      const result = await builder.build({
        operationId: input.sealedMaterialization.operationId,
        receiveIntentDigest: input.receiveIntentDigest,
        sealedMaterializationDigest: input.sealedMaterialization.digest,
        manifest: input.materializedManifest,
        layout: input.layout,
        packageOwnedObjectId: allocation.ownedObjectId,
        output: await this.#store.openPackageWritable(allocation),
        source: this.#store,
        readPackageExactBytes: () => this.#store.packageExactBytes(allocation.ownedObjectId),
        signal: input.signal,
      })
      return this.#finishZipResult(result, builder, allocation, input)
    } catch (error) {
      return this.#recordFailure(error, allocation, input.sealedMaterialization)
    }
  }

  async buildOriginalFile(input: {
    readonly receiveIntentDigest: string
    readonly artifactSpecDigest: string
    readonly sealedMaterialization: SealedMaterializationV1
    readonly materializedManifest: MaterializedManifestV1
    readonly signal: AbortSignal
    readonly retry?: boolean
  }): Promise<OriginPrivatePackageAttemptResult> {
    const allocation = await this.#allocateAndStart(input.sealedMaterialization, input.retry === true)
    try {
      const verification = await this.#store.promoteOriginalFile({
        receiveIntentDigest: input.receiveIntentDigest,
        sealedMaterializationDigest: input.sealedMaterialization.digest,
        artifactSpecDigest: input.artifactSpecDigest,
        manifest: input.materializedManifest,
        allocation,
        signal: input.signal,
      })
      return this.#seal(
        input.sealedMaterialization,
        input.materializedManifest,
        verification,
      )
    } catch (error) {
      return this.#recordFailure(error, allocation, input.sealedMaterialization)
    }
  }

  async pausePackage(input: {
    readonly allocation: OriginPrivatePackageAllocation
    readonly sealedMaterializationDigest: string
  }): Promise<Extract<ReceiveLifecycleState, { kind: 'resumable-package' | 'needs-attention' }>> {
    try {
      const proof = await this.#store.cleanupPackage(input.allocation.ownedObjectId)
      return this.#stages.pausePackage({
        sealedMaterializationDigest: input.sealedMaterializationDigest,
        temporaryCleanup: proof,
      })
    } catch (error) {
      if (!(error instanceof TargetOwnershipUnknownError)) throw error
      return this.#stages.recordTargetOwnershipUnknown(input.sealedMaterializationDigest)
    }
  }

  async #allocateAndStart(
    sealedMaterialization: SealedMaterializationV1,
    retry: boolean,
  ): Promise<OriginPrivatePackageAllocation> {
    const allocation = await this.#store.allocatePackage()
    try {
      if (retry) {
        await this.#stages.resumePackage(sealedMaterialization, allocation.handleRecord)
      } else {
        await this.#stages.startPackage(sealedMaterialization, allocation.handleRecord)
      }
      return allocation
    } catch (error) {
      await this.#store.cleanupUncommittedPackage(allocation).catch(() => undefined)
      throw error
    }
  }

  async #finishZipResult(
    result: OriginPrivateZipPackageResult,
    builder: OriginPrivateZipPackageBuilder,
    allocation: OriginPrivatePackageAllocation,
    input: {
      readonly sealedMaterialization: SealedMaterializationV1
      readonly materializedManifest: MaterializedManifestV1
      readonly layout: SealedZipLayoutPlanV1
    },
  ): Promise<OriginPrivatePackageAttemptResult> {
    if (result.kind === 'cleanup-pending') {
      return Object.freeze({
        kind: 'cleanup-pending',
        retryCleanup: async () => this.#finishZipResult(
          await builder.retryCleanup(),
          builder,
          allocation,
          input,
        ),
      })
    }
    return this.#seal(
      input.sealedMaterialization,
      input.materializedManifest,
      result.verification,
      input.layout,
    )
  }

  async #seal(
    sealedMaterialization: SealedMaterializationV1,
    materializedManifest: MaterializedManifestV1,
    artifactVerification: ArtifactVerificationReceiptV1,
    zipLayout?: SealedZipLayoutPlanV1,
  ): Promise<OriginPrivatePackageAttemptResult> {
    const sealed = await this.#stages.sealPackage({
      sealedMaterialization,
      materializedManifest,
      artifactVerification,
      ...(zipLayout === undefined ? {} : { zipLayout }),
    })
    return Object.freeze({ kind: 'sealed', package: sealed.package, state: sealed.state })
  }

  async #recordFailure(
    error: unknown,
    allocation: OriginPrivatePackageAllocation,
    seal: SealedMaterializationV1,
  ): Promise<OriginPrivatePackageAttemptResult> {
    if (error instanceof TargetOwnershipUnknownError) {
      return Object.freeze({
        kind: 'needs-attention',
        state: await this.#stages.recordTargetOwnershipUnknown(seal.digest),
      })
    }
    try {
      const proof = await this.#store.cleanupPackage(allocation.ownedObjectId)
      const reason = packageFailureReason(error)
      const state = await this.#stages.recordRetryablePackageFailure({
        reason,
        temporaryCleanup: proof,
      })
      if (state.kind !== 'resumable-package') {
        throw new TypeError('package failure did not enter resumable-package')
      }
      return Object.freeze({ kind: 'retryable-failure', reason, state })
    } catch (cleanupError) {
      if (!(cleanupError instanceof TargetOwnershipUnknownError)) throw cleanupError
      return Object.freeze({
        kind: 'needs-attention',
        state: await this.#stages.recordTargetOwnershipUnknown(seal.digest),
      })
    }
  }
}

function packageFailureReason(error: unknown): PackageFailureReason {
  if (error instanceof DOMException && error.name === 'QuotaExceededError') {
    return 'quota-insufficient'
  }
  if (error instanceof TypeError) return 'layout-mismatch'
  return 'writer-failed'
}
