import type { V2CatalogEntry } from '../catalog/v2-records'
import type { V2ImageHeader } from './image-header'

// Automatic presentation is optional: these smaller budgets leave the manual
// preview's larger allowance available without competing with the first download.
export const V2_AUTOMATIC_PHOTO_ENCODED_BYTES = 4 * 1024 * 1024
export const V2_AUTOMATIC_PHOTO_DECODED_BYTES = 32 * 1024 * 1024
export const V2_AUTOMATIC_PHOTO_TIMEOUT_MILLISECONDS = 5_000

export class V2AutomaticPhotoDeferredError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'V2AutomaticPhotoDeferredError'
  }
}

export function isAutomaticPhotoCandidate(entry: V2CatalogEntry): boolean {
  return entry.kind === 'file' && entry.expectedSize > 0n &&
    entry.expectedSize <= BigInt(V2_AUTOMATIC_PHOTO_ENCODED_BYTES) &&
    /\.(png|jpe?g|webp)$/i.test(entry.name)
}

export function requireAutomaticPhotoHeader(header: V2ImageHeader | undefined): asserts header is V2ImageHeader {
  if (header === undefined) {
    throw new V2AutomaticPhotoDeferredError('Use Preview to inspect this file')
  }
  if (BigInt(header.width) * BigInt(header.height) * 4n > BigInt(V2_AUTOMATIC_PHOTO_DECODED_BYTES)) {
    throw new V2AutomaticPhotoDeferredError('Use Preview for this larger image')
  }
}
