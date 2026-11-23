import { bigintToSafeNumber } from '../../content/geometry'
import type { TransferJobResult } from '../../transfer/job'
import type {
  TransferDirectoryRequest,
  TransferFileRequest,
  TransferOutputFile,
  TransferOutputSession,
} from '../../transfer/output-port'
import { transitionReceiveState } from '../../transfer/state'
import { emitOutputTrace, type OutputDiagnosticsPorts } from '../diagnostics'
import type { ReceiveStore } from '../store/database'
import type { ReceiveTaskRecord } from '../store/types'
import { StreamingZipWriter } from '../zip/streaming-zip'
import {
  artifactDownloadMediaType,
  triggerBrowserDownload,
  type BrowserDownloadWindow,
} from './browser-handoff'
import {
  PORTABLE_MAX_BYTES,
  resolveZipEntryPath,
  type OutputSessionHandle,
  type StartBackendTaskInput,
} from './contract'

export interface OpenPortableSessionInput {
  readonly task: StartBackendTaskInput
  readonly windowPort: BrowserDownloadWindow
  readonly store?: ReceiveStore
  readonly maxBytes?: bigint
  readonly diagnostics?: OutputDiagnosticsPorts
}

/**
 * In-memory Blob / streaming ZIP backend for small files and fallback environments
 * where FSA and OPFS are unavailable. Hands the finished result directly to the browser.
 */
export function openPortableSession(input: OpenPortableSessionInput): OutputSessionHandle {
  const maxBytes = input.maxBytes ?? PORTABLE_MAX_BYTES
  const isZip = input.task.artifact.kind === 'zip-archive'
  const downloading = transitionReceiveState(input.task.lifecycle, {
    kind: 'downloading',
    phase: 'receiving',
  })
  const display = input.task.display === undefined
    ? undefined
    : Object.freeze({ ...input.task.display, destinationLabel: Object.freeze({ kind: 'browser-downloads' as const }) })

  const baseRecord: ReceiveTaskRecord = Object.freeze({
    taskId: input.task.taskId,
    shareInstance: input.task.selection.shareInstance,
    selectionKey: input.task.selectionKey,
    selection: input.task.selection,
    artifact: input.task.artifact,
    backend: 'portable',
    lifecycle: downloading,
    target: Object.freeze({ kind: 'portable', fileName: input.task.artifact.name }),
    ...(display === undefined ? {} : { display }),
    discoveryComplete: false,
    updatedAt: Date.now(),
  })

  return new PortableSessionHandle(baseRecord, isZip, maxBytes, input.windowPort, input.store, input.diagnostics)
}

class PortableSessionHandle implements OutputSessionHandle {
  readonly taskId: string
  readonly backend = 'portable' as const
  readonly transferOutput: TransferOutputSession
  readonly #baseRecord: ReceiveTaskRecord
  readonly #isZip: boolean
  readonly #maxBytes: bigint
  readonly #windowPort: BrowserDownloadWindow
  readonly #store: ReceiveStore | undefined
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  readonly #zip: StreamingZipWriter | undefined
  #singleFileBuffer: Uint8Array<ArrayBuffer> | undefined
  #singleFileSize = 0n
  #settled = false

  constructor(
    baseRecord: ReceiveTaskRecord,
    isZip: boolean,
    maxBytes: bigint,
    windowPort: BrowserDownloadWindow,
    store?: ReceiveStore,
    diagnostics?: OutputDiagnosticsPorts,
  ) {
    this.taskId = baseRecord.taskId
    this.#baseRecord = baseRecord
    this.#isZip = isZip
    this.#maxBytes = maxBytes
    this.#windowPort = windowPort
    this.#store = store
    this.#diagnostics = diagnostics
    if (isZip) {
      this.#zip = new StreamingZipWriter({
        taskId: baseRecord.taskId,
        maxBytes,
        ...(store === undefined ? {} : { store }),
      })
    }
    this.transferOutput = Object.freeze({
      sessionId: baseRecord.taskId,
      capabilities: Object.freeze({
        // Sequential streaming ZIP writes one member at a time to avoid extra buffering.
        maxConcurrentFiles: 1,
      }),
      ...(isZip ? { ensureDirectory: (req: TransferDirectoryRequest) => this.#ensureDir(req) } : {}),
      openFile: (request: TransferFileRequest, signal: AbortSignal) => this.#openFile(request, signal),
    })
  }

  async settle(result: TransferJobResult): Promise<ReceiveTaskRecord> {
    this.#settled = true
    if (result.status !== 'completed') {
      await this.#zip?.abort()
      this.#singleFileBuffer = undefined
      const reason = failedReasonFor(result.status)
      const failed = transitionReceiveState(this.#baseRecord.lifecycle, {
        kind: 'failed',
        reason,
        ...(result.error instanceof Error ? { detail: result.error.message } : {}),
      })
      const record: ReceiveTaskRecord = Object.freeze({
        ...this.#baseRecord,
        lifecycle: failed,
        ...(result.sourceRevisionFailures === undefined
          ? {}
          : { sourceRevisionFailures: result.sourceRevisionFailures }),
        updatedAt: Date.now(),
      })
      await this.#persistHistory(record)
      return record
    }

    const mediaType = artifactDownloadMediaType(this.#baseRecord.artifact.kind)
    const parts = this.#singleFileBuffer === undefined ? [] : [this.#singleFileBuffer]
    const blob = this.#zip !== undefined
      ? await this.#zip.finalize()
      : new Blob(parts, { type: mediaType })
    this.#singleFileBuffer = undefined

    triggerBrowserDownload(this.#windowPort, blob, this.#baseRecord.artifact.name, mediaType)
    emitOutputTrace(this.#diagnostics?.trace, 'publication', {
      backend: 'portable',
      transition: 'committed',
    })

    const started = transitionReceiveState(this.#baseRecord.lifecycle, {
      kind: 'download-started',
      retained: false,
      fileName: this.#baseRecord.artifact.name,
      contentWarning: result.contentWarning ?? null,
    })
    const record: ReceiveTaskRecord = Object.freeze({
      ...this.#baseRecord,
      lifecycle: started,
      discoveryComplete: true,
      updatedAt: Date.now(),
    })
    await this.#persistHistory(record)
    return record
  }

  async close(): Promise<void> {
    if (!this.#settled) {
      await this.#zip?.abort()
      this.#singleFileBuffer = undefined
    }
  }

  async #ensureDir(request: TransferDirectoryRequest): Promise<void> {
    const zipPath = resolveZipEntryPath(this.#baseRecord.artifact, request.path)
    if (zipPath === undefined) return
    await this.#zip?.addDirectory(zipPath, request.modifiedTimeMilliseconds)
  }

  async #openFile(request: TransferFileRequest, signal: AbortSignal): Promise<TransferOutputFile> {
    signal.throwIfAborted()
    if (request.exactSize > this.#maxBytes) {
      throw new DOMException(
        `Portable browser download is limited to ${Number(this.#maxBytes / (1024n * 1024n))} MiB`,
        'NotSupportedError',
      )
    }

    if (this.#isZip && this.#zip !== undefined) {
      const zipPath = resolveZipEntryPath(this.#baseRecord.artifact, request.path)
      if (zipPath === undefined) {
        throw new TypeError('ZIP file entry requires a non-empty path')
      }
      const zipFile = this.#zip.beginFile(zipPath, request.exactSize, request.modifiedTimeMilliseconds)
      let expectedOffset = 0n
      const outOfOrder = new Map<bigint, Uint8Array>()
      const discard = async () => {
        outOfOrder.clear()
        zipFile.abort()
      }
      return Object.freeze({
        completedRanges: Object.freeze([]),
        alreadyCompleted: false,
        writeAt: async (offset: bigint, data: Uint8Array) => {
          if (offset === expectedOffset) {
            zipFile.write(data)
            expectedOffset += BigInt(data.byteLength)
            while (outOfOrder.has(expectedOffset)) {
              const next = outOfOrder.get(expectedOffset)!
              outOfOrder.delete(expectedOffset)
              zipFile.write(next)
              expectedOffset += BigInt(next.byteLength)
            }
          } else {
            outOfOrder.set(offset, data.slice())
          }
        },
        checkpoint: () => Promise.resolve(),
        commit: async () => {
          await zipFile.close()
        },
        pause: discard,
        abort: discard,
      })
    }

    const sizeNumber = bigintToSafeNumber(request.exactSize, 'portable file size')
    const buffer = new Uint8Array(sizeNumber)
    this.#singleFileBuffer = buffer
    this.#singleFileSize = request.exactSize
    return Object.freeze({
      completedRanges: Object.freeze([]),
      alreadyCompleted: false,
      writeAt: async (offset: bigint, data: Uint8Array) => {
        const start = bigintToSafeNumber(offset, 'portable write offset')
        if (offset + BigInt(data.byteLength) > this.#singleFileSize) {
          throw new RangeError('Portable write exceeds declared file size')
        }
        buffer.set(data, start)
      },
      checkpoint: () => Promise.resolve(),
      commit: () => Promise.resolve(),
      pause: () => Promise.resolve(),
      abort: async () => { this.#singleFileBuffer = undefined },
    })
  }

  async #persistHistory(record: ReceiveTaskRecord): Promise<void> {
    if (this.#store === undefined) return
    await this.#store.putTask(record).catch(() => undefined)
  }
}

function failedReasonFor(status: TransferJobResult['status']): 'source-changed' | 'cancelled' | 'error' {
  if (status === 'source-changed') return 'source-changed'
  if (status === 'stopped') return 'cancelled'
  return 'error'
}
