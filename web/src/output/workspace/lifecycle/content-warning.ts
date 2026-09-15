import { snapshotPortableCatalogPath } from '../../../catalog/path-policy'

export const MAX_CONTENT_WARNING_FILES = 8
const MAX_FILE_COUNT = 0xffff_ffff_ffff_ffffn

export interface MissingContentFile {
  readonly path: readonly string[]
  readonly reason: 'source-changed' | 'source-unavailable' | 'file-failed'
}

/** A usable archive can omit selected files; publication must preserve that distinction. */
export interface ReceiveContentWarning {
  readonly kind: 'partial-zip'
  readonly completedFileCount: bigint
  readonly selectedFileCount: bigint
  readonly missingFiles: readonly MissingContentFile[]
}

export function snapshotReceiveContentWarning(value: ReceiveContentWarning): ReceiveContentWarning {
  if (value === null || typeof value !== 'object' || value.kind !== 'partial-zip' ||
      !validCount(value.completedFileCount) || !validCount(value.selectedFileCount) ||
      value.completedFileCount >= value.selectedFileCount ||
      !Array.isArray(value.missingFiles) || value.missingFiles.length > MAX_CONTENT_WARNING_FILES ||
      BigInt(value.missingFiles.length) > value.selectedFileCount - value.completedFileCount) {
    throw new TypeError('Partial ZIP content warning is invalid')
  }
  const missingFiles = value.missingFiles.map(file => {
    if (file === null || typeof file !== 'object' ||
        !['source-changed', 'source-unavailable', 'file-failed'].includes(file.reason)) {
      throw new TypeError('Missing file reason is invalid')
    }
    return Object.freeze({ path: snapshotPortableCatalogPath(file.path), reason: file.reason })
  })
  return Object.freeze({
    kind: value.kind, completedFileCount: value.completedFileCount,
    selectedFileCount: value.selectedFileCount, missingFiles: Object.freeze(missingFiles),
  })
}

export function receiveContentWarningFields(
  value: ReceiveContentWarning | undefined,
): Readonly<{ contentWarning?: ReceiveContentWarning }> {
  return value === undefined ? {} : { contentWarning: snapshotReceiveContentWarning(value) }
}

function validCount(value: bigint): boolean {
  return typeof value === 'bigint' && value >= 0n && value <= MAX_FILE_COUNT
}
