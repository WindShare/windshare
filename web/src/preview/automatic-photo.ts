import type { CatalogEntry } from '../catalog/records'
import type { ImageHeader } from './image-header'

// Automatic presentation is optional: these smaller budgets leave the manual
// preview's larger allowance available without competing with the first download.
export const AUTOMATIC_PHOTO_ENCODED_BYTES = 4 * 1024 * 1024
export const AUTOMATIC_PHOTO_DECODED_BYTES = 32 * 1024 * 1024
export const AUTOMATIC_PHOTO_TIMEOUT_MILLISECONDS = 5_000

export class AutomaticPhotoDeferredError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'AutomaticPhotoDeferredError'
  }
}

export function isAutomaticPhotoCandidate(entry: CatalogEntry): boolean {
  return entry.kind === 'file' && entry.expectedSize > 0n &&
    entry.expectedSize <= BigInt(AUTOMATIC_PHOTO_ENCODED_BYTES) &&
    /\.(png|jpe?g|webp)$/i.test(entry.name)
}

export function requireAutomaticPhotoHeader(header: ImageHeader | undefined): asserts header is ImageHeader {
  if (header === undefined) {
    throw new AutomaticPhotoDeferredError('Use Preview to inspect this file')
  }
  if (BigInt(header.width) * BigInt(header.height) * 4n > BigInt(AUTOMATIC_PHOTO_DECODED_BYTES)) {
    throw new AutomaticPhotoDeferredError('Use Preview for this larger image')
  }
}
