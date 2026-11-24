import { bigintToSafeNumber } from '../../content/geometry'
import { classifyNativeError, isPortableLimitExceededError, portableLimitExceededError } from '../../transfer/fault'
import type { SettledTransferJobResult } from '../../transfer/job'
import type {
  TransferDirectoryRequest,
  TransferFileRequest,
  TransferOutputFile,
  TransferOutputSession,
} from '../../transfer/output-port'
import { transitionReceiveState, type ReceiveFailureReason } from '../../transfer/state'
import { emitOutputTrace, type OutputDiagnosticsPorts, type OutputTraceSource } from '../diagnostics'
import type { ReceiveOperationDisplay } from '../model/operation-display'
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
  mergeSourceOmittedEntries,
  resolveZipEntryPath,
  type OutputSessionHandle,
  type StartBackendTaskInput,
} from './contract'

const BROWSER_DOWNLOADS_DESTINATION = Object.freeze({ kind: 'browser-downloads' as const })

export interface OpenPortableSessionInput {
  readonly task: StartBackendTaskInput
  readonly windowPort: BrowserDownloadWindow
  readonly store?: ReceiveStore
  readonly maxBytes?: bigint
  readonly diagnostics?: OutputDiagnosticsPorts
}

/**
 * Builds the terminal portable handoff record shared by completed transfers and
 * preview-blob fast-path saves so browser history behaves identically regardless of timing.
 */
export function createCompletedPortableRecord(input: Readonly<{
  task: StartBackendTaskInput
  now?: number
}>): ReceiveTaskRecord {
  const now = input.now ?? Date.now()
  const started = transitionReceiveState(input.task.lifecycle, {
    kind: 'download-started',
    retained: false,
    fileName: input.task.artifact.name,
  }, now)
  const display = portableDisplay(input.task.display, input.task.artifact.name, now)
  return Object.freeze({
    taskId: input.task.taskId,
    shareInstance: input.task.selection.shareInstance,
    selectionKey: input.task.selectionKey,
    selection: input.task.selection,
    artifact: input.task.artifact,
    backend: 'portable',
    lifecycle: started,
    target: Object.freeze({ kind: 'portable', fileName: input.task.artifact.name }),
    ...(display === undefined ? {} : { display }),
    discoveryComplete: true,
    updatedAt: now,
  })
}

export function handoffLocalPortableBlob(input: Readonly<{
  task: StartBackendTaskInput
  blob: Blob
  windowPort: BrowserDownloadWindow
  trace?: OutputTraceSource
  now?: number
}>): ReceiveTaskRecord {
  const mediaType = artifactDownloadMediaType(input.task.artifact.kind)
  triggerBrowserDownload(input.windowPort, input.blob, input.task.artifact.name, mediaType)
  emitOutputTrace(input.trace, 'publication', {
    backend: 'portable',
    task_id: input.task.taskId,
    transition: 'committed',
  })
  return createCompletedPortableRecord({
    task: input.task,
    ...(input.now === undefined ? {} : { now: input.now }),
  })
}

export async function persistPortableHistory(
  store: ReceiveStore | undefined,
  record: ReceiveTaskRecord,
  trace?: OutputTraceSource,
): Promise<void> {
  if (store === undefined) return
  await store.putTask(record).catch((error: unknown) => {
    emitOutputTrace(trace, 'publication', {
      backend: 'portable',
      task_id: record.taskId,
      transition: 'history_persist_failed',
      error_class: classifyNativeError(error),
    })
  })
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
  const display = portableDisplay(input.task.display)

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
        resumable: false,
      }),
      ...(isZip ? { ensureDirectory: (req: TransferDirectoryRequest) => this.#ensureDir(req) } : {}),
      openFile: (request: TransferFileRequest, signal: AbortSignal) => this.#openFile(request, signal),
    })
  }

  async settle(result: SettledTransferJobResult): Promise<ReceiveTaskRecord> {
    this.#settled = true
    const sourceOmittedEntries = mergeSourceOmittedEntries(
      result.sourceOmittedEntries,
      undefined,
      result.discoveryComplete,
    )
    if (result.status !== 'completed') {
      await this.#zip?.abort()
      this.#singleFileBuffer = undefined
      const reason = failedReasonFor(result)
      const failed = transitionReceiveState(this.#baseRecord.lifecycle, {
        kind: 'failed',
        reason,
        ...(reason === 'error' && result.error instanceof Error ? { detail: result.error.message } : {}),
      })
      emitOutputTrace(this.#diagnostics?.trace, 'settlement', {
        backend: 'portable',
        task_id: this.taskId,
        transition: 'failed',
        reason,
      })
      const record: ReceiveTaskRecord = Object.freeze({
        ...this.#baseRecord,
        lifecycle: failed,
        ...(sourceOmittedEntries === undefined ? {} : { sourceOmittedEntries }),
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
      ...(sourceOmittedEntries === undefined ? {} : { source_omitted_entries: String(sourceOmittedEntries) }),
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
      ...(sourceOmittedEntries === undefined ? {} : { sourceOmittedEntries }),
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
      throw portableLimitExceededError(this.#maxBytes)
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
        checkpoint: () => Promise.resolve('skipped' as const),
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
      writeAt: async (offset: bigint, data: Uint8Array) => {
        const start = bigintToSafeNumber(offset, 'portable write offset')
        if (offset + BigInt(data.byteLength) > this.#singleFileSize) {
          throw new RangeError('Portable write exceeds declared file size')
        }
        buffer.set(data, start)
      },
      checkpoint: () => Promise.resolve('skipped' as const),
      commit: () => Promise.resolve(),
      pause: () => Promise.resolve(),
      abort: async () => { this.#singleFileBuffer = undefined },
    })
  }

  async #persistHistory(record: ReceiveTaskRecord): Promise<void> {
    await persistPortableHistory(this.#store, record, this.#diagnostics?.trace)
  }
}

function portableDisplay(
  display: ReceiveOperationDisplay | undefined,
  fallbackLabel?: string,
  now = Date.now(),
): ReceiveOperationDisplay | undefined {
  if (display !== undefined) {
    return Object.freeze({ ...display, destinationLabel: BROWSER_DOWNLOADS_DESTINATION })
  }
  if (fallbackLabel === undefined) return undefined
  return Object.freeze({
    objectLabel: fallbackLabel,
    destinationLabel: BROWSER_DOWNLOADS_DESTINATION,
    createdAtMilliseconds: now,
  })
}

function failedReasonFor(result: SettledTransferJobResult): ReceiveFailureReason {
  if (isPortableLimitExceededError(result.error)) return 'portable-limit-exceeded'
  if (result.status === 'source-changed') return 'source-changed'
  if (result.status === 'stopped') return 'cancelled'
  return 'error'
}
