import { directoryId, fileId } from '../catalog/model'
import type { V2CatalogEntry } from '../catalog/v2-records'
import type { V2FrozenSelectionPolicy } from '../catalog/v2-selection'
import { decodeBase64Url } from '../crypto/bytes'
import type { TransferJobOptions } from './job/contract'
import {
  normalizeV2FileTransferFailure,
  transferFileOutcomeEvidence,
  type TransferFailureClassificationOptions,
} from './job/failures'
import type { V2TransferObservers } from './job/observers'
import {
  V2ExplicitSelectionTargetLedger,
  V2SelectionTargetMissingError,
} from './job/selection'
import type { SelectionMeasure, SelectionMeasureTracker } from './measure'
import {
  TransferFailureAccumulator,
  projectTransferFileOutcome,
  type TransferFailureSummary,
} from './outcome'
import type { V2TransferProgressLedger } from './progress/v2-ledger'

interface V2JobFailureAuthorityOptions {
  readonly selection: V2FrozenSelectionPolicy
  readonly signal: AbortSignal
  readonly measure: SelectionMeasureTracker
  readonly progress: V2TransferProgressLedger
  readonly observers: () => V2TransferObservers | undefined
  readonly emitProgress: () => void
  readonly incidentScope?: TransferJobOptions['incidentScope']
}

export class V2JobFailureAuthority {
  readonly explicitTargets: V2ExplicitSelectionTargetLedger
  readonly #failedDirectoryIds = new Set<string>()
  readonly #failures = new TransferFailureAccumulator()
  readonly #options: V2JobFailureAuthorityOptions

  constructor(options: V2JobFailureAuthorityOptions) {
    this.#options = options
    this.explicitTargets = new V2ExplicitSelectionTargetLedger(options.selection, options.signal)
  }

  get failureCount(): number {
    return this.#failures.failureCount
  }

  snapshot(): TransferFailureSummary {
    return this.#failures.snapshot()
  }

  recordDirectory(identity: string, reason: unknown): void {
    if (this.#failedDirectoryIds.has(identity)) return
    this.#failedDirectoryIds.add(identity)
    const normalized = normalizeV2FileTransferFailure(reason, this.#failureContext())
    if (normalized.kind === 'canceled') throw normalized.diagnostic
    this.#options.progress.failDirectory()
    this.#failures.record(
      Object.freeze({
        kind: 'directory',
        directoryId: directoryId(identity),
        classification: normalized.diagnostic.classification,
      }),
      transferSelectionOrdinal(identity),
    )
    this.#options.emitProgress()
  }

  recordFile(entry: Extract<V2CatalogEntry, { kind: 'file' }>, reason: unknown): void {
    const normalized = normalizeV2FileTransferFailure(reason, this.#failureContext())
    if (normalized.kind === 'canceled') throw normalized.diagnostic
    const evidence = transferFileOutcomeEvidence(reason) ??
      Object.freeze({ kind: 'residual-failure' as const })
    this.#failures.record(
      Object.freeze({
        kind: 'file',
        fileId: fileId(entry.idText),
        classification: normalized.diagnostic.classification,
      }),
      transferSelectionOrdinal(entry.idText),
      projectTransferFileOutcome(evidence),
    )
    this.#options.progress.recordFileError()
    this.#options.emitProgress()
  }

  finishDiscovery(): SelectionMeasure {
    const missing = this.explicitTargets.missing()
    if (this.#options.progress.failedDirectories === 0) {
      for (const target of missing) {
        const normalized = normalizeV2FileTransferFailure(
          new V2SelectionTargetMissingError(target),
          this.#failureContext(),
        )
        if (normalized.kind === 'canceled') throw normalized.diagnostic
        this.#failures.recordRepresentative(
          target.kind === 'directory'
            ? Object.freeze({
                kind: 'directory',
                directoryId: directoryId(target.idText),
                classification: normalized.diagnostic.classification,
              })
            : Object.freeze({
                kind: 'file',
                fileId: fileId(target.idText),
                classification: normalized.diagnostic.classification,
              }),
          transferSelectionOrdinal(target.idText),
        )
        this.#options.progress.recordSelectionError()
      }
    }
    const complete = this.#options.progress.failedDirectories === 0 && missing.length === 0
    const measure = complete ? this.#options.measure.complete() : this.#options.measure.fail()
    this.#options.observers()?.measure(measure)
    this.#options.emitProgress()
    return measure
  }

  #failureContext(): TransferFailureClassificationOptions {
    return this.#options.incidentScope === undefined
      ? Object.freeze({})
      : Object.freeze({ incidentScope: this.#options.incidentScope })
  }
}

function transferSelectionOrdinal(identity: string): bigint {
  const bytes = decodeBase64Url(identity)
  if (bytes?.byteLength !== 16 || bytes.every(value => value === 0)) {
    throw new TypeError('Transfer failure identity is not canonical')
  }
  let ordinal = 0n
  for (const value of bytes) ordinal = ordinal << 8n | BigInt(value)
  return ordinal
}
