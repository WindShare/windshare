import type { DirectoryId, FileId } from '../catalog/model'
import { isFailureFact } from '../diagnostics/incident'
import { compareFaults, isFault } from './fault'
import { MATERIALIZATION_FAILURE_REASONS } from './job/contract'
import type { ClassifiedTransferFailure } from './job/failures'

export interface DirectoryTransferFailure {
  readonly kind: 'directory'
  readonly directoryId: DirectoryId
  readonly classification: ClassifiedTransferFailure
}

export interface FileTransferFailure {
  readonly kind: 'file'
  readonly fileId: FileId
  readonly classification: ClassifiedTransferFailure
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
  readonly trigger?: ClassifiedTransferFailure
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
  readonly trigger?: ClassifiedTransferFailure
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

interface TriggerCandidate {
  readonly classification: ClassifiedTransferFailure
  readonly selectionOrdinal: bigint
}

/**
 * Retains bounded product evidence while trigger nomination stays exact even after
 * representative storage saturates. The stable ordinal is never exported.
 */
export class TransferFailureAccumulator {
  readonly #failures: TransferFailure[] = []
  #failureCount = 0
  #fileFailureCount = 0
  #directoryFailureCount = 0
  readonly #fileOutcomes = { ...EMPTY_TRANSFER_FILE_OUTCOME_COUNTS }
  #trigger: TriggerCandidate | undefined

  get failureCount(): number {
    return this.#failureCount
  }

  get hasDirectoryFailures(): boolean {
    return this.#directoryFailureCount > 0
  }

  record(
    failure: TransferFailure,
    selectionOrdinal: bigint,
    fileOutcome?: TransferFileOutcome,
  ): void {
    this.#count(failure, selectionOrdinal, fileOutcome)
    if (this.#failures.length < MAXIMUM_RETAINED_TRANSFER_FAILURES) {
      this.#failures.push(snapshotFailure(failure))
    }
  }

  /** Retains bounded evidence for a terminal semantic settlement even after saturation. */
  recordRepresentative(
    failure: TransferFailure,
    selectionOrdinal: bigint,
    fileOutcome?: TransferFileOutcome,
  ): void {
    this.#count(failure, selectionOrdinal, fileOutcome)
    const snapshot = snapshotFailure(failure)
    if (this.#failures.length < MAXIMUM_RETAINED_TRANSFER_FAILURES) {
      this.#failures.push(snapshot)
      return
    }
    this.#failures[this.#failures.length - 1] = snapshot
  }

  #count(
    failure: TransferFailure,
    selectionOrdinal: bigint,
    fileOutcome?: TransferFileOutcome,
  ): void {
    if (!isClassifiedTransferFailure(failure.classification)) {
      throw new TypeError('Transfer failures require one closed classification')
    }
    if (selectionOrdinal < 0n) {
      throw new RangeError('Transfer failure selection ordinal must be non-negative')
    }
    if (failure.kind === 'directory' && fileOutcome !== undefined) {
      throw new TypeError('directory failures cannot carry file outcomes')
    }
    this.#failureCount = incrementExact(this.#failureCount, 'Transfer failure count')
    if (failure.kind === 'directory') {
      this.#directoryFailureCount = incrementExact(
        this.#directoryFailureCount,
        'Directory failure count',
      )
    } else {
      this.#fileFailureCount = incrementExact(this.#fileFailureCount, 'File failure count')
      this.#recordFileOutcome(fileOutcome ?? 'failed')
    }
    const candidate = Object.freeze({
      classification: failure.classification,
      selectionOrdinal,
    })
    if (this.#trigger === undefined || compareTriggerCandidates(candidate, this.#trigger) > 0) {
      this.#trigger = candidate
    }
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
      failures: Object.freeze(this.#failures.map(snapshotFailure)),
      failureCount: this.#failureCount,
      fileFailureCount: this.#fileFailureCount,
      omittedFailureCount: this.#failureCount - this.#failures.length,
      fileOutcomes: Object.freeze({ ...this.#fileOutcomes }),
      ...(this.#trigger === undefined ? {} : { trigger: this.#trigger.classification }),
    })
  }
}

export function summarizeTransferFailures(
  failures: readonly Readonly<{
    readonly failure: TransferFailure
    readonly selectionOrdinal: bigint
  }>[],
): TransferFailureSummary {
  const accumulator = new TransferFailureAccumulator()
  for (const entry of failures) {
    accumulator.record(entry.failure, entry.selectionOrdinal)
  }
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
    failures: Object.freeze(summary.failures.map(snapshotFailure)),
    failureCount: summary.failureCount,
    fileFailureCount: summary.fileFailureCount,
    omittedFailureCount: summary.omittedFailureCount,
    fileOutcomes: Object.freeze({ ...summary.fileOutcomes }),
    ...(summary.trigger === undefined ? {} : { trigger: summary.trigger }),
  }) as TransferWorkerSettlement
}

function compareTriggerCandidates(left: TriggerCandidate, right: TriggerCandidate): number {
  const authority = compareFaults(left.classification.fault, right.classification.fault)
  if (authority !== 0) return authority
  if (left.selectionOrdinal < right.selectionOrdinal) return 1
  if (left.selectionOrdinal > right.selectionOrdinal) return -1
  return 0
}

function snapshotFailure(failure: TransferFailure): TransferFailure {
  return failure.kind === 'directory'
    ? Object.freeze({
        kind: 'directory',
        directoryId: failure.directoryId,
        classification: failure.classification,
      })
    : Object.freeze({
        kind: 'file',
        fileId: failure.fileId,
        classification: failure.classification,
      })
}

function isClassifiedTransferFailure(value: unknown): value is ClassifiedTransferFailure {
  if (typeof value !== 'object' || value === null || !Object.isFrozen(value)) return false
  const candidate = value as Partial<ClassifiedTransferFailure>
  return isFault(candidate.fault) &&
    Object.isFrozen(candidate.fault) &&
    isFailureFact(candidate.fact) &&
    (candidate.fact.kind !== 'fault' || candidate.fact.payload.fault === candidate.fault) &&
    typeof candidate.materializationFailureReason === 'string' &&
    MATERIALIZATION_FAILURE_REASONS.some(
      reason => reason === candidate.materializationFailureReason,
    )
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
    !validFileOutcomeCounts(summary.fileOutcomes, summary.fileFailureCount) ||
    (summary.failureCount === 0) !== (summary.trigger === undefined)
  ) {
    throw new TypeError('Transfer failure summary is inconsistent or unbounded')
  }
  if (
    summary.trigger !== undefined &&
    !isClassifiedTransferFailure(summary.trigger)
  ) {
    throw new TypeError('Transfer failure summary trigger is not classified')
  }
  for (const failure of summary.failures) {
    if (!isClassifiedTransferFailure(failure.classification)) {
      throw new TypeError('Transfer failure summary contains an unclassified entry')
    }
  }
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
