import { openRetainedZipPartialReader } from './partial-zip-continuation'
import { OriginPrivateWorkspaceBudgetAuthority } from '../../origin-private/admission'
import { openOriginPrivateProgressiveZipBackend, progressiveZipObjectRef } from '../../origin-private/progressive-backend'
import { IndexedDbTaskCheckpointStore } from '../../origin-private/task-checkpoint/indexeddb-store'
import { verifyProgressiveZipRecovery, verifyProgressiveZipSelection } from '../progressive-checkpoint'
import { requireOriginPrivateBudgetClaim } from './persistence'
import type { ReopenLifecycleAuthority } from './model'
import type { WorkspaceContinuationAuthorityOptions, WorkspaceContinuationInput } from './workspace-continuation-authority'
import type { WorkspaceOperationStages } from '../../workspace/stages'

export async function reopenProgressiveZipContinuation(
  input: WorkspaceContinuationInput, options: WorkspaceContinuationAuthorityOptions,
  stages: WorkspaceOperationStages, localOnly: boolean, partialExport: boolean,
): Promise<ReopenLifecycleAuthority> {
  const intent = input.snapshot.operation.receiveIntent
  const object = await progressiveZipObjectRef(intent)
  const handle = await input.repository.readHandle(object.handleId)
  if (handle === undefined || handle.operationId !== object.operationId ||
      handle.ownedObjectId !== object.objectId) throw new TypeError('Retained ZIP object handle is missing. Start a new download; retained data has not been changed.')
  const store = await IndexedDbTaskCheckpointStore.open(object, options.checkpointDatabaseName)
  let requirement: Awaited<ReturnType<typeof verifyProgressiveZipRecovery>>['requirement']
  try {
    const verified = await verifyProgressiveZipRecovery(store, object, input.snapshot.lifecycle)
    verifyProgressiveZipSelection(verified.checkpoint, intent)
    requirement = verified.requirement
  } finally { store.close() }
  if (localOnly && requirement !== 'local-finalization') {
    throw new DOMException('ZIP still needs remote content or discovery', 'InvalidStateError')
  }
  if (partialExport) {
    const partialContinuation = await openRetainedZipPartialReader(input, object, options.checkpointDatabaseName)
    input.resources.partialReader = partialContinuation
    return { lifecycle: input.snapshot.lifecycle, stages, partialContinuation }
  }
  // A retained claim is fenced by the newly acquired task lease, even if the old tab crashed.
  const budget = await OriginPrivateWorkspaceBudgetAuthority.open(intent.operationId, {
    estimate: options.estimateWorkspaceStorage, now: options.now,
    ...(options.workspaceBudgetDatabaseName === undefined ? {} : { databaseName: options.workspaceBudgetDatabaseName }),
  })
  const admittedContent = await stages.progressive.admit({ claim: requested => budget.reclaim(requested, input.lease) })
  input.resources.reclaimedClaim = admittedContent.claim
  const backend = await openOriginPrivateProgressiveZipBackend({
    receiveIntent: intent, operationRepository: input.repository, namespace: input.target.namespace,
    contentGate: admittedContent.gate,
    budgetClaim: requireOriginPrivateBudgetClaim(admittedContent.claim, intent.operationId),
    ...(options.checkpointDatabaseName === undefined ? {} : { checkpointDatabaseName: options.checkpointDatabaseName }),
    ...(input.diagnostics === undefined ? {} : { diagnostics: input.diagnostics }),
  })
  input.resources.receiveBackend = backend
  return Object.freeze({
    lifecycle: await stages.progressive.runtime.lifecycle(), stages, admittedContent,
    progressiveContinuation: Object.freeze({ backend, requirement }),
  })
}