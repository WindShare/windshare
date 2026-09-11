import type { DirectZipJournalRepository } from '../direct-zip/journal/repository'
import type { ReceiveLifecycleState } from '../workspace/state'

export type DirectZipRecoveryRequirement = 'receive' | 'verify-completion' | 'published'

/** A final candidate authorizes local verification; its existence does not prove publication. */
export async function readDirectZipRecoveryRequirement(
  lifecycle: ReceiveLifecycleState,
  journal: Pick<DirectZipJournalRepository, 'readState' | 'readOperationCandidate'>,
): Promise<DirectZipRecoveryRequirement | undefined> {
  if (lifecycle.kind !== 'receiving' && lifecycle.kind !== 'resumable-receive' &&
      lifecycle.kind !== 'authorization-required' && lifecycle.kind !== 'target-verification-required' &&
      lifecycle.kind !== 'destination-space-required' && lifecycle.kind !== 'published' &&
      !(lifecycle.kind === 'needs-attention' && lifecycle.reason === 'publication-unknown')) return undefined
  const state = await journal.readState(lifecycle.operationId)
  if (state === undefined) return undefined
  if (state.checkpoint.receiveIntentDigest !== lifecycle.receiveIntentDigest) {
    throw new TypeError('Direct ZIP recovery checkpoint belongs to another receive intent')
  }
  if (lifecycle.kind === 'published') return lifecycle.cleanupState === 'clean' ? 'published' : undefined
  const candidate = await journal.readOperationCandidate(lifecycle.operationId)
  if (candidate !== undefined && (candidate.kind === 'bootstrap' ||
      candidate.predecessorCheckpointDigest !== state.checkpointDigest)) {
    throw new TypeError('Direct ZIP recovery candidate lost its committed predecessor')
  }
  if (state.checkpoint.closingReplay?.completion !== undefined || candidate?.kind === 'closing') {
    return 'verify-completion'
  }
  return lifecycle.kind === 'receiving' || lifecycle.kind === 'resumable-receive' ? 'receive' : undefined
}
