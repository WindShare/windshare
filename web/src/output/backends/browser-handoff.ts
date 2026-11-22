import type { ArtifactKind } from '../../transfer/selection'

export const ORIGINAL_FILE_MEDIA_TYPE = 'application/octet-stream'
export const ZIP_ARCHIVE_MEDIA_TYPE = 'application/zip'
export const OBJECT_URL_LEASE_MILLISECONDS = 60_000

export interface BrowserDownloadWindow {
  readonly URL?: Pick<typeof URL, 'createObjectURL' | 'revokeObjectURL'>
  readonly document?: Pick<Document, 'createElement' | 'body'>
  readonly setTimeout?: typeof setTimeout
  readonly clearTimeout?: typeof clearTimeout
}

export interface BrowserDownloadResult {
  readonly kind: 'download-started'
  readonly suggestedName: string
  readonly leaseStartedAt: number
  readonly leaseEndsAt: number
}

export function artifactDownloadMediaType(kind: ArtifactKind): string {
  if (kind === 'zip-archive') return ZIP_ARCHIVE_MEDIA_TYPE
  if (kind === 'original-file') return ORIGINAL_FILE_MEDIA_TYPE
  throw new TypeError('A directory tree cannot be handed to the browser as a single file')
}

export function supportsBrowserDownload(windowPort: BrowserDownloadWindow): boolean {
  return typeof windowPort.URL?.createObjectURL === 'function' &&
    typeof windowPort.URL?.revokeObjectURL === 'function' &&
    typeof windowPort.document?.createElement === 'function'
}

/**
 * Triggers a browser download for a Blob or OPFS File and schedules automatic
 * ObjectURL revocation so retained references cannot leak memory.
 */
export function triggerBrowserDownload(
  windowPort: BrowserDownloadWindow,
  source: Blob,
  suggestedName: string,
  mediaType: string,
  leaseMilliseconds = OBJECT_URL_LEASE_MILLISECONDS,
): BrowserDownloadResult {
  if (!supportsBrowserDownload(windowPort)) {
    throw new DOMException('Browser download handoff is unavailable', 'NotSupportedError')
  }
  const blob = source.type === mediaType ? source : source.slice(0, source.size, mediaType)
  const objectUrl = windowPort.URL!.createObjectURL(blob)
  const schedule = windowPort.setTimeout?.bind(windowPort) ?? setTimeout
  const anchor = windowPort.document!.createElement('a')
  anchor.download = suggestedName
  anchor.href = objectUrl
  anchor.hidden = true
  const parent = windowPort.document!.body
  if (parent !== null) parent.append(anchor)
  try {
    anchor.click()
  } catch (error) {
    anchor.remove()
    windowPort.URL!.revokeObjectURL(objectUrl)
    throw error
  }
  anchor.remove()
  const now = Date.now()
  schedule(() => {
    try { windowPort.URL?.revokeObjectURL(objectUrl) } catch { /* Best-effort URL cleanup. */ }
  }, leaseMilliseconds)
  return Object.freeze({
    kind: 'download-started',
    suggestedName,
    leaseStartedAt: now,
    leaseEndsAt: now + leaseMilliseconds,
  })
}
