import { encodeBase64Url } from '../../../src/crypto/bytes'
import { IndexedDbReceiveOperationRepository } from '../../../src/output/browser/indexeddb/receive-operation-repository'
import { IndexedDbReceiveResumeSource } from '../../../src/output/browser/indexeddb-resume-state'
import { acquireBrowserReceiveOperationLease } from '../../../src/output/browser/session-lease'
import { openOriginPrivateWorkspaceNamespace } from '../../../src/output/origin-private/namespace'
import { ReceiveOperationResumeAuthority } from '../../../src/output/resume/authority'
import { createBrowserReceiveOperationMutationPort } from '../../../src/output/resume/reopen-authority'
import { decodePreparationAdmissionAuthority } from '../../../src/output/workspace/receipts'
import { RECEIVE_RECORD_RECEIPT } from '../../../src/output/workspace/records'
import type { ReceiveOperationRepository } from '../../../src/output/workspace/repository'
import { WorkspaceOperationStages } from '../../../src/output/workspace/stages'
import {
  createOperationID, createReceiveIntent, createSelectionSpec, createSyntheticSelectionResultRoot,
  createWorkspaceBinding, createWorkspaceID, createWorkspaceThenPublishPlan,
  createZipArchiveArtifact, deriveArtifactChoiceIdentity,
} from '../../../src/transfer/intent'
import { normalizeV2FileTransferFailure } from '../../../src/transfer/job/failures'
import { TransferPauseRequestedError, type WorkspaceExecution } from '../../../src/transfer/output-session'
import { EMPTY_TRANSFER_FAILURE_SUMMARY } from '../../../src/transfer/outcome'
import { WorkspaceReceiveOperation } from '../../../src/ui/browser-receive/workspace-operation'

let runtime: WorkspaceReceiveOperation | undefined
let repository: ReceiveOperationRepository | undefined
let execution: WorkspaceExecution | undefined

export async function create(): Promise<string> {
  const artifact = await createZipArchiveArtifact(createSyntheticSelectionResultRoot())
  const workspace = await createWorkspaceBinding({
    operationId: createOperationID(), workspaceId: createWorkspaceID(), artifact,
    repositoryRef: encodeBase64Url(crypto.getRandomValues(new Uint8Array(32))),
  })
  const intent = await createReceiveIntent({
    selection: await createSelectionSpec({
      shareInstance: encodeBase64Url(crypto.getRandomValues(new Uint8Array(16))),
      syntheticRoot: encodeBase64Url(crypto.getRandomValues(new Uint8Array(16))),
      rules: { mode: 'node-id', defaultSelected: true, rules: [] },
    }),
    artifact, plan: await createWorkspaceThenPublishPlan(artifact, workspace),
  })
  repository = await IndexedDbReceiveOperationRepository.open()
  const namespace = await openOriginPrivateWorkspaceNamespace({
    receiveIntent: intent, repository,
    preClickRanking: [(await deriveArtifactChoiceIdentity(artifact, intent.plan)).id],
  })
  const lease = await acquireBrowserReceiveOperationLease(repository, intent.operationId)
  const stages = await WorkspaceOperationStages.open({
    repository, receiveIntent: intent, leaseId: lease.leaseId,
    clock: Date.now, contentRequests: { count: () => 0n },
  })
  runtime = await WorkspaceReceiveOperation.create({ windowPort: window, intent, repository, namespace, lease, stages })
  return intent.operationId
}

export async function attempt() {
  if (runtime === undefined || repository === undefined) throw new Error('No active startup')
  let failureStage: string | undefined
  let recovery: string | undefined
  try {
    const result = await runtime.admitZip(runtime.intent, new AbortController().signal)
    if (result.kind !== 'accepted') throw new Error('Unexpected capacity rejection')
    execution = result.execution
  } catch (error) {
    const normalized = normalizeV2FileTransferFailure(error)
    if (normalized.kind === 'fault') {
      failureStage = normalized.fact.stage
      recovery = normalized.fact.recoveryDisposition
    }
    await runtime.settleTransferAdmissionFailure(error)
  }
  const records = await repository.listRecords(runtime.intent.operationId, RECEIVE_RECORD_RECEIPT)
  const admissions = await Promise.all(records.map(record => decodePreparationAdmissionAuthority(record, runtime!.intent)))
  return { operationId: runtime.intent.operationId, state: runtime.lifecycle.kind,
    admissions: admissions.filter(value => value !== undefined).length, failureStage, recovery }
}

export async function retry() {
  if (runtime === undefined) throw new Error('No active startup')
  await runtime.startLifecycleAction('continue', runtime.lifecycle)
  return attempt()
}

export async function pause() {
  if (runtime === undefined) throw new Error('No active startup')
  if (execution !== undefined) {
    return execution.pause({
      worker: { status: 'Paused', ...EMPTY_TRANSFER_FAILURE_SUMMARY },
      materialization: { entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n },
      selectionFacts: { discoveredFileCount: 0n, discoveredBytes: 0n, discovery: 'failed' },
      reason: new TransferPauseRequestedError(),
    }, new AbortController().signal)
  }
  const mutation = await runtime.settleTransferAdmissionFailure(new TransferPauseRequestedError())
  return mutation.lifecycle
}

export async function detach(): Promise<void> {
  await runtime?.detach()
  runtime = undefined
  repository = undefined
  execution = undefined
}

export async function inventory() {
  const source = await IndexedDbReceiveResumeSource.open()
  const authority = new ReceiveOperationResumeAuthority({ source, mutations: createBrowserReceiveOperationMutationPort() })
  try {
    const saved = await authority.listResumeState()
    try { return saved.operations.map(operation => operation.descriptor) } finally { saved.close() }
  } finally { source.close() }
}

export async function reopen(operationId: string): Promise<void> {
  const descriptor = (await inventory()).find(candidate => candidate.operationId === operationId)
  if (descriptor === undefined) throw new Error('Startup is missing from production inventory')
  const result = await createBrowserReceiveOperationMutationPort().resume(descriptor)
  if (result.kind !== 'continuation' || result.continuation.kind !== 'workspace-start') {
    throw new Error('Startup did not reopen its authority')
  }
  repository = result.continuation.operation.repository
  runtime = await WorkspaceReceiveOperation.reopenStart({ windowPort: window, operation: result.continuation.operation })
}

export async function discard(operationId: string) {
  if (runtime !== undefined) {
    const result = await runtime.startLifecycleAction('discard', runtime.lifecycle)
    await detach()
    return result.lifecycle.kind
  }
  const descriptor = (await inventory()).find(candidate => candidate.operationId === operationId)
  if (descriptor === undefined) throw new Error('Startup is missing from production inventory')
  return (await createBrowserReceiveOperationMutationPort().discard(descriptor)).kind
}
