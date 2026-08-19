import type { DirectoryId, FileId } from '../catalog/model'

export interface DirectoryTransferFailure {
  readonly kind: 'directory'
  readonly directoryId: DirectoryId
  readonly reason: unknown
}

export interface FileTransferFailure {
  readonly kind: 'file'
  readonly fileId: FileId
  readonly reason: unknown
}

export type TransferFailure = DirectoryTransferFailure | FileTransferFailure
export const MAXIMUM_RETAINED_TRANSFER_FAILURES = 64

export type TransferFileOutcome =
  | 'source-drift'
  | 'revision-conflict'
  | 'checkpoint-invalid'
  | 'owned-object-unknown'
  | 'destination-collision'
  | 'failed'

export type TransferFileOutcomeEvidence =
  | Readonly<{ readonly kind: 'authenticated-source-drift' }>
  | Readonly<{
      readonly kind: 'checkpoint-decision'
      readonly decision: 'revision-conflict' | 'ownership-conflict' | 'invalid'
    }>
  | Readonly<{ readonly kind: 'occupied-unbound-destination' }>
  | Readonly<{ readonly kind: 'residual-failure' }>

export interface TransferFileOutcomeCounts {
  readonly sourceDriftFiles: number
  readonly revisionConflictFiles: number
  readonly checkpointInvalidFiles: number
  readonly ownedObjectUnknownFiles: number
  readonly collisionFiles: number
  readonly failedFiles: number
}

export const EMPTY_TRANSFER_FILE_OUTCOME_COUNTS: TransferFileOutcomeCounts = Object.freeze({
  sourceDriftFiles: 0,
  revisionConflictFiles: 0,
  checkpointInvalidFiles: 0,
  ownedObjectUnknownFiles: 0,
  collisionFiles: 0,
  failedFiles: 0,
})

export function projectTransferFileOutcome(
  evidence: TransferFileOutcomeEvidence,
): TransferFileOutcome {
  switch (evidence.kind) {
    case 'authenticated-source-drift': return 'source-drift'
    case 'occupied-unbound-destination': return 'destination-collision'
    case 'residual-failure': return 'failed'
    case 'checkpoint-decision':
      switch (evidence.decision) {
        case 'revision-conflict': return 'revision-conflict'
        case 'ownership-conflict': return 'owned-object-unknown'
        case 'invalid': return 'checkpoint-invalid'
      }
  }
}

export interface TransferFailureSummary {
  readonly failures: readonly TransferFailure[]
  readonly failureCount: number
  readonly fileFailureCount: number
  readonly omittedFailureCount: number
  readonly fileOutcomes: TransferFileOutcomeCounts
}

export const EMPTY_TRANSFER_FAILURE_SUMMARY: TransferFailureSummary = Object.freeze({
  failures: Object.freeze([]),
  failureCount: 0,
  fileFailureCount: 0,
  omittedFailureCount: 0,
  fileOutcomes: EMPTY_TRANSFER_FILE_OUTCOME_COUNTS,
})

interface TransferWorkerSettlementBase {
  readonly failures: readonly TransferFailure[]
  readonly failureCount: number
  readonly fileFailureCount: number
  readonly omittedFailureCount: number
  readonly fileOutcomes: TransferFileOutcomeCounts
}

export type TransferWorkerSettlement =
  | Readonly<TransferWorkerSettlementBase & { readonly status: 'Succeeded' }>
  | Readonly<TransferWorkerSettlementBase & { readonly status: 'CompletedWithErrors' }>
  | Readonly<TransferWorkerSettlementBase & { readonly status: 'Paused' }>

export type CompletedTransferWorkerSettlement = Exclude<
  TransferWorkerSettlement,
  Readonly<TransferWorkerSettlementBase & { readonly status: 'Paused' }>
>

export type SuccessfulTransferWorkerSettlement = Extract<
  TransferWorkerSettlement,
  Readonly<{ readonly status: 'Succeeded' }>
>

/** Retains representative diagnostics while exact aggregate counts remain width-independent. */
export class TransferFailureAccumulator {
  readonly #failures: TransferFailure[] = []
  #failureCount = 0
  #fileFailureCount = 0
  #directoryFailureCount = 0
  readonly #fileOutcomes = { ...EMPTY_TRANSFER_FILE_OUTCOME_COUNTS }

  get failureCount(): number {
    return this.#failureCount
  }

  get hasDirectoryFailures(): boolean {
    return this.#directoryFailureCount > 0
  }

  record(failure: TransferFailure, fileOutcome?: TransferFileOutcome): void {
    this.#count(failure, fileOutcome)
    if (this.#failures.length < MAXIMUM_RETAINED_TRANSFER_FAILURES) {
      this.#failures.push(Object.freeze({ ...failure }))
    }
  }

  /** Retains bounded evidence for a terminal semantic settlement even after saturation. */
  recordRepresentative(failure: TransferFailure, fileOutcome?: TransferFileOutcome): void {
    this.#count(failure, fileOutcome)
    const snapshot = Object.freeze({ ...failure })
    if (this.#failures.length < MAXIMUM_RETAINED_TRANSFER_FAILURES) {
      this.#failures.push(snapshot)
      return
    }
    this.#failures[this.#failures.length - 1] = snapshot
  }

  #count(failure: TransferFailure, fileOutcome?: TransferFileOutcome): void {
    if (failure.kind === 'directory' && fileOutcome !== undefined) {
      throw new TypeError('directory failures cannot carry file outcomes')
    }
    this.#failureCount = incrementExact(this.#failureCount, 'Transfer failure count')
    if (failure.kind === 'directory') {
      this.#directoryFailureCount += 1
      return
    }
    this.#fileFailureCount = incrementExact(this.#fileFailureCount, 'File failure count')
    this.#recordFileOutcome(fileOutcome ?? 'failed')
  }

  #recordFileOutcome(outcome: TransferFileOutcome): void {
    switch (outcome) {
      case 'source-drift':
        this.#fileOutcomes.sourceDriftFiles = incrementExact(
          this.#fileOutcomes.sourceDriftFiles,
          'Source drift count',
        )
        break
      case 'revision-conflict':
        this.#fileOutcomes.revisionConflictFiles = incrementExact(
          this.#fileOutcomes.revisionConflictFiles,
          'Revision conflict count',
        )
        break
      case 'checkpoint-invalid':
        this.#fileOutcomes.checkpointInvalidFiles = incrementExact(
          this.#fileOutcomes.checkpointInvalidFiles,
          'Invalid checkpoint count',
        )
        break
      case 'owned-object-unknown':
        this.#fileOutcomes.ownedObjectUnknownFiles = incrementExact(
          this.#fileOutcomes.ownedObjectUnknownFiles,
          'Owned object conflict count',
        )
        break
      case 'destination-collision':
        this.#fileOutcomes.collisionFiles = incrementExact(
          this.#fileOutcomes.collisionFiles,
          'Destination collision count',
        )
        break
      case 'failed':
        this.#fileOutcomes.failedFiles = incrementExact(
          this.#fileOutcomes.failedFiles,
          'Residual file failure count',
        )
        break
      default: {
        const exhaustive: never = outcome
        throw new TypeError(`Unknown transfer file outcome: ${String(exhaustive)}`)
      }
    }
  }

  snapshot(): TransferFailureSummary {
    return Object.freeze({
      failures: Object.freeze([...this.#failures]),
      failureCount: this.#failureCount,
      fileFailureCount: this.#fileFailureCount,
      omittedFailureCount: this.#failureCount - this.#failures.length,
      fileOutcomes: Object.freeze({ ...this.#fileOutcomes }),
    })
  }
}

export function summarizeTransferFailures(
  failures: readonly TransferFailure[],
): TransferFailureSummary {
  const accumulator = new TransferFailureAccumulator()
  for (const failure of failures) accumulator.record(failure)
  return accumulator.snapshot()
}

export function transferWorkerSettlement(
  status: 'Succeeded',
  summary: TransferFailureSummary,
): Extract<TransferWorkerSettlement, { readonly status: 'Succeeded' }>
export function transferWorkerSettlement(
  status: 'CompletedWithErrors',
  summary: TransferFailureSummary,
): Extract<TransferWorkerSettlement, { readonly status: 'CompletedWithErrors' }>
export function transferWorkerSettlement(
  status: 'Paused',
  summary: TransferFailureSummary,
): Extract<TransferWorkerSettlement, { readonly status: 'Paused' }>
export function transferWorkerSettlement(
  status: TransferWorkerSettlement['status'],
  summary: TransferFailureSummary,
): TransferWorkerSettlement {
  validateFailureSummary(summary)
  if (status === 'Succeeded' && summary.failureCount !== 0) {
    throw new TypeError('successful transfer workers cannot contain failures')
  }
  if (status === 'CompletedWithErrors' && summary.failureCount === 0) {
    throw new TypeError('transfer workers completed with errors require failure evidence')
  }
  return Object.freeze({
    status,
    failures: Object.freeze(summary.failures.map((failure) => Object.freeze({ ...failure }))),
    failureCount: summary.failureCount,
    fileFailureCount: summary.fileFailureCount,
    omittedFailureCount: summary.omittedFailureCount,
    fileOutcomes: Object.freeze({ ...summary.fileOutcomes }),
  }) as TransferWorkerSettlement
}

function validateFailureSummary(summary: TransferFailureSummary): void {
  if (
    !Number.isSafeInteger(summary.failureCount) ||
    summary.failureCount < 0 ||
    !Number.isSafeInteger(summary.omittedFailureCount) ||
    summary.omittedFailureCount < 0 ||
    summary.failures.length > MAXIMUM_RETAINED_TRANSFER_FAILURES ||
    summary.failureCount !== summary.failures.length + summary.omittedFailureCount ||
    summary.fileFailureCount > summary.failureCount ||
    !validFileOutcomeCounts(summary.fileOutcomes, summary.fileFailureCount)
  ) throw new TypeError('Transfer failure summary is inconsistent or unbounded')
}

function validFileOutcomeCounts(
  counts: TransferFileOutcomeCounts,
  expected: number,
): boolean {
  if (!Number.isSafeInteger(expected) || expected < 0) return false
  let total = 0
  for (const count of Object.values(counts)) {
    if (!Number.isSafeInteger(count) || count < 0 || total > Number.MAX_SAFE_INTEGER - count) {
      return false
    }
    total += count
  }
  return total === expected
}

function incrementExact(value: number, label: string): number {
  if (value === Number.MAX_SAFE_INTEGER) {
    throw new RangeError(`${label} exceeds exact integer representation`)
  }
  return value + 1
}
