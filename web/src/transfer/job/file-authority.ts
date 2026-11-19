import type {
  CatalogEntry,
  CatalogModifiedTime,
  ShareDescriptor,
} from '../../catalog/records'
import { CATALOG_IDENTITY_BYTES } from '../../catalog/records'
import { FileGeometry, type ByteRangeSet } from '../../content/geometry'
import type { FileRevisionDescriptor } from '../../content/records'
import {
  compareRevisionDescriptors, RevisionEvidenceUnavailableError, sourceMetadataFields, type RevisionComparison,
} from '../../content/revision-comparison'
import type { OpenedRevision, RevisionReopenedDuringRecoveryError } from '../../content/session-services'
import { encodeBase64Url, equalBytes } from '../../crypto/bytes'
import type { IncidentScopeHandle } from '../../diagnostics/incident'
import type { OutputFileTransaction, VerifiedDurableRanges } from '../output/file'
import { FaultScope, OutputFaultCode, SourceFaultCode, outputFault, sourceFault } from '../failure/fault'
import { withOutputSettlementTimeout } from '../settlement/output'
import type { FileTransferSettlement } from './contract'
import {
  FileRevisionChangedError, normalizeFileTransferFailure, normalizedFileTransferFault,
  type NormalizedFileTransferFailure,
} from './failures'

/** Validates the untrusted revision adapter result before output or content I/O. */
export function validateOpenedFileRevision(
  share: ShareDescriptor,
  entry: Extract<CatalogEntry, { readonly kind: 'file' }>,
  opened: OpenedRevision,
): OpenedRevision {
  const descriptor = opened.descriptor
  if (!Number.isSafeInteger(share.chunkSize) || share.chunkSize <= 0 ||
      !isNonzeroIdentity(opened.leaseId) ||
      !equalBytes(descriptor.shareInstance, share.shareInstance) ||
      !equalBytes(descriptor.fileId, entry.id) ||
      !isNonzeroIdentity(descriptor.fileRevision) ||
      descriptor.exactSize !== entry.expectedSize ||
      descriptor.geometry.exactSize !== descriptor.exactSize ||
      descriptor.geometry.blockSize !== BigInt(share.chunkSize)) {
    throw new FileRevisionChangedError(
      'Opened revision identity does not match its authenticated catalog entry',
    )
  }
  if (entry.modifiedTime !== undefined && descriptor.modifiedTime === undefined) {
    throw new RevisionEvidenceUnavailableError()
  }
  if (!catalogModifiedTimeMatches(entry.modifiedTime, descriptor.modifiedTime)) {
    throw new FileRevisionChangedError('Opened revision modification evidence changed')
  }
  const shareInstance = descriptor.shareInstance.slice()
  const fileId = descriptor.fileId.slice()
  const fileRevision = descriptor.fileRevision.slice()
  const modifiedTime = descriptor.modifiedTime === undefined
    ? undefined
    : Object.freeze({ ...descriptor.modifiedTime })
  return Object.freeze({
    descriptor: Object.freeze({
      shareInstance,
      shareInstanceId: encodeBase64Url(shareInstance),
      fileId,
      fileIdText: encodeBase64Url(fileId),
      fileRevision,
      fileRevisionText: encodeBase64Url(fileRevision),
      exactSize: descriptor.exactSize,
      geometry: new FileGeometry(descriptor.exactSize, descriptor.geometry.blockSize),
      ...(modifiedTime === undefined ? {} : { modifiedTime }),
      ...sourceMetadataFields(descriptor),
    }),
    leaseId: opened.leaseId.slice(),
    release: () => opened.release(),
  })
}

function isNonzeroIdentity(value: Uint8Array): boolean {
  return value.byteLength === CATALOG_IDENTITY_BYTES && value.some((byte) => byte !== 0)
}

function catalogModifiedTimeMatches(
  expected: CatalogModifiedTime | undefined,
  actual: CatalogModifiedTime | undefined,
): boolean {
  if (expected === undefined) return true
  return actual !== undefined &&
    actual.seconds === expected.seconds &&
    actual.nanoseconds === expected.nanoseconds &&
    actual.precision === expected.precision &&
    actual.milliseconds === expected.milliseconds
}

export const FILE_REVISION_READMISSION_DECISIONS = [
  'readmit', 'readmitted', 'observed-change', 'evidence-unavailable',
  'retry-limit', 'admission-failed', 'settlement-failed', 'canceled',
] as const

export interface FileRevisionReadmissionObservation {
  readonly previousRevision: string
  readonly currentRevision: string
  readonly comparisonReason: RevisionComparison['reason']
  readonly decision: typeof FILE_REVISION_READMISSION_DECISIONS[number]
  readonly retainedBytes: bigint
  readonly reusedBytes: bigint
  readonly elapsedMilliseconds: number
}

export interface FileRevisionReadmission {
  readonly previous: FileRevisionDescriptor
  readonly current: FileRevisionDescriptor
  readonly comparison: RevisionComparison
  readonly started: number
  readonly paused: VerifiedDurableRanges
  readonly initialDurable: ByteRangeSet
}

export const UNCONFIRMED_FILE_SETTLEMENT: FileTransferSettlement = Object.freeze({ kind: 'unconfirmed' })

interface ReadmissionReporting {
  readonly onRevisionReadmission?: (event: FileRevisionReadmissionObservation) => void
  readonly onRecoverableAcknowledged?: (bytes: bigint, transition: 'readmission') => void
}

interface ReadmissionSettlement extends ReadmissionReporting {
  readonly outputSettlementTimeoutMilliseconds: number
  readonly signal: AbortSignal
  readonly incidentScope?: IncidentScopeHandle
}

export function observeRevisionReadmission(
  options: ReadmissionReporting,
  previous: FileRevisionReadmission['previous'], current: FileRevisionReadmission['current'],
  comparison: FileRevisionReadmission['comparison'], decision: FileRevisionReadmissionObservation['decision'],
  retainedBytes: bigint, reusedBytes: bigint, started: number,
): void {
  options.onRevisionReadmission?.({
    previousRevision: previous.fileRevisionText, currentRevision: current.fileRevisionText,
    comparisonReason: comparison.reason, decision, retainedBytes, reusedBytes,
    elapsedMilliseconds: Math.max(0, performance.now() - started),
  })
}

export function observeReadmittedCoverage(
  options: ReadmissionReporting, previous: FileRevisionReadmission, current: FileRevisionReadmission['current'],
  admitted: ByteRangeSet,
): void {
  const retained = previous.paused.asRangeSet()
  const retainedBytes = rangeBytes(retained)
  const reusedBytes = retainedBytes - rangeBytes(admitted.missingFrom(retained))
  const lostNewBytes = rangeBytes(admitted.missingFrom(previous.initialDurable.missingFrom(retained)))
  if (lostNewBytes !== 0n) options.onRecoverableAcknowledged?.(-lostNewBytes, 'readmission')
  observeRevisionReadmission(options, previous.previous, current, compareRevisionDescriptors(previous.previous, current),
    'readmitted', retainedBytes, reusedBytes, previous.started)
}

export async function settleReopenedFile(
  options: ReadmissionSettlement, transaction: OutputFileTransaction,
  error: RevisionReopenedDuringRecoveryError, initialDurable: ByteRangeSet, started: number, retried: boolean,
): Promise<{
  readonly settlement: FileTransferSettlement
  readonly failure?: NormalizedFileTransferFailure
  readonly retry?: FileRevisionReadmission
}> {
  const { previous, current, comparison } = error
  let paused: VerifiedDurableRanges
  try {
    // The iterator and checkpoint scheduler have both drained. Force a durable
    // cut before releasing the old lease and asking for another revision.
    paused = await withOutputSettlementTimeout('pause reopened output file',
      options.outputSettlementTimeoutMilliseconds, () => transaction.pause(error))
  } catch (reason) {
    observeRevisionReadmission(options, previous, current, comparison, 'settlement-failed', 0n, 0n, started)
    return {
      settlement: UNCONFIRMED_FILE_SETTLEMENT,
      failure: normalizedFileTransferFault(outputFault(FaultScope.OutputPause, OutputFaultCode.MutationAmbiguous), {
        stage: 'settlement', error: reason, materializationFailureReason: 'output-write-failed',
        ...(options.incidentScope === undefined ? {} : { incidentScope: options.incidentScope }),
      }),
    }
  }
  const retainedBytes = rangeBytes(paused.asRangeSet())
  const settlement = { kind: 'paused', retainedBytes } as const
  const decision = readmissionDecision(options.signal, comparison, retried)
  observeRevisionReadmission(options, previous, current, comparison, decision, retainedBytes, 0n, started)
  if (decision === 'readmit') {
    return { settlement, retry: { previous, current, comparison, started, paused, initialDurable } }
  }
  if (decision === 'retry-limit' && !options.signal.aborted) {
    return {
      settlement,
      failure: normalizedFileTransferFault(sourceFault(FaultScope.FileLocal, SourceFaultCode.Unavailable), {
        error,
        recoveryDisposition: 'resumable_receive',
        fileOutcomeEvidence: { kind: 'deferred-revision-readmission' },
        ...(options.incidentScope === undefined ? {} : { incidentScope: options.incidentScope }),
      }),
    }
  }
  return {
    settlement, failure: normalizeFileTransferFailure(options.signal.aborted ? options.signal.reason : error, {
      signal: options.signal, ...(options.incidentScope === undefined ? {} : { incidentScope: options.incidentScope }),
    }),
  }
}

function readmissionDecision(
  signal: AbortSignal, comparison: RevisionComparison, retried: boolean,
): FileRevisionReadmissionObservation['decision'] {
  if (signal.aborted) return 'canceled'
  if (comparison.kind === 'observed-change') return 'observed-change'
  if (comparison.kind === 'unavailable') return 'evidence-unavailable'
  return retried ? 'retry-limit' : 'readmit'
}

export function rangeBytes(ranges: ByteRangeSet): bigint {
  return ranges.ranges.reduce((total, range) => total + range.end - range.start, 0n)
}

export async function releaseRevisionLease(
  acquired: OpenedRevision | undefined,
): Promise<NormalizedFileTransferFailure | undefined> {
  if (acquired === undefined) return undefined
  try {
    // Release waits for shared-read safety and hands reclamation to the session.
    // Remote acknowledgments cannot delay already-settled output.
    await acquired.release()
    return undefined
  } catch (error) {
    return normalizeFileTransferFailure(error, { stage: 'settlement' })
  }
}
