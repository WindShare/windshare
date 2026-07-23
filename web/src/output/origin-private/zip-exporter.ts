import {
  ORIGIN_PRIVATE_EXPORT_COMPLETE,
  type OriginPrivateExportResult,
  type OriginPrivateOutputExporter,
} from './session'
import type { StagedOutputCatalog, StagedOutputFile } from '../persistent-tree/session'
import type { ZipArchiveWriter } from '../streams/zip-archive'
import { StreamingZipArchiveWriter } from '../streams/streaming-zip'
import {
  IndexedDbZipCentralDirectorySpool,
  type ZipCentralDirectorySpool,
} from '../streams/zip-spool'

export class OriginPrivateZipExporter implements OriginPrivateOutputExporter {
  readonly #output: WritableStream<Uint8Array>
  readonly #createSpool: () => ZipCentralDirectorySpool
  #state: 'idle' | 'exporting' | 'settled' | 'aborting' | 'aborted' | 'abort-failed' = 'idle'
  #controller: AbortController | undefined
  #activeArchive: StreamingZipArchiveWriter | undefined
  #activeReader: ReadableStreamDefaultReader<Uint8Array> | undefined
  #abortPromise: Promise<void> | undefined
  #cleanupArchive: StreamingZipArchiveWriter | undefined

  constructor(
    output: WritableStream<Uint8Array>,
    createSpool: () => ZipCentralDirectorySpool = () => new IndexedDbZipCentralDirectorySpool(),
  ) {
    this.#output = output
    this.#createSpool = createSpool
  }

  abort(reason: unknown): Promise<void> {
    if (this.#state === 'settled') return Promise.resolve()
    if (this.#abortPromise !== undefined) return this.#abortPromise
    this.#state = 'aborting'
    this.#controller?.abort(reason)
    const operation = this.#performAbort(reason).then(
      () => {
        if (this.#state !== 'settled') this.#state = 'aborted'
      },
      (error: unknown) => {
        if (this.#state !== 'settled') this.#state = 'abort-failed'
        throw error
      },
    )
    this.#abortPromise = operation
    // The signal listener can start cancellation before session authority calls
    // abort(). Attach an observer without replacing the rejecting shared promise.
    operation.catch(() => undefined)
    return operation
  }

  async #performAbort(reason: unknown): Promise<void> {
    const results = await Promise.allSettled([
      this.#activeReader?.cancel(reason) ?? Promise.resolve(),
      this.#activeArchive?.abort(reason) ??
        (!this.#output.locked ? this.#output.abort(reason) : Promise.resolve()),
    ])
    const failures = results
      .filter((result): result is PromiseRejectedResult => result.status === 'rejected')
      .map((result) => result.reason)
    if (failures.length > 0) throw new AggregateError(failures, 'OPFS ZIP export abort failed')
  }

  async export(
    catalog: StagedOutputCatalog,
    outcome: Parameters<OriginPrivateOutputExporter['export']>[1],
    signal: AbortSignal,
  ): Promise<OriginPrivateExportResult> {
    if (this.#state !== 'idle') throw new Error('OPFS ZIP exporter is not idle')
    if (outcome.status === 'Aborted') throw new Error('Cannot export an aborted output job')
    signal.throwIfAborted()
    this.#state = 'exporting'
    const controller = new AbortController()
    this.#controller = controller
    const detach = forwardAbort(signal, controller)
    const exportSignal = controller.signal
    const interrupt = () => {
      this.abort(exportSignal.reason)
    }
    try {
      // Resource construction belongs to the same fail-closed settlement as data
      // export; spool or writer acquisition can fail before an archive exists.
      const archive = new StreamingZipArchiveWriter(this.#output, this.#createSpool())
      this.#activeArchive = archive
      exportSignal.addEventListener('abort', interrupt, { once: true })
      for await (const directory of catalog.directories()) {
        exportSignal.throwIfAborted()
        await archive.addDirectory({
          path: directory.canonicalPath,
          ...(directory.modifiedTimeMilliseconds === undefined
            ? {}
            : { modifiedTimeMilliseconds: directory.modifiedTimeMilliseconds }),
        })
      }
      for await (const staged of catalog.files()) {
        exportSignal.throwIfAborted()
        await exportFile(archive, staged, exportSignal, (reader) => {
          this.#activeReader = reader
        })
      }
      exportSignal.throwIfAborted()
      await archive.close(exportSignal)
      this.#state = 'settled'
      if (archive.cleanupPending) this.#cleanupArchive = archive
      return archive.cleanupPending
        ? Object.freeze({ cleanupPending: true, cleanupFailure: archive.cleanupFailure })
        : ORIGIN_PRIVATE_EXPORT_COMPLETE
    } catch (error) {
      try {
        await this.abort(error)
      } catch (abortError) {
        throw new AggregateError(
          [error, abortError],
          'OPFS ZIP export and cleanup failed',
          { cause: abortError },
        )
      }
      throw error
    } finally {
      exportSignal.removeEventListener('abort', interrupt)
      detach()
      this.#controller = undefined
      this.#activeArchive = undefined
      this.#activeReader = undefined
    }
  }

  async retryCleanup(): Promise<OriginPrivateExportResult> {
    const archive = this.#cleanupArchive
    if (archive === undefined) return ORIGIN_PRIVATE_EXPORT_COMPLETE
    try {
      await archive.retryCleanup()
    } catch (error) {
      return Object.freeze({ cleanupPending: true, cleanupFailure: error })
    }
    if (archive.cleanupPending) {
      return Object.freeze({ cleanupPending: true, cleanupFailure: archive.cleanupFailure })
    }
    this.#cleanupArchive = undefined
    return ORIGIN_PRIVATE_EXPORT_COMPLETE
  }
}

async function exportFile(
  archive: ZipArchiveWriter,
  staged: StagedOutputFile,
  signal: AbortSignal,
  setReader: (reader: ReadableStreamDefaultReader<Uint8Array> | undefined) => void,
): Promise<void> {
  signal.throwIfAborted()
  const blob = await staged.read()
  signal.throwIfAborted()
  if (BigInt(blob.size) !== staged.record.exactSize) {
    throw new Error('Staged output size changed before export')
  }
  const member = await archive.beginFile({
    path: staged.record.canonicalPath,
    exactSize: staged.record.exactSize,
    ...(staged.record.modifiedTimeMilliseconds === undefined
      ? {}
      : { modifiedTimeMilliseconds: staged.record.modifiedTimeMilliseconds }),
  })
  const reader = blob.stream().getReader()
  setReader(reader)
  try {
    while (true) {
      signal.throwIfAborted()
      const chunk = await reader.read()
      if (chunk.done) break
      signal.throwIfAborted()
      await member.write(chunk.value)
    }
    signal.throwIfAborted()
    await member.close()
  } catch (error) {
    await member.abort(error)
    throw error
  } finally {
    setReader(undefined)
    reader.releaseLock()
  }
}

function forwardAbort(source: AbortSignal, target: AbortController): () => void {
  const abort = () => target.abort(
    source.reason ?? new DOMException('OPFS ZIP export aborted', 'AbortError'),
  )
  if (source.aborted) {
    abort()
    return () => {}
  }
  source.addEventListener('abort', abort, { once: true })
  return () => source.removeEventListener('abort', abort)
}
