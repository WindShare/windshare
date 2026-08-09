import type { ReceiveIntent } from '../../transfer/intent'
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
  canonicalSealedZipLayoutStorageBytes,
  createPreparationManifestPages,
  decodePreparationManifestV1,
  validateWorkspaceZipPreparation,
  type SealedWorkspaceZipPreparationV1,
} from '../workspace/preparation'
import {
  RECEIVE_RECORD_MATERIALIZED_MANIFEST,
  RECEIVE_RECORD_PREPARATION,
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
  WORKSPACE_HANDLE_ZIP_LAYOUT,
  workspaceZipLayoutHandleId,
  type AdmittedWorkspaceContent,
  type WorkspaceOperationStages,
} from '../workspace/stages'

export interface ReopenedWorkspacePackageContinuation {
  readonly sealedMaterialization: SealedMaterializationV1
  readonly materializedManifest: MaterializedManifestV1
  readonly preparation?: SealedWorkspaceZipPreparationV1
  execute(signal: AbortSignal): Promise<OriginPrivatePackageAttemptResult>
}

export type OpenOriginPrivatePackageContinuation =
  typeof openOriginPrivatePackageContinuationBackend

export async function reopenWorkspacePreparationAuthority(input: {
  readonly repository: ReceiveOperationRepository
  readonly intent: ReceiveIntent
  readonly admissionReceipt: PreparationAdmissionReceiptV1
}): Promise<SealedWorkspaceZipPreparationV1 | undefined> {
  if (input.intent.plan.kind !== 'workspace-then-publish') {
    throw new TypeError('workspace preparation reopen requires a workspace intent')
  }
  if (input.intent.plan.preparation !== 'exact-zip') {
    if (input.admissionReceipt.preparationManifestDigest !== undefined ||
        input.admissionReceipt.sealedZipLayoutDigest !== undefined) {
      throw new TypeError('unprepared workspace retained ZIP preparation evidence')
    }
    return undefined
  }
  const manifestDigest = input.admissionReceipt.preparationManifestDigest
  const layoutDigest = input.admissionReceipt.sealedZipLayoutDigest
  if (manifestDigest === undefined || layoutDigest === undefined) {
    throw new TypeError('workspace ZIP admission omitted preparation authority')
  }
  const records = await input.repository.listRecords(
    input.intent.operationId,
    RECEIVE_RECORD_PREPARATION,
  )
  if (records.length !== 1) throw new TypeError('workspace preparation record is ambiguous')
  const record = await validatePersistedReceiveRecord(records[0]!)
  const manifest = await decodePreparationManifestV1(record.canonicalBytes, input.intent)
  if (record.operationId !== input.intent.operationId || record.digest !== manifest.digest ||
      manifest.digest !== manifestDigest) {
    throw new TypeError('workspace preparation record escaped its admission receipt')
  }
  const pages = await readExactManifestPages(
    input.repository,
    input.intent.operationId,
    RECEIVE_RECORD_PREPARATION,
    await createPreparationManifestPages(manifest),
  )
  const handleId = workspaceZipLayoutHandleId(input.intent.operationId, manifest.preparationId)
  const layoutHandle = await input.repository.readHandle(handleId)
  if (layoutHandle === undefined || layoutHandle.id !== handleId ||
      layoutHandle.operationId !== input.intent.operationId ||
      layoutHandle.kind !== WORKSPACE_HANDLE_ZIP_LAYOUT ||
      layoutHandle.authorityRef !== input.intent.plan.workspace.repositoryRef ||
      layoutHandle.ownedObjectId !== undefined || typeof layoutHandle.handle !== 'object' ||
      layoutHandle.handle === null) {
    throw new TypeError('workspace ZIP layout handle authority is missing')
  }
  const zipLayout = layoutHandle.handle as SealedWorkspaceZipPreparationV1['zipLayout']
  if (zipLayout.digest !== layoutDigest) {
    throw new TypeError('workspace ZIP layout escaped its admission receipt')
  }
  return validateWorkspaceZipPreparation({
    manifest,
    pages,
    zipLayout,
    zipLayoutCanonicalBytes: canonicalSealedZipLayoutStorageBytes(zipLayout),
  }, input.intent)
}

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
  readonly lifecycle: Extract<ReceiveLifecycleState, { kind: 'resumable-package' }>
  readonly namespace: OriginPrivateWorkspaceNamespace
  readonly stages: WorkspaceOperationStages
  readonly admitted: AdmittedWorkspaceContent
  readonly admissionReceipt: PreparationAdmissionReceiptV1
  readonly cleanupReceipt: PackageTemporaryCleanupReceiptV1
  readonly checkpointDatabaseName?: string
  readonly openBackend?: OpenOriginPrivatePackageContinuation
}): Promise<Readonly<{
  backend: OriginPrivatePackageContinuationBackend
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
  })
  try {
    const preparation = await reopenWorkspacePreparationAuthority({
      repository: input.repository,
      intent: input.intent,
      admissionReceipt: input.admissionReceipt,
    })
    const seal = await readSealedMaterialization(input)
    const manifest = await readMaterializedManifest(input, backend, seal, preparation)
    await validateSealReceiptAndObjects(input, seal, manifest)
    await backend.verifyManifestOwnership(manifest)
    await backend.verifyTemporaryCleanup(input.cleanupReceipt)
    let executed = false
    const workflow = new OriginPrivatePackageWorkflow({ stages: input.stages, store: backend.packages })
    const continuation: ReopenedWorkspacePackageContinuation = Object.freeze({
      sealedMaterialization: seal,
      materializedManifest: manifest,
      ...(preparation === undefined ? {} : { preparation }),
      execute: async (signal: AbortSignal) => {
        if (executed) throw new DOMException('Package continuation was already consumed', 'InvalidStateError')
        executed = true
        signal.throwIfAborted()
        if (input.intent.artifact.kind === 'zip-archive') {
          if (preparation === undefined) throw new TypeError('ZIP package continuation lacks preparation')
          return workflow.buildZip({
            receiveIntentDigest: input.intent.digest,
            sealedMaterialization: seal,
            materializedManifest: manifest,
            layout: preparation.zipLayout,
            signal,
            retry: true,
          })
        }
        if (input.intent.artifact.kind !== 'original-file' || preparation !== undefined) {
          throw new TypeError('package continuation artifact authority is invalid')
        }
        return workflow.buildOriginalFile({
          receiveIntentDigest: input.intent.digest,
          artifactSpecDigest: input.intent.artifact.digest,
          sealedMaterialization: seal,
          materializedManifest: manifest,
          signal,
          retry: true,
        })
      },
    })
    return Object.freeze({ backend, continuation })
  } catch (error) {
    await backend.close().catch(() => undefined)
    throw error
  }
}

async function readSealedMaterialization(input: {
  readonly repository: ReceiveOperationRepository
  readonly intent: ReceiveIntent
  readonly lifecycle: Extract<ReceiveLifecycleState, { kind: 'resumable-package' }>
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
  preparation: SealedWorkspaceZipPreparationV1 | undefined,
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
    ...(preparation === undefined ? {} : { preparation: preparation.manifest }),
  })
  if (record.operationId !== input.intent.operationId || record.digest !== manifest.digest ||
      manifest.digest !== seal.materializedManifestDigest ||
      manifest.preparationBinding.kind !== seal.preparationBinding.kind ||
      (manifest.preparationBinding.kind === 'present' &&
       (seal.preparationBinding.kind !== 'present' ||
        manifest.preparationBinding.preparationDigest !== seal.preparationBinding.preparationDigest)) ||
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
  kind: typeof RECEIVE_RECORD_PREPARATION | typeof RECEIVE_RECORD_MATERIALIZED_MANIFEST,
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
