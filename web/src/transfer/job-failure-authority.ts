import { directoryId, fileId } from '../catalog/model'
import type { CatalogEntry } from '../catalog/records'
import type { FrozenSelectionPolicy } from '../catalog/selection'
import { decodeBase64Url } from '../crypto/bytes'
import type { TransferJobOptions } from './job/contract'
import {
  normalizeFileTransferFailure,
  transferFileOutcomeEvidence,
  type TransferFailureClassificationOptions,
} from './job/failures'
import type { TransferObservers } from './job/observers'
import {
  ExplicitSelectionTargetLedger,
  SelectionTargetMissingError,
} from './job/selection'
import type { SelectionMeasure, SelectionMeasureTracker } from './measure'
import {
  TransferFailureAccumulator,
  projectTransferFileOutcome,
  type TransferFailureSummary,
} from './outcome'
import type { TransferProgressLedger } from './progress/ledger'

interface JobFailureAuthorityOptions {
  readonly selection: FrozenSelectionPolicy
  readonly signal: AbortSignal
  readonly measure: SelectionMeasureTracker
  readonly progress: TransferProgressLedger
  readonly observers: () => TransferObservers | undefined
  readonly emitProgress: () => void
  readonly incidentScope?: TransferJobOptions['incidentScope']
}

export class JobFailureAuthority {
  readonly explicitTargets: ExplicitSelectionTargetLedger
  readonly #failedDirectoryIds = new Set<string>()
  readonly #failures = new TransferFailureAccumulator()
  readonly #options: JobFailureAuthorityOptions
  #discoveryFailed = false

  constructor(options: JobFailureAuthorityOptions) {
    this.#options = options
    this.explicitTargets = new ExplicitSelectionTargetLedger(options.selection, options.signal)
  }

  get failureCount(): number {
    return this.#failures.failureCount
  }

  snapshot(): TransferFailureSummary {
    return this.#failures.snapshot()
  }

  recordDiscoveryDirectory(identity: string, reason: unknown): void {
    this.#discoveryFailed = true
    this.recordDirectory(identity, reason)
  }

  recordDirectory(identity: string, reason: unknown): void {
    if (this.#failedDirectoryIds.has(identity)) return
    this.#failedDirectoryIds.add(identity)
    const normalized = normalizeFileTransferFailure(reason, this.#failureContext())
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

  recordFile(entry: Extract<CatalogEntry, { kind: 'file' }>, reason: unknown): void {
    const normalized = normalizeFileTransferFailure(reason, this.#failureContext())
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
    if (!this.#discoveryFailed) {
      for (const target of missing) {
        const normalized = normalizeFileTransferFailure(
          new SelectionTargetMissingError(target),
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
    const complete = !this.#discoveryFailed && missing.length === 0
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
