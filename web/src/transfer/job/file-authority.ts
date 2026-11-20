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
import type { OpenedRevision } from '../../content/session-services'
import { encodeBase64Url, equalBytes } from '../../crypto/bytes'
import type { OutputSession, VerifiedDurableRanges } from '../output/file'
import {
  FileRevisionChangedError, normalizeFileTransferFailure, type NormalizedFileTransferFailure,
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
  'readmission-unsupported', 'retry-limit', 'admission-failed', 'settlement-failed', 'canceled',
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
  readonly beginFile: OutputSession['beginFile']
  readonly stalledReopens: number
}

interface ReadmissionReporting {
  readonly onRevisionReadmission?: (event: FileRevisionReadmissionObservation) => void
  readonly onRecoverableAcknowledged?: (bytes: bigint, transition: 'readmission') => void
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
