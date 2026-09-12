import { IndexedDbReceiveOperationRepository } from '../../../src/output/browser/indexeddb-repository'
import { IndexedDbReceiveResumeSource } from '../../../src/output/browser/indexeddb-resume-state'
import { createBrowserReceiveOperationMutationPort } from '../../../src/output/resume/reopen-authority'
import { forgetReceiveOperationHistory, persistPortableDownloadHistory } from '../../../src/output/resume/operation-history'
import { receiveOperationResumeDescriptor } from '../../../src/output/resume/descriptor'
import { initialReceiveLifecycleState, type ReceiveLifecycleState } from '../../../src/output/workspace/state'
import { createReceiveOperationV2, storedReceiveOperationRecord } from '../../../src/output/workspace/records'
import { storedReceiveLifecycleState } from '../../../src/output/workspace/state-codec'
import { createPortableBinding, createPortableHandoffPlan, createReceiveIntent, deriveArtifactChoiceIdentity } from '../../../src/transfer/intent'
import { listBrowserRetainedOperations } from '../../../src/ui/browser-receive/retained'
import type { BrowserReceiveWindow } from '../../../src/ui/browser-receive/contracts'
import { durableIdentities, durableIntent } from '../durable-output-fixture'
import { presentTask, retainedTaskFacts } from '../../../src/ui/tasks'

const DATABASE_PREFIX = 'windshare-download-history-test-'

export async function seedDownloadHistory(key: string) {
  const repository = await IndexedDbReceiveOperationRepository.open(DATABASE_PREFIX + key)
  try {
    for (let index = 0; index < 2; index += 1) {
      const ids = await durableIdentities(key + '-' + index)
      const workspaceIntent = await durableIntent(ids)
      const portable = await createPortableBinding({
        operationId: ids.operationId, portablePlanId: ids.workspaceId, artifact: workspaceIntent.artifact,
      })
      const plan = await createPortableHandoffPlan(workspaceIntent.artifact, portable)
      const intent = await createReceiveIntent({ selection: workspaceIntent.selection, artifact: workspaceIntent.artifact, plan })
      const lifecycle: ReceiveLifecycleState = Object.freeze({
        kind: 'download-started', attemptKind: 'portable', attemptId: ids.firstPublicationAttemptId,
        operationId: intent.operationId, receiveIntentDigest: intent.digest, generation: 5n,
        timing: { startedAtMilliseconds: 1000 + index, resultReadyAtMilliseconds: 126_000 + index },
      })
      await persistPortableDownloadHistory({ repository, intent, lifecycle,
        display: { objectLabel: 'Holiday photos', destinationLabel: 'Browser downloads', createdAtMilliseconds: 1000 + index } })
    }
  } finally { repository.close() }
}

export async function inspectAndForgetDownloadHistory(key: string) {
  const databaseName = DATABASE_PREFIX + key
  const inventory = await listBrowserRetainedOperations(window as unknown as BrowserReceiveWindow, {
    openResumeSource: () => IndexedDbReceiveResumeSource.open(databaseName),
    resumeMutations: createBrowserReceiveOperationMutationPort({ checkpointDatabaseName: databaseName }),
  }, new AbortController().signal)
  try {
    const rows = inventory.operations
    const first = rows[0]!
    let copiedRejected = false
    try { await inventory.act({ ...first }, 'forget', new AbortController().signal) } catch { copiedRejected = true }
    await inventory.act(first, 'forget', new AbortController().signal)
    const source = await IndexedDbReceiveResumeSource.open(databaseName)
    try {
      return {
        labels: rows.map(row => row.display?.objectLabel),
        destinations: rows.map(row => row.display?.destinationLabel),
        times: rows.map(row => row.display?.createdAtMilliseconds),
        elapsed: rows.map(row => presentTask(retainedTaskFacts(row)).elapsedMilliseconds),
        distinctIdentities: rows[0]!.operationId !== rows[1]!.operationId,
        sameShareIsNotAssumed: rows[0]!.shareInstance !== rows[1]!.shareInstance,
        continuations: rows.map(row => row.continuation),
        actions: rows.map(row => row.actions),
        copiedRejected,
        remaining: (await source.listLifecycleStates()).length,
      }
    } finally { source.close() }
  } finally { inventory.close() }
}

export async function rejectUnfinishedHistoryRemoval(key: string) {
  const repository = await IndexedDbReceiveOperationRepository.open(DATABASE_PREFIX + key)
  const ids = await durableIdentities(key)
  const intent = await durableIntent(ids)
  const choice = await deriveArtifactChoiceIdentity(intent.artifact, intent.plan)
  const operation = await createReceiveOperationV2({ receiveIntent: intent, preClickRanking: [choice.id],
    display: { objectLabel: 'Already saved (untrusted label)', createdAtMilliseconds: 1 } })
  const lifecycle: ReceiveLifecycleState = Object.freeze({
    ...initialReceiveLifecycleState({ operationId: intent.operationId, receiveIntentDigest: intent.digest }),
    kind: 'receiving', activeLeaseId: ids.workspaceId,
  })
  try {
    await repository.commitTransition({ operationId: intent.operationId,
      records: [storedReceiveOperationRecord(operation), await storedReceiveLifecycleState(lifecycle)] })
    const descriptor = receiveOperationResumeDescriptor(lifecycle)!
    let rejected = false
    try { await forgetReceiveOperationHistory(descriptor, repository) } catch { rejected = true }
    return { rejected, retained: (await repository.readLifecycle(intent.operationId)) !== undefined }
  } finally { repository.close() }
}
