import type { ReceiveIntent } from '../../transfer/intent'
import type { PersistentDirectTreeMaterializationEvidence } from '../execution/persistent-execution'
import { retireFSAMaterializationRecoveryMetadata } from './recovery-metadata-retirement'
import { forEachFSAFileCheckpointPage } from './checkpoint-repository'
import type {
  FSAFileCheckpointRepository,
  FSASemanticOutputRepository,
} from './checkpoint-repository'
import { fsaCheckpointSetDigest, type DirectTreeIntent } from './settlement-proof'
import { fileCheckpointIsFinalized, type FileCheckpointV2 } from '../persistence/checkpoint'

export interface FSACheckpointSettlementSnapshot {
  readonly fileCount: bigint
  readonly completedBytes: bigint
  readonly rootFileFinalized: boolean
}

export interface FSAResumableCheckpointEvidence {
  readonly checkpointCount: bigint
  readonly checkpointSetDigest: string
  readonly checkpoints: readonly FileCheckpointV2[]
}

export interface FSAResumableCheckpointSettlement {
  readonly snapshot: FSACheckpointSettlementSnapshot
  readonly evidence: FSAResumableCheckpointEvidence
}

interface SettlementSnapshotAccumulator {
  fileCount: bigint
  completedBytes: bigint
  rootFileFinalized: boolean
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
    await this.#requireSettledCandidates(evidence)
    const accumulator = createSettlementSnapshotAccumulator()
    // Terminal settlement only needs scalar totals; folding per page avoids retaining all checkpoints.
    await forEachFSAFileCheckpointPage(this.#checkpoints, 'committed', records => {
      accumulateSettlementSnapshot(accumulator, records)
    })
    return freezeSettlementSnapshot(accumulator)
  }

  async resumableCheckpointSettlement(
    evidence: PersistentDirectTreeMaterializationEvidence,
  ): Promise<FSAResumableCheckpointSettlement> {
    await this.#requireSettledCandidates(evidence)
    const accumulator = createSettlementSnapshotAccumulator()
    const checkpoints: FileCheckpointV2[] = []
    // Pause needs both settlement totals and the committed set for digest and recovery summary in one scan.
    await forEachFSAFileCheckpointPage(this.#checkpoints, 'committed', records => {
      accumulateSettlementSnapshot(accumulator, records)
      checkpoints.push(...records)
    })
    const frozenCheckpoints = Object.freeze(checkpoints)
    return Object.freeze({
      snapshot: freezeSettlementSnapshot(accumulator),
      evidence: Object.freeze({
        checkpointCount: BigInt(frozenCheckpoints.length),
        checkpointSetDigest: await fsaCheckpointSetDigest(
          this.#intent as DirectTreeIntent,
          frozenCheckpoints,
        ),
        checkpoints: frozenCheckpoints,
      }),
    })
  }

  async retireRecoveryMetadata(): Promise<void> {
    await retireFSAMaterializationRecoveryMetadata(this.#repository())
  }

  async #requireSettledCandidates(
    evidence: PersistentDirectTreeMaterializationEvidence,
  ): Promise<void> {
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

function createSettlementSnapshotAccumulator(): SettlementSnapshotAccumulator {
  return {
    fileCount: 0n,
    completedBytes: 0n,
    rootFileFinalized: false,
  }
}

function accumulateSettlementSnapshot(
  accumulator: SettlementSnapshotAccumulator,
  records: readonly FileCheckpointV2[],
): void {
  for (const checkpoint of records) {
    if (!fileCheckpointIsFinalized(checkpoint)) continue
    accumulator.fileCount += 1n
    accumulator.completedBytes += checkpoint.exactSize
    if (checkpoint.canonicalPath.length === 0) accumulator.rootFileFinalized = true
  }
}

function freezeSettlementSnapshot(
  accumulator: SettlementSnapshotAccumulator,
): FSACheckpointSettlementSnapshot {
  return Object.freeze({
    fileCount: accumulator.fileCount,
    completedBytes: accumulator.completedBytes,
    rootFileFinalized: accumulator.rootFileFinalized,
  })
}
