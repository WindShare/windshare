import type { ReceiveIntent } from '../../transfer/intent'
import { IndexedDbFileCheckpointRepository, IndexedDbReceiveOperationRepository } from '../browser/indexeddb-repository'
import { FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE, fileCheckpointIsComplete } from '../persistence/checkpoint'
import type { FileCheckpointJournal, FinalFileCheckpointProof } from '../persistence/journal'
import { durableCheckpointNamespaceIdentity } from '../persistence/namespace'
import { validateWorkspaceBudget, type WorkspaceBudgetV1 } from '../workspace/budget'
import type { ReceiveLifecycleState } from '../workspace/state'
import { readPersistedWorkspaceAdmission } from './reopen/persistence'

const ORIGINAL_CHECKPOINT_AUTHORITY_BOUND = 2

export function originalCheckpointBinding(intent: ReceiveIntent) {
  if (intent.plan.kind !== 'workspace-then-publish' || intent.artifact.kind !== 'original-file') {
    throw new TypeError('Original checkpoint recovery requires its workspace intent')
  }
  return durableCheckpointNamespaceIdentity({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    materializationBindingDigest: intent.plan.workspace.digest,
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE,
    authorityRef: intent.plan.workspace.repositoryRef,
  })
}

/** A single-file intent is exhaustive; a directory transfer cannot infer discovery completion this way. */
export async function readCompletedOriginalFile(
  intent: ReceiveIntent,
  budget: WorkspaceBudgetV1,
  checkpoints: Pick<FileCheckpointJournal, 'scanCommitted' | 'finalCheckpointProof'>,
): Promise<FinalFileCheckpointProof | undefined> {
  originalCheckpointBinding(intent)
  const admitted = await validateWorkspaceBudget(budget, intent)
  if (intent.artifact.kind !== 'original-file' || admitted.evidence.kind !== 'single-file') return undefined
  const page = await checkpoints.scanCommitted({ direction: 'ascending', limit: ORIGINAL_CHECKPOINT_AUTHORITY_BOUND })
  if (page.nextCursor !== undefined || page.records.length !== 1) return undefined
  const record = page.records[0]!
  if (record.fileId !== intent.artifact.fileId || record.exactSize !== admitted.evidence.catalogSize ||
      record.canonicalPath.length !== 1 || record.canonicalPath[0] !== intent.artifact.suggestedName ||
      !fileCheckpointIsComplete(record)) return undefined
  const proof = await checkpoints.finalCheckpointProof(record.recordId, record.checkpointGeneration)
  if (proof.operationId !== intent.operationId || proof.receiveIntentDigest !== intent.digest ||
      proof.materializationBindingDigest !== budget.workspaceBindingDigest) {
    throw new TypeError('Completed original checkpoint escaped its immutable intent')
  }
  return proof
}

export async function readOriginalFileRecoveryRequirement(
  intent: ReceiveIntent,
  lifecycle: ReceiveLifecycleState,
  databaseName?: string,
): Promise<'local-finalization' | undefined> {
  if (intent.plan.kind !== 'workspace-then-publish' || intent.artifact.kind !== 'original-file' ||
      (lifecycle.kind !== 'receiving' &&
       !(lifecycle.kind === 'resumable-receive' && lifecycle.payloadKind === 'file-set'))) return undefined
  const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
  try {
    const admission = await readPersistedWorkspaceAdmission(repository, intent)
    const checkpoints = await IndexedDbFileCheckpointRepository.open(originalCheckpointBinding(intent), databaseName)
    try {
      return await readCompletedOriginalFile(intent, admission.budget, checkpoints) === undefined
        ? undefined : 'local-finalization'
    } finally {
      checkpoints.close()
    }
  } finally {
    repository.close()
  }
}
