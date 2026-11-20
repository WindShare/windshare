import { IMAGE_FILE_BYTES } from './image-header'

const PREVIEWABLE_IMAGE_EXTENSION = /\.(?:png|jpe?g|webp)$/iu
const PREVIEWABLE_VIDEO_EXTENSION = /\.(?:mp4|m4v)$/iu
const MIN_MP4_PREVIEW_BYTES = 16n

export type PreviewCapability = 'image' | 'video' | 'unsupported'

/**
 * Classifies whether a catalog file can realistically be previewed by FilePreview
 * based on its filename extension and known size bounds.
 *
 * This only gates UI preview affordances so clearly unsupported files (archives,
 * binaries, documents, oversized images) do not present a dead-end Preview button.
 * FilePreview still enforces header sniffing and byte/dimension limits at decode time.
 */
export function filePreviewCapability(name: string, expectedSize?: bigint): PreviewCapability {
  if (PREVIEWABLE_IMAGE_EXTENSION.test(name)) {
    if (expectedSize !== undefined && (expectedSize <= 0n || expectedSize > BigInt(IMAGE_FILE_BYTES))) {
      return 'unsupported'
    }
    return 'image'
  }
  if (PREVIEWABLE_VIDEO_EXTENSION.test(name)) {
    if (expectedSize !== undefined && expectedSize < MIN_MP4_PREVIEW_BYTES) {
      return 'unsupported'
    }
    return 'video'
  }
  return 'unsupported'
}

export function canPreviewFile(name: string, expectedSize?: bigint): boolean {
  return filePreviewCapability(name, expectedSize) !== 'unsupported'
}
