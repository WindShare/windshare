import { acquireBrowserReceiveOperationLease, type BrowserReceiveOperationLeaseOptions } from '../browser/session-lease'
import { createReceiveOperationV2, storedReceiveOperationRecord, decodeStoredReceiveOperation, RECEIVE_RECORD_OPERATION, operationRecordId } from '../workspace/records'
import { storedReceiveLifecycleState, decodeStoredReceiveLifecycleState } from '../workspace/state-codec'
import type { ReceiveOperationHandleInventoryRepository } from '../workspace/repository'
import type { ReceiveLifecycleState } from '../workspace/state'
import type { ReceiveOperationResumeDescriptor } from './descriptor'
import { deriveArtifactChoiceIdentity, type ArtifactChoiceID, type ReceiveIntent } from '../../transfer/intent'
import type { ReceiveOperationDisplay } from '../workspace/operation-display'
import type { ReceiveOperationRepository } from '../workspace/repository'

/** Portable output has no recoverable payload; only its terminal browser handoff is inventoried. */
export async function persistPortableDownloadHistory(input: {
  repository: ReceiveOperationRepository
  intent: ReceiveIntent
  lifecycle: ReceiveLifecycleState
  display?: ReceiveOperationDisplay
  preClickRanking?: readonly ArtifactChoiceID[]
}): Promise<void> {
  if (input.intent.plan.kind !== 'portable-handoff' ||
      input.lifecycle.kind !== 'download-started' || input.lifecycle.attemptKind !== 'portable' ||
      input.lifecycle.operationId !== input.intent.operationId ||
      input.lifecycle.receiveIntentDigest !== input.intent.digest) {
    throw new TypeError('Portable history requires a matching terminal browser handoff')
  }
  const choice = await deriveArtifactChoiceIdentity(input.intent.artifact, input.intent.plan)
  const operation = await createReceiveOperationV2({
    receiveIntent: input.intent,
    preClickRanking: input.preClickRanking ?? [choice.id],
    ...(input.display === undefined ? {} : { display: input.display }),
  })
  // Both rows appear atomically, so a refresh can never mistake inert portable history for a resumable receive.
  await input.repository.commitTransition({ operationId: input.intent.operationId,
    records: [storedReceiveOperationRecord(operation), await storedReceiveLifecycleState(input.lifecycle)] })
}

export function canForgetReceiveOperationHistory(lifecycle: ReceiveLifecycleState): boolean {
  return (lifecycle.kind === 'published' && lifecycle.cleanupState === 'clean') ||
    (lifecycle.kind === 'download-started' && lifecycle.attemptKind === 'portable')
}

/** Forgetting releases database references only; it has no filesystem or output cleanup port. */
export async function forgetReceiveOperationHistory(
  descriptor: ReceiveOperationResumeDescriptor,
  repository: ReceiveOperationHandleInventoryRepository,
  leaseOptions: Omit<BrowserReceiveOperationLeaseOptions, 'acquireTransition'> = {},
): Promise<void> {
  const lease = await acquireBrowserReceiveOperationLease(repository, descriptor.operationId, leaseOptions)
  try {
    const stored = await repository.readLifecycle(descriptor.operationId)
    if (stored === undefined) throw new DOMException('Download history changed', 'InvalidStateError')
    const lifecycle = decodeStoredReceiveLifecycleState(stored)
    if (lifecycle.generation !== descriptor.lifecycleGeneration ||
        lifecycle.receiveIntentDigest !== descriptor.receiveIntentDigest ||
        !canForgetReceiveOperationHistory(lifecycle)) {
      throw new DOMException('Unfinished output cannot be removed as download history', 'InvalidStateError')
    }
    const record = await repository.readRecord(operationRecordId(descriptor.operationId, RECEIVE_RECORD_OPERATION))
    if (record === undefined) throw new TypeError('Download history has no operation identity')
    const operation = await decodeStoredReceiveOperation(record)
    if (operation.receiveIntentDigest !== lifecycle.receiveIntentDigest ||
        (lifecycle.kind === 'download-started' && operation.receiveIntent.plan.kind !== 'portable-handoff')) {
      throw new TypeError('Download history does not match its operation')
    }
    const [records, pages, handles] = await Promise.all([
      repository.listRecords(descriptor.operationId),
      repository.listManifestPages(descriptor.operationId),
      repository.listHandles(descriptor.operationId),
    ])
    await repository.commitTransition({
      operationId: descriptor.operationId,
      expectedLifecycleGeneration: descriptor.lifecycleGeneration,
      expectedLeaseId: lease.leaseId,
      deleteRecordIds: records.map(value => value.id),
      deleteManifestPageIds: pages.map(value => value.id),
      deleteHandleIds: handles.map(value => value.id),
    })
  } finally {
    await lease.release()
  }
}
