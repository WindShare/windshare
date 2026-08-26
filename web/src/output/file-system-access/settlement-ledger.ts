import type { ReceiveIntent } from '../../transfer/intent'
import type { PersistentDirectTreeMaterializationEvidence } from '../../transfer/settlement/persistent-execution'
import { MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT } from '../materialization-ledger/model'
import type {
  MaterializationLedgerBindingV1,
  MaterializationLedgerSealPurpose,
  MaterializationLedgerSealV1,
} from '../materialization-ledger/model'
import { scanAllFSAFileCheckpoints } from './checkpoint-repository'
import type {
  FSAFileCheckpointRepository,
  FSASemanticOutputRepository,
} from './checkpoint-repository'
import { fsaCheckpointSetDigest, type DirectTreeIntent } from './settlement-proof'
import type { FileCheckpointV2 } from '../persistence/checkpoint'
import {
  observePerformance,
  performanceElapsedMilliseconds,
  performanceNowMilliseconds,
  type PerformanceSummaryObservations,
} from '../diagnostics/performance-summary'

export interface FSAResumableCheckpointEvidence {
  readonly checkpointCount: bigint
  readonly checkpointSetDigest: string
  readonly checkpoints: readonly FileCheckpointV2[]
}

export class FSASettlementLedgerAuthority {
  readonly #intent: ReceiveIntent
  readonly #checkpoints: FSAFileCheckpointRepository
  readonly #binding: Promise<MaterializationLedgerBindingV1>
  readonly #performance: PerformanceSummaryObservations | undefined

  constructor(input: Readonly<{
    intent: ReceiveIntent
    checkpoints: FSAFileCheckpointRepository
    binding: Promise<MaterializationLedgerBindingV1>
    performance?: PerformanceSummaryObservations
  }>) {
    this.#intent = input.intent
    this.#checkpoints = input.checkpoints
    this.#binding = input.binding
    this.#performance = input.performance
  }

  async seal(input: Readonly<{
    evidence: PersistentDirectTreeMaterializationEvidence
    sealSequence: bigint
    purpose: MaterializationLedgerSealPurpose
  }>): Promise<MaterializationLedgerSealV1> {
    const binding = await this.#binding
    if (input.evidence.kind !== 'direct-tree-ledger' ||
        input.evidence.materializationBindingDigest !== binding.materializationBindingDigest) {
      throw new TypeError('DirectTree evidence does not locate this materialization ledger')
    }
    const startedAtMilliseconds = performanceNowMilliseconds(this.#performance)
    const seal = await this.#repository().sealMaterializationLedger({
      binding,
      sealSequence: input.sealSequence,
      purpose: input.purpose,
    })
    const elapsedMilliseconds = performanceElapsedMilliseconds(
      startedAtMilliseconds,
      performanceNowMilliseconds(this.#performance),
    )
    if (elapsedMilliseconds !== undefined) {
      observePerformance(this.#performance, summary => {
        for (let page = 0n; page < seal.pageCount; page += 1n) {
          summary.observeLedger({ transition: 'page' })
        }
        summary.observeLedger({ transition: 'seal', elapsedMilliseconds })
      })
    }
    return seal
  }

  async resumableCheckpointEvidence(): Promise<FSAResumableCheckpointEvidence> {
    observePerformance(this.#performance, summary =>
      summary.observeLedger({ transition: 'recovery_scan_fallback' }))
    const checkpoints = await scanAllFSAFileCheckpoints(this.#checkpoints, 'committed')
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
    const repository = this.#repository()
    const binding = await this.#binding
    for (;;) {
      const result = await repository.retireMaterializationLedgerBatch(
        binding,
        MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
      )
      if (result.state === 'complete') return
      if (result.deletedRows === 0) {
        throw new DOMException('FSA recovery metadata retirement made no progress', 'OperationError')
      }
    }
  }

  #repository(): FSASemanticOutputRepository {
    const repository = this.#checkpoints as Partial<FSASemanticOutputRepository>
    if (typeof repository.sealMaterializationLedger !== 'function' ||
        typeof repository.retireMaterializationLedgerBatch !== 'function') {
      throw new DOMException(
        'DirectTree settlement requires semantic ledger repository authority',
        'InvalidStateError',
      )
    }
    return repository as FSASemanticOutputRepository
  }
}
