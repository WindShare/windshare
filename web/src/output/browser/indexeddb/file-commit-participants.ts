import type { FileCheckpointV2 } from '../../persistence/checkpoint'
import type { MaterializationFinalFileProofV1 } from '../../materialization-ledger/model'

export type IndexedDbFileCommit = Readonly<{
  readonly kind: 'initial-claim'
  readonly checkpoint: FileCheckpointV2
}> | Readonly<{
  readonly kind: 'final-file'
  readonly checkpoint: FileCheckpointV2
  readonly finalProof: MaterializationFinalFileProofV1
}>

/** Participants share the checkpoint's transaction; notifications run only after its commit. */
export interface IndexedDbFileCommitParticipant {
  readonly stores: readonly string[]
  apply(transaction: IDBTransaction, commit: IndexedDbFileCommit): Promise<() => void>
}

export interface IndexedDbFileCommitHost {
  enlistFileCommit(fileId: string, participant: IndexedDbFileCommitParticipant): () => void
}

export class IndexedDbFileCommitParticipants implements IndexedDbFileCommitHost {
  readonly #files = new Map<string, IndexedDbFileCommitParticipant>()

  enlistFileCommit(fileId: string, participant: IndexedDbFileCommitParticipant): () => void {
    if (this.#files.has(fileId)) throw new DOMException('File already has an active commit participant', 'InvalidStateError')
    this.#files.set(fileId, participant)
    return () => { if (this.#files.get(fileId) === participant) this.#files.delete(fileId) }
  }

  prepare(checkpoints: readonly FileCheckpointV2[]) {
    // Snapshot enrollment before opening the transaction so its store scope cannot change mid-cut.
    const entries = new Map(checkpoints.flatMap(checkpoint => {
      const participant = this.#files.get(checkpoint.fileId)
      return participant === undefined ? [] : [[checkpoint.fileId, participant] as const]
    }))
    return {
      stores: [...new Set([...entries.values()].flatMap(participant => participant.stores))],
      apply: async (transaction: IDBTransaction, commits: readonly IndexedDbFileCommit[]): Promise<() => void> => {
        const notifications = await Promise.all(commits.flatMap(commit => {
          const participant = entries.get(commit.checkpoint.fileId)
          return participant === undefined ? [] : [participant.apply(transaction, commit)]
        }))
        return () => { for (const notify of notifications) notify() }
      },
    }
  }
}
