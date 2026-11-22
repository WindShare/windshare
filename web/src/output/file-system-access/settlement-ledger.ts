import type { ReceiveIntent } from '../../transfer/intent'
import type { PersistentDirectTreeMaterializationEvidence } from '../execution/persistent-execution'
import { retireFSAMaterializationRecoveryMetadata } from './recovery-metadata-retirement'
import { scanAllFSAFileCheckpoints } from './checkpoint-repository'
import type {
  FSAFileCheckpointRepository,
  FSASemanticOutputRepository,
} from './checkpoint-repository'
import { fsaCheckpointSetDigest, type DirectTreeIntent } from './settlement-proof'
import { fileCheckpointIsFinalized, type FileCheckpointV2 } from '../persistence/checkpoint'

export interface FSACheckpointSettlementSnapshot {
  readonly checkpoints: readonly FileCheckpointV2[]
  readonly fileCount: bigint
  readonly completedBytes: bigint
  readonly rootFileFinalized: boolean
}

export interface FSAResumableCheckpointEvidence {
  readonly checkpointCount: bigint
  readonly checkpointSetDigest: string
  readonly checkpoints: readonly FileCheckpointV2[]
}

export class FSASettlementLedgerAuthority {
  readonly #intent: ReceiveIntent
  readonly #checkpoints: FSAFileCheckpointRepository

  constructor(input: Readonly<{
    intent: ReceiveIntent
    checkpoints: FSAFileCheckpointRepository
  }>) {
    this.#intent = input.intent
    this.#checkpoints = input.checkpoints
  }

  async checkpointSettlementSnapshot(
    evidence: PersistentDirectTreeMaterializationEvidence,
  ): Promise<FSACheckpointSettlementSnapshot> {
    if (evidence.kind !== 'direct-tree-checkpoints' ||
        evidence.materializationBindingDigest !== this.#checkpoints.binding.materializationBindingDigest) {
      throw new TypeError('DirectTree evidence does not match this checkpoint namespace')
    }
    const repository = this.#repository()
    const candidateCount = await repository.countCheckpointCandidates()
    if (candidateCount !== 0n) {
      throw new DOMException(
        'Materialization settlement cannot run with unresolved checkpoint candidates',
        'InvalidStateError',
      )
    }
    const checkpoints = await scanAllFSAFileCheckpoints(this.#checkpoints, 'committed')
    let fileCount = 0n
    let completedBytes = 0n
    let rootFileFinalized = false
    for (const checkpoint of checkpoints) {
      if (!fileCheckpointIsFinalized(checkpoint)) continue
      fileCount += 1n
      completedBytes += checkpoint.exactSize
      if (checkpoint.canonicalPath.length === 0) rootFileFinalized = true
    }
    return Object.freeze({
      checkpoints,
      fileCount,
      completedBytes,
      rootFileFinalized,
    })
  }

  async resumableCheckpointEvidence(
    preScanned?: readonly FileCheckpointV2[],
  ): Promise<FSAResumableCheckpointEvidence> {
    const checkpoints = preScanned ?? await scanAllFSAFileCheckpoints(this.#checkpoints, 'committed')
    return Object.freeze({
      checkpointCount: BigInt(checkpoints.length),
      checkpointSetDigest: await fsaCheckpointSetDigest(
        this.#intent as DirectTreeIntent,
        checkpoints,
      ),
      checkpoints,
    })
  }

  async retireRecoveryMetadata(): Promise<void> {
    await retireFSAMaterializationRecoveryMetadata(this.#repository())
  }

  #repository(): FSASemanticOutputRepository {
    const repository = this.#checkpoints as Partial<FSASemanticOutputRepository>
    if (typeof repository.countCheckpointCandidates !== 'function' ||
        typeof repository.retireRecoveryMetadataBatch !== 'function') {
      throw new DOMException(
        'DirectTree settlement requires semantic checkpoint repository authority',
        'InvalidStateError',
      )
    }
    return repository as FSASemanticOutputRepository
  }
}
