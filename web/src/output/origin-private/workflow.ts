import {
  emitOutputTrace,
  outputTraceEvent,
  recordOutputException,
  type OutputDiagnosticsPorts,
} from '../diagnostics'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import type {
  PackagedArtifactV1,
  SealedMaterializationV1,
} from '../workspace/aggregate'
import type { MaterializedManifestV1 } from '../workspace/manifest'
import type { ArtifactVerificationReceiptV1 } from '../workspace/receipts'
import type { ReceiveLifecycleState } from '../workspace/state'
import type { WorkspaceOperationStages } from '../workspace/stages'
import { OriginPrivatePackageStore } from './package-store'

export interface OriginPrivatePackageAttemptResult {
  readonly kind: 'sealed'
  readonly package: PackagedArtifactV1
  readonly state: Extract<ReceiveLifecycleState, { kind: 'waiting-to-save' }>
}

/** Coordinates only package attempts; receive and publication remain independent durable stages. */
export class OriginPrivatePackageWorkflow {
  readonly #stages: WorkspaceOperationStages
  readonly #store: OriginPrivatePackageStore
  readonly #diagnostics: OutputDiagnosticsPorts | undefined

  constructor(input: {
    readonly stages: WorkspaceOperationStages
    readonly store: OriginPrivatePackageStore
    readonly diagnostics?: OutputDiagnosticsPorts
  }) {
    this.#stages = input.stages
    this.#store = input.store
    this.#diagnostics = input.diagnostics
  }

  async buildOriginalFile(input: {
    readonly receiveIntentDigest: string
    readonly artifactSpecDigest: string
    readonly sealedMaterialization: SealedMaterializationV1
    readonly materializedManifest: MaterializedManifestV1
    readonly signal: AbortSignal
  }): Promise<OriginPrivatePackageAttemptResult> {
    try {
      const verification = await this.#store.promoteOriginalFile({
        receiveIntentDigest: input.receiveIntentDigest,
        sealedMaterializationDigest: input.sealedMaterialization.digest,
        artifactSpecDigest: input.artifactSpecDigest,
        manifest: input.materializedManifest,
        signal: input.signal,
      })
      return this.#seal(
        input.sealedMaterialization,
        input.materializedManifest,
        verification,
      )
    } catch (error) {
      this.#recordPackageFailure(error)
      // There is no disposable package allocation: failure must retain the original.
      throw error
    }
  }

  async #seal(
    sealedMaterialization: SealedMaterializationV1,
    materializedManifest: MaterializedManifestV1,
    artifactVerification: ArtifactVerificationReceiptV1,
  ): Promise<OriginPrivatePackageAttemptResult> {
    const sealed = await this.#stages.sealPackage({
      sealedMaterialization,
      materializedManifest,
      artifactVerification,
    })
    return Object.freeze({ kind: 'sealed', package: sealed.package, state: sealed.state })
  }

  #recordPackageFailure(error: unknown): void {
    if (error instanceof TargetOwnershipUnknownError && error.stage === 'cleanup') {
      recordOutputException(
        this.#diagnostics?.failures?.cleanup,
        error,
        { recoveryDisposition: 'needs_attention' },
      )
      this.#traceCleanup('ownership_unknown')
      return
    }
    recordOutputException(
      this.#diagnostics?.failures?.outputWrite,
      error,
      {
        recoveryDisposition: error instanceof TargetOwnershipUnknownError
          ? 'needs_attention'
          : 'resumable_package',
      },
    )
    emitOutputTrace(this.#diagnostics?.trace, () =>
      outputTraceEvent('output_write', {
        backend: 'origin_private',
        transition: 'transaction_failed',
      }))
  }

  #traceCleanup(
    transition: 'ownership_unknown' | 'failed',
  ): void {
    emitOutputTrace(this.#diagnostics?.trace, () =>
      outputTraceEvent('cleanup', {
        backend: 'origin_private',
        transition,
      }))
  }
}
