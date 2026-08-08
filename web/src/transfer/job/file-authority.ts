import type {
  V2CatalogEntry,
  V2CatalogModifiedTime,
  V2ShareDescriptor,
} from '../../catalog/v2-records'
import { V2_CATALOG_IDENTITY_BYTES } from '../../catalog/v2-records'
import { FileGeometry } from '../../content/geometry'
import type { V2OpenedRevision } from '../../content/v2-session-services'
import { encodeBase64Url, equalBytes } from '../../crypto/bytes'
import { V2FileRevisionChangedError } from './failures'

/** Validates the untrusted revision adapter result before output or content I/O. */
export function validateOpenedFileRevision(
  share: V2ShareDescriptor,
  entry: Extract<V2CatalogEntry, { readonly kind: 'file' }>,
  opened: V2OpenedRevision,
): V2OpenedRevision {
  const descriptor = opened.descriptor
  if (!Number.isSafeInteger(share.chunkSize) || share.chunkSize <= 0 ||
      !isNonzeroIdentity(opened.leaseId) ||
      !equalBytes(descriptor.shareInstance, share.shareInstance) ||
      !equalBytes(descriptor.fileId, entry.id) ||
      !isNonzeroIdentity(descriptor.fileRevision) ||
      descriptor.exactSize !== entry.expectedSize ||
      descriptor.geometry.exactSize !== descriptor.exactSize ||
      descriptor.geometry.blockSize !== BigInt(share.chunkSize) ||
      !catalogModifiedTimeMatches(entry.modifiedTime, descriptor.modifiedTime)) {
    throw new V2FileRevisionChangedError(
      'Opened revision identity does not match its authenticated catalog entry',
    )
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
    }),
    leaseId: opened.leaseId.slice(),
    release: () => opened.release(),
  })
}

function isNonzeroIdentity(value: Uint8Array): boolean {
  return value.byteLength === V2_CATALOG_IDENTITY_BYTES && value.some((byte) => byte !== 0)
}

function catalogModifiedTimeMatches(
  expected: V2CatalogModifiedTime | undefined,
  actual: V2CatalogModifiedTime | undefined,
): boolean {
  if (expected === undefined) return true
  return actual !== undefined &&
    actual.seconds === expected.seconds &&
    actual.nanoseconds === expected.nanoseconds &&
    actual.precision === expected.precision &&
    actual.milliseconds === expected.milliseconds
}
