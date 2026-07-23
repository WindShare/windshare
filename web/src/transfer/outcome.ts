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
export type JobOutcomeStatus = 'Succeeded' | 'CompletedWithErrors' | 'Aborted'
export const MAXIMUM_RETAINED_TRANSFER_FAILURES = 64

export interface TransferFailureSummary {
  readonly failures: readonly TransferFailure[]
  readonly failureCount: number
  readonly omittedFailureCount: number
}

export const EMPTY_TRANSFER_FAILURE_SUMMARY: TransferFailureSummary = Object.freeze({
  failures: Object.freeze([]),
  failureCount: 0,
  omittedFailureCount: 0,
})

export interface JobOutcome {
  readonly status: JobOutcomeStatus
  readonly failures: readonly TransferFailure[]
  readonly failureCount: number
  readonly omittedFailureCount: number
}

/** Retains representative diagnostics while exact aggregate counts remain width-independent. */
export class TransferFailureAccumulator {
  readonly #failures: TransferFailure[] = []
  #failureCount = 0
  #directoryFailureCount = 0

  get failureCount(): number {
    return this.#failureCount
  }

  get hasDirectoryFailures(): boolean {
    return this.#directoryFailureCount > 0
  }

  record(failure: TransferFailure): void {
    if (this.#failureCount === Number.MAX_SAFE_INTEGER) {
      throw new RangeError('Transfer failure count exceeds exact integer representation')
    }
    this.#failureCount += 1
    if (failure.kind === 'directory') this.#directoryFailureCount += 1
    if (this.#failures.length < MAXIMUM_RETAINED_TRANSFER_FAILURES) {
      this.#failures.push(Object.freeze({ ...failure }))
    }
  }

  snapshot(): TransferFailureSummary {
    return Object.freeze({
      failures: Object.freeze([...this.#failures]),
      failureCount: this.#failureCount,
      omittedFailureCount: this.#failureCount - this.#failures.length,
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

export function jobOutcome(
  status: JobOutcomeStatus,
  summary: TransferFailureSummary,
): JobOutcome {
  validateFailureSummary(summary)
  if (status === 'Succeeded' && summary.failureCount !== 0) {
    throw new TypeError('a succeeded transfer job cannot contain failures')
  }
  if (status === 'CompletedWithErrors' && summary.failureCount === 0) {
    throw new TypeError('a transfer completed with errors must contain a failure')
  }
  return Object.freeze({
    status,
    failures: Object.freeze(summary.failures.map((failure) => Object.freeze({ ...failure }))),
    failureCount: summary.failureCount,
    omittedFailureCount: summary.omittedFailureCount,
  })
}

function validateFailureSummary(summary: TransferFailureSummary): void {
  if (
    !Number.isSafeInteger(summary.failureCount) ||
    summary.failureCount < 0 ||
    !Number.isSafeInteger(summary.omittedFailureCount) ||
    summary.omittedFailureCount < 0 ||
    summary.failures.length > MAXIMUM_RETAINED_TRANSFER_FAILURES ||
    summary.failureCount !== summary.failures.length + summary.omittedFailureCount
  ) throw new TypeError('Transfer failure summary is inconsistent or unbounded')
}
