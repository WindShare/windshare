import type { ReceiveIntent } from '../../transfer/intent'
import type { OutputDiagnosticsPorts } from '../diagnostics'
import type { OriginPrivateWorkspaceBudgetClaim } from '../origin-private/admission'
import {
  openOriginPrivatePackageContinuationBackend,
  type OriginPrivatePackageContinuationBackend,
} from '../origin-private/session'
import { OriginPrivatePackageWorkflow, type OriginPrivatePackageAttemptResult } from '../origin-private/workflow'
import type { OriginPrivateWorkspaceNamespace } from '../origin-private/namespace'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import {
  decodeSealedMaterializationV1,
  type SealedMaterializationV1,
} from '../workspace/aggregate'
import {
  createMaterializedManifestPages,
  decodeMaterializedManifestV1,
  materializedGenerationTableDigest,
  type MaterializedManifestV1,
} from '../workspace/manifest'
import {
  RECEIVE_RECORD_MATERIALIZED_MANIFEST,
  RECEIVE_RECORD_RECEIPT,
  RECEIVE_RECORD_SEALED_MATERIALIZATION,
  validateManifestPageRecord,
  validatePersistedReceiveRecord,
} from '../workspace/records'
import {
  decodePackageTemporaryCleanupReceipt,
  decodeWorkspaceSealReceipt,
  type PackageTemporaryCleanupReceiptV1,
  type PreparationAdmissionReceiptV1,
} from '../workspace/receipts'
import type { ReceiveOperationRepository } from '../workspace/repository'
import type { ReceiveLifecycleState } from '../workspace/state'
import {
  type AdmittedWorkspaceContent,
  type WorkspaceOperationStages,
} from '../workspace/stages'

export type WorkspacePackageRecoveryLifecycle = Extract<ReceiveLifecycleState, {
  kind: 'resumable-package' | 'materialization-sealed' | 'packaging'
}>

export interface ReopenedWorkspacePackageContinuation {
  readonly sealedMaterialization: SealedMaterializationV1
  readonly materializedManifest: MaterializedManifestV1
  execute(signal: AbortSignal): Promise<OriginPrivatePackageAttemptResult>
}

export type OpenOriginPrivatePackageContinuation =
  typeof openOriginPrivatePackageContinuationBackend

export async function readWorkspacePackageCleanupAuthority(input: {
  readonly repository: ReceiveOperationRepository
  readonly intent: ReceiveIntent
  readonly lifecycle: Extract<ReceiveLifecycleState, { kind: 'resumable-package' }>
}): Promise<PackageTemporaryCleanupReceiptV1> {
  const records = await input.repository.listRecords(input.intent.operationId, RECEIVE_RECORD_RECEIPT)
  const matches: PackageTemporaryCleanupReceiptV1[] = []
  for (const record of records) {
    const receipt = await decodePackageTemporaryCleanupReceipt(record)
    if (receipt?.digest === input.lifecycle.tempCleanupProofDigest) matches.push(receipt)
  }
  if (matches.length !== 1) throw new TypeError('temporary package cleanup authority is ambiguous')
  const receipt = matches[0]!
  if (receipt.operationId !== input.intent.operationId ||
      receipt.receiveIntentDigest !== input.intent.digest ||
      receipt.sealedMaterializationDigest !== input.lifecycle.sealedMaterializationDigest) {
    throw new TypeError('temporary package cleanup escaped its stable lifecycle')
  }
  return receipt
}

export async function reopenWorkspacePackageContinuation(input: {
  readonly repository: ReceiveOperationRepository
  readonly intent: ReceiveIntent
  readonly lifecycle: WorkspacePackageRecoveryLifecycle
  readonly namespace: OriginPrivateWorkspaceNamespace
  readonly stages: WorkspaceOperationStages
  readonly admitted: AdmittedWorkspaceContent
  readonly admissionReceipt: PreparationAdmissionReceiptV1
  readonly cleanupReceipt?: PackageTemporaryCleanupReceiptV1
  readonly checkpointDatabaseName?: string
  readonly openBackend?: OpenOriginPrivatePackageContinuation
  readonly diagnostics?: OutputDiagnosticsPorts
}): Promise<Readonly<{
  backend: OriginPrivatePackageContinuationBackend
  lifecycle: WorkspacePackageRecoveryLifecycle
  continuation: ReopenedWorkspacePackageContinuation
}>> {
  if (input.intent.plan.kind !== 'workspace-then-publish') {
    throw new TypeError('package continuation requires a workspace receive intent')
  }
  const budgetClaim = requireOriginPrivateBudgetClaim(
    input.admitted.claim,
    input.intent.operationId,
  )
  const openBackend = input.openBackend ?? openOriginPrivatePackageContinuationBackend
  const backend = await openBackend({
    receiveIntent: input.intent,
    operationRepository: input.repository,
    namespace: input.namespace,
    contentGate: input.admitted.gate,
    budgetClaim,
    ...(input.checkpointDatabaseName === undefined
      ? {}
      : { checkpointDatabaseName: input.checkpointDatabaseName }),
    ...(input.diagnostics === undefined ? {} : { diagnostics: input.diagnostics }),
  })
  try {
    const seal = await readSealedMaterialization(input)
    const manifest = await readMaterializedManifest(input, backend, seal)
    await validateSealReceiptAndObjects(input, seal, manifest)
    await backend.verifyManifestOwnership(manifest)
    const lifecycle = await recoverPackageCut(input, backend)
    let executed = false
    const workflow = new OriginPrivatePackageWorkflow({
      stages: input.stages,
      store: backend.packages,
      ...(input.diagnostics === undefined ? {} : { diagnostics: input.diagnostics }),
    })
    const continuation: ReopenedWorkspacePackageContinuation = Object.freeze({
      sealedMaterialization: seal,
      materializedManifest: manifest,
      execute: async (signal: AbortSignal) => {
        if (executed) throw new DOMException('Package continuation was already consumed', 'InvalidStateError')
        executed = true
        signal.throwIfAborted()
        if (input.intent.artifact.kind !== 'original-file') {
          throw new TypeError('package continuation artifact authority is invalid')
        }
        return workflow.buildOriginalFile({
          receiveIntentDigest: input.intent.digest,
          artifactSpecDigest: input.intent.artifact.digest,
          sealedMaterialization: seal,
          materializedManifest: manifest,
          signal,
        })
      },
    })
    return Object.freeze({ backend, continuation, lifecycle })
  } catch (error) {
    let cleanupFailed = false
    let cleanupFailure: unknown
    try {
      await backend.close()
    } catch (caughtCleanupFailure) {
      cleanupFailed = true
      cleanupFailure = caughtCleanupFailure
    }
    if (cleanupFailed) {
      throw new AggregateError(
        [error, cleanupFailure],
        'Package continuation failed and its checkpoint authority did not close',
        { cause: error },
      )
    }
    throw error
  }
}

async function recoverPackageCut(
  input: Parameters<typeof reopenWorkspacePackageContinuation>[0],
  backend: OriginPrivatePackageContinuationBackend,
): Promise<WorkspacePackageRecoveryLifecycle> {
  if (input.lifecycle.kind === 'packaging') {
    const temporaryCleanup = await backend.packages.cleanupPackage(input.lifecycle.packageTempObjectId)
    const state = await input.stages.recordRetryablePackageFailure({ reason: 'writer-failed', temporaryCleanup })
    if (state.kind !== 'resumable-package') throw new TypeError('Interrupted package lost its verified cleanup cut')
    return state
  }
  if (input.lifecycle.kind === 'resumable-package') {
    if (input.cleanupReceipt === undefined) throw new TypeError('Package retry lacks temporary cleanup proof')
    await backend.verifyTemporaryCleanup(input.cleanupReceipt)
  }
  return input.lifecycle
}

async function readSealedMaterialization(input: {
  readonly repository: ReceiveOperationRepository
  readonly intent: ReceiveIntent
  readonly lifecycle: WorkspacePackageRecoveryLifecycle
}): Promise<SealedMaterializationV1> {
  const records = await input.repository.listRecords(
    input.intent.operationId,
    RECEIVE_RECORD_SEALED_MATERIALIZATION,
  )
  if (records.length !== 1) throw new TypeError('sealed materialization authority is ambiguous')
  const record = await validatePersistedReceiveRecord(records[0]!)
  const seal = await decodeSealedMaterializationV1(record.canonicalBytes)
  if (record.operationId !== input.intent.operationId || record.digest !== seal.digest ||
      seal.digest !== input.lifecycle.sealedMaterializationDigest ||
      seal.receiveIntentDigest !== input.intent.digest ||
      input.intent.plan.kind !== 'workspace-then-publish' ||
      seal.workspaceBindingDigest !== input.intent.plan.workspace.digest ||
      seal.artifactVersion !== input.intent.artifact.version || seal.layoutVersion !== 1) {
    throw new TypeError('sealed materialization escaped its stable lifecycle')
  }
  return seal
}

async function readMaterializedManifest(
  input: {
    readonly repository: ReceiveOperationRepository
    readonly intent: ReceiveIntent
  },
  backend: OriginPrivatePackageContinuationBackend,
  seal: SealedMaterializationV1,
): Promise<MaterializedManifestV1> {
  const records = await input.repository.listRecords(
    input.intent.operationId,
    RECEIVE_RECORD_MATERIALIZED_MANIFEST,
  )
  if (records.length !== 1) throw new TypeError('materialized manifest authority is ambiguous')
  const record = await validatePersistedReceiveRecord(records[0]!)
  if (input.intent.plan.kind !== 'workspace-then-publish') {
    throw new TypeError('materialized manifest requires a workspace intent')
  }
  const manifest = await decodeMaterializedManifestV1({
    canonicalBytes: record.canonicalBytes,
    operationId: input.intent.operationId,
    receiveIntentDigest: input.intent.digest,
    materializationBindingDigest: input.intent.plan.workspace.digest,
    checkpoints: backend.finalCheckpoints,
  })
  if (record.operationId !== input.intent.operationId || record.digest !== manifest.digest ||
      manifest.digest !== seal.materializedManifestDigest ||
      await materializedGenerationTableDigest(manifest.generations) !== seal.generationTableDigest) {
    throw new TypeError('materialized manifest escaped its seal')
  }
  await readExactManifestPages(
    input.repository,
    input.intent.operationId,
    RECEIVE_RECORD_MATERIALIZED_MANIFEST,
    await createMaterializedManifestPages(manifest),
  )
  return manifest
}

async function validateSealReceiptAndObjects(
  input: {
    readonly repository: ReceiveOperationRepository
    readonly intent: ReceiveIntent
  },
  seal: SealedMaterializationV1,
  manifest: MaterializedManifestV1,
): Promise<void> {
  const records = await input.repository.listRecords(input.intent.operationId, RECEIVE_RECORD_RECEIPT)
  const receipts = []
  for (const record of records) {
    const receipt = await decodeWorkspaceSealReceipt(record)
    if (receipt !== undefined) receipts.push(receipt)
  }
  if (receipts.length !== 1) throw new TypeError('workspace seal receipt authority is ambiguous')
  const receipt = receipts[0]!
  if (input.intent.plan.kind !== 'workspace-then-publish' ||
      receipt.operationId !== input.intent.operationId ||
      receipt.receiveIntentDigest !== input.intent.digest ||
      receipt.workspaceBindingDigest !== input.intent.plan.workspace.digest ||
      receipt.sealedMaterializationDigest !== seal.digest ||
      receipt.rawWorkspaceReceipt.digest !== seal.rawWorkspaceReceiptDigest ||
      receipt.rawWorkspaceReceipt.materializedManifestDigest !== manifest.digest ||
      receipt.rawWorkspaceReceipt.uniqueRawBytes !== manifest.rawBytes) {
    throw new TypeError('workspace seal receipt escaped its materialization')
  }
  const expected = new Map(manifest.entries.map((entry) => [
    entry.ownedObjectId,
    entry.kind === 'file' ? entry.exactSize : 0n,
  ] as const))
  if (expected.size !== manifest.entries.length ||
      receipt.rawWorkspaceReceipt.ownedObjects.length !== expected.size ||
      receipt.rawWorkspaceReceipt.ownedObjects.some((object) =>
        expected.get(object.ownedObjectId) !== object.exactBytes)) {
    throw new TypeError('raw workspace receipt changed its owned object inventory')
  }
}

async function readExactManifestPages(
  repository: ReceiveOperationRepository,
  operationId: string,
  kind: typeof RECEIVE_RECORD_MATERIALIZED_MANIFEST,
  expected: readonly Awaited<ReturnType<typeof validateManifestPageRecord>>[],
): Promise<readonly Awaited<ReturnType<typeof validateManifestPageRecord>>[]> {
  const actual = await Promise.all((await repository.listManifestPages(operationId, kind))
    .map(validateManifestPageRecord))
  if (actual.length !== expected.length || expected.some((page, index) => {
    const candidate = actual[index]
    return candidate === undefined || candidate.id !== page.id || candidate.digest !== page.digest ||
      candidate.ownerDigest !== page.ownerDigest ||
      !sameBytes(candidate.canonicalBytes, page.canonicalBytes)
  })) {
    throw new TypeError('persisted manifest page authority is incomplete or ambiguous')
  }
  return Object.freeze(actual)
}

function requireOriginPrivateBudgetClaim(
  claim: AdmittedWorkspaceContent['claim'],
  operationId: string,
): OriginPrivateWorkspaceBudgetClaim {
  if (!('readmit' in claim) || typeof claim.readmit !== 'function') {
    throw new TargetOwnershipUnknownError('reservation', operationId)
  }
  return claim as OriginPrivateWorkspaceBudgetClaim
}

function sameBytes(left: Uint8Array, right: Uint8Array): boolean {
  return left.byteLength === right.byteLength && left.every((byte, index) => byte === right[index])
}
