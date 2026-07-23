const MEBIBYTE = 1024 * 1024

export const PORTABLE_DOWNLOAD_MAXIMUM_BYTES = 64 * MEBIBYTE
export const PORTABLE_DOWNLOAD_PART_BYTES = MEBIBYTE
export const PORTABLE_DOWNLOAD_MAXIMUM_PARTS =
  PORTABLE_DOWNLOAD_MAXIMUM_BYTES / PORTABLE_DOWNLOAD_PART_BYTES

const OBJECT_URL_RETENTION_MILLISECONDS = 60_000

export interface PortableDownloadPorts {
  readonly createBlob: (parts: readonly Uint8Array<ArrayBuffer>[]) => Blob
  readonly publish: (name: string, blob: Blob) => void
  readonly observeAssembly?: (snapshot: PortableDownloadAssemblySnapshot) => void
}

export interface PortableDownloadAssemblySnapshot {
  readonly bufferedBytes: number
  readonly retainedParts: number
  readonly rejectedWriteBytes: number
}

export interface PortableDownloadWindow {
  readonly Blob: typeof Blob
  readonly WritableStream: typeof WritableStream
  readonly URL: Pick<typeof URL, 'createObjectURL' | 'revokeObjectURL'>
  readonly document: Document
  readonly setTimeout: Window['setTimeout']
}

export function browserSupportsPortableDownload(
  windowPort: PortableDownloadWindow,
): boolean {
  return typeof windowPort.Blob === 'function' &&
    typeof windowPort.WritableStream === 'function' &&
    typeof windowPort.URL?.createObjectURL === 'function' &&
    typeof windowPort.URL?.revokeObjectURL === 'function' &&
    typeof windowPort.document?.createElement === 'function' &&
    windowPort.document.documentElement !== null &&
    typeof windowPort.setTimeout === 'function'
}

export function createPortableBrowserDownload(
  name: string,
  minimumBytes: bigint,
  windowPort: PortableDownloadWindow,
): WritableStream<Uint8Array> {
  if (!browserSupportsPortableDownload(windowPort)) {
    throw new DOMException('Portable browser downloads are unavailable', 'NotSupportedError')
  }
  if (minimumBytes < 0n) throw new RangeError('Portable download size must not be negative')
  if (minimumBytes > BigInt(PORTABLE_DOWNLOAD_MAXIMUM_BYTES)) {
    throw new DOMException(portableDownloadLimitMessage(), 'NotSupportedError')
  }
  return createBoundedPortableDownloadStream(name, {
    createBlob: (parts) => new windowPort.Blob([...parts], {
      type: name.toLowerCase().endsWith('.zip') ? 'application/zip' : 'application/octet-stream',
    }),
    publish: (suggestedName, blob) => publishBlob(windowPort, suggestedName, blob),
  })
}

export function createBoundedPortableDownloadStream(
  name: string,
  ports: PortableDownloadPorts,
  maximumBytes = PORTABLE_DOWNLOAD_MAXIMUM_BYTES,
): WritableStream<Uint8Array> {
  if (name.length === 0) throw new TypeError('Portable download requires a file name')
  if (!Number.isSafeInteger(maximumBytes) || maximumBytes <= 0) {
    throw new RangeError('Portable download byte limit must be a positive safe integer')
  }
  if (maximumBytes > PORTABLE_DOWNLOAD_MAXIMUM_BYTES) {
    throw new RangeError('Portable download byte limit exceeds its fixed assembly bound')
  }
  let parts: Uint8Array<ArrayBuffer>[] = []
  let pending: Uint8Array<ArrayBuffer> | undefined
  let pendingBytes = 0
  let bufferedBytes = 0
  return new WritableStream<Uint8Array>({
    write(chunk) {
      if (!(chunk instanceof Uint8Array)) {
        throw new TypeError('Portable download accepts only byte chunks')
      }
      if (chunk.byteLength > maximumBytes - bufferedBytes) {
        observe(ports, bufferedBytes, parts.length + (pending === undefined ? 0 : 1), chunk.byteLength)
        parts = []
        pending = undefined
        pendingBytes = 0
        bufferedBytes = 0
        throw new DOMException(portableDownloadLimitMessage(maximumBytes), 'QuotaExceededError')
      }
      let consumed = 0
      while (consumed < chunk.byteLength) {
        pending ??= new Uint8Array(PORTABLE_DOWNLOAD_PART_BYTES)
        const copied = Math.min(
          chunk.byteLength - consumed,
          PORTABLE_DOWNLOAD_PART_BYTES - pendingBytes,
        )
        pending.set(chunk.subarray(consumed, consumed + copied), pendingBytes)
        consumed += copied
        pendingBytes += copied
        if (pendingBytes === PORTABLE_DOWNLOAD_PART_BYTES) {
          parts.push(pending)
          pending = undefined
          pendingBytes = 0
        }
      }
      bufferedBytes += chunk.byteLength
      observe(ports, bufferedBytes, parts.length + (pending === undefined ? 0 : 1), 0)
    },
    close() {
      let blob: Blob
      try {
        if (pending !== undefined && pendingBytes > 0) parts.push(pending.slice(0, pendingBytes))
        if (parts.length > PORTABLE_DOWNLOAD_MAXIMUM_PARTS) {
          throw new Error('Portable download assembler exceeded its fixed part bound')
        }
        blob = ports.createBlob(parts)
      } finally {
        parts = []
        pending = undefined
        pendingBytes = 0
        bufferedBytes = 0
      }
      ports.publish(name, blob)
    },
    abort() {
      parts = []
      pending = undefined
      pendingBytes = 0
      bufferedBytes = 0
    },
  })
}

function observe(
  ports: PortableDownloadPorts,
  bufferedBytes: number,
  retainedParts: number,
  rejectedWriteBytes: number,
): void {
  ports.observeAssembly?.(Object.freeze({ bufferedBytes, retainedParts, rejectedWriteBytes }))
}

export function portableDownloadLimitMessage(
  maximumBytes = PORTABLE_DOWNLOAD_MAXIMUM_BYTES,
): string {
  return `Portable browser downloads are limited to ${maximumBytes / MEBIBYTE} MiB`
}

function publishBlob(windowPort: PortableDownloadWindow, name: string, blob: Blob): void {
  const objectUrl = windowPort.URL.createObjectURL(blob)
  const anchor = windowPort.document.createElement('a')
  anchor.download = name
  anchor.href = objectUrl
  anchor.hidden = true
  try {
    windowPort.document.documentElement.append(anchor)
    anchor.click()
  } catch (error) {
    windowPort.URL.revokeObjectURL(objectUrl)
    throw error
  } finally {
    anchor.remove()
  }
  // Revoking synchronously can cancel the download in Firefox/WebKit. The Blob is
  // still bounded, and the browser also releases the URL if the document closes.
  try {
    windowPort.setTimeout(
      () => windowPort.URL.revokeObjectURL(objectUrl),
      OBJECT_URL_RETENTION_MILLISECONDS,
    )
  } catch (error) {
    windowPort.URL.revokeObjectURL(objectUrl)
    throw error
  }
}
