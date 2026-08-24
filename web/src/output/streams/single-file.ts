import {
  type BeginOutputFileResult,
  type FileRetirementDisposition,
  type OpenedOutputRevision,
  type OutputCapabilities,
  type OutputFileOwnership,
  type OutputFileRequest,
  type OutputFileTransaction,
  type OutputSession,
  type OutputSessionIdentity,
  OutputSessionBindingError,
  VerifiedDurableRanges,
  outputCapabilities,
  outputSessionIdentity,
  snapshotOpenedOutputRevision,
  snapshotOutputFileRequest,
} from '../../transfer/output-session'

export const SINGLE_FILE_STREAM_BACKEND = 'single-file-stream'

type SessionState = 'available' | 'opening-revision' | 'active' | 'failed'
type StreamState = 'open' | 'closing' | 'committed' | 'aborting' | 'aborted' | 'failed'

export class SingleFileStreamOutputSession implements OutputSession {
  readonly identity: OutputSessionIdentity
  readonly capabilities: OutputCapabilities = outputCapabilities({
    durability: 'None',
    randomWrite: false,
    fileFailureIsolation: false,
    modificationTime: false,
  })

  readonly #output: WritableStream<Uint8Array>
  #state: SessionState = 'available'

  constructor(outputSessionId: string, output: WritableStream<Uint8Array>) {
    if (output.locked) throw new TypeError('Single-file output stream is already locked')
    this.identity = outputSessionIdentity({
      backend: SINGLE_FILE_STREAM_BACKEND,
      outputSessionId,
    })
    this.#output = output
  }

  async beginFile(input: OutputFileRequest, signal: AbortSignal): Promise<BeginOutputFileResult> {
    signal.throwIfAborted()
    this.#requireAvailable()
    const request = snapshotOutputFileRequest(input)
    this.#state = 'opening-revision'

    try {
      const revision = snapshotOpenedOutputRevision(await request.openRevision(signal))
      signal.throwIfAborted()
      requireMatchingRevision(request, revision)

      const ownership: OutputFileOwnership = Object.freeze({
        ...this.identity,
        canonicalPath: request.materializationRelativePath,
        ownedFileIdentity: `${this.identity.outputSessionId}:stream`,
      })
      const durableRanges = new VerifiedDurableRanges(
        ownership,
        revision,
        revision.exactSize,
        [],
      )

      // Acquiring the writer is the first output-authority mutation. Keeping it
      // after revision authentication prevents failed opens from locking a target.
      if (this.#output.locked) {
        throw new TypeError('Single-file output stream became locked before revision authentication')
      }
      const writer = this.#output.getWriter()
      const transaction = new SingleFileStreamTransaction(writer, revision, ownership)
      this.#state = 'active'
      return Object.freeze({ revision, transaction, durableRanges })
    } catch (error) {
      this.#state = 'failed'
      throw error
    }
  }

  #requireAvailable(): void {
    if (this.#state !== 'available') {
      throw new Error('Single-file output accepts exactly one authenticated revision open')
    }
  }
}

class SingleFileStreamTransaction implements OutputFileTransaction {
  readonly #writer: WritableStreamDefaultWriter<Uint8Array>
  readonly #revision: OpenedOutputRevision
  readonly #ownership: OutputFileOwnership
  #tail: Promise<unknown> = Promise.resolve()
  #nextOffset = 0n
  #started = false
  #settled = false
  #state: StreamState = 'open'
  #closePromise: Promise<void> | undefined
  #abortPromise: Promise<void> | undefined
  #settlementFailure: unknown
  #writerReleased = false

  constructor(
    writer: WritableStreamDefaultWriter<Uint8Array>,
    revision: OpenedOutputRevision,
    ownership: OutputFileOwnership,
  ) {
    this.#writer = writer
    this.#revision = revision
    this.#ownership = ownership
  }

  writeRange(offset: bigint, data: Uint8Array, signal: AbortSignal): Promise<void> {
    const snapshot = data.slice()
    return this.#enqueue(async () => {
      signal.throwIfAborted()
      this.#requireOpen()
      if (offset !== this.#nextOffset || offset + BigInt(snapshot.byteLength) > this.#revision.exactSize) {
        throw new RangeError('Single-file stream requires contiguous ascending ranges')
      }
      if (snapshot.byteLength === 0) return
      // A rejected stream write may still have exposed a prefix, so retirement
      // must become job-scoped as soon as the write is attempted.
      this.#started = true
      await this.#writer.write(snapshot)
      signal.throwIfAborted()
      this.#nextOffset += BigInt(snapshot.byteLength)
    })
  }

  checkpoint(signal: AbortSignal): Promise<VerifiedDurableRanges> {
    return this.#enqueue(async () => {
      signal.throwIfAborted()
      this.#requireOpen()
      return new VerifiedDurableRanges(
        this.#ownership,
        this.#revision,
        this.#revision.exactSize,
        [],
      )
    })
  }

  commit(signal: AbortSignal): Promise<void> {
    return this.#enqueue(async () => {
      signal.throwIfAborted()
      this.#requireOpen()
      if (this.#nextOffset !== this.#revision.exactSize) {
        throw new Error('Single-file stream is incomplete')
      }
      this.#started = true
      await this.#commitOutput()
      this.#settled = true
      signal.throwIfAborted()
    })
  }

  retire(reason: unknown): Promise<FileRetirementDisposition> {
    return this.#enqueue(async () => {
      const disposition = this.#started ? 'JobOutputCompromised' : 'FileIsolated'
      if (this.#settled) return disposition
      try {
        await this.#abortOutput(reason)
      } finally {
        this.#settled = true
      }
      return disposition
    })
  }

  pause(reason: unknown): Promise<void> {
    return this.#enqueue(async () => {
      if (this.#settled) return
      try {
        await this.#abortOutput(reason)
      } finally {
        this.#settled = true
      }
    })
  }

  #enqueue<T>(operation: () => Promise<T>): Promise<T> {
    const result = this.#tail.then(operation, operation)
    this.#tail = result
    return result
  }

  #commitOutput(): Promise<void> {
    if (this.#state === 'committed') return Promise.resolve()
    if (this.#closePromise !== undefined) return this.#closePromise
    if (this.#state === 'failed') return Promise.reject(this.#settlementFailure)
    this.#requireOpen()
    this.#state = 'closing'
    const operation = this.#writer.close().then(
      () => { this.#state = 'committed' },
      (error: unknown) => {
        this.#settlementFailure = error
        if (this.#state === 'closing') this.#state = 'failed'
        throw error
      },
    ).finally(() => { this.#releaseWriter() })
    this.#closePromise = operation
    return operation
  }

  #abortOutput(reason: unknown): Promise<void> {
    if (this.#state === 'committed' || this.#state === 'aborted') return Promise.resolve()
    if (this.#abortPromise !== undefined) return this.#abortPromise
    if (this.#state === 'failed') return Promise.reject(this.#settlementFailure)
    const close = this.#closePromise
    this.#state = 'aborting'
    const operation = close === undefined
      ? this.#abortOpenOutput(reason)
      : this.#interruptClose(reason, close)
    this.#abortPromise = operation
    // Abort may start from a signal listener with no direct awaiter. Retaining the
    // original promise still lets a later settlement caller observe its failure.
    operation.catch(() => undefined)
    return operation
  }

  async #abortOpenOutput(reason: unknown): Promise<void> {
    try {
      await this.#writer.abort(reason)
      this.#state = 'aborted'
    } catch (error) {
      this.#state = 'failed'
      this.#settlementFailure = error
      throw error
    } finally {
      this.#releaseWriter()
    }
  }

  async #interruptClose(reason: unknown, close: Promise<void>): Promise<void> {
    const interrupt = writerAbort(this.#writer, reason)
    interrupt.catch(() => undefined)
    let closeFailure: unknown
    try {
      await close
    } catch (error) {
      closeFailure = error
    }
    if (this.#state === 'committed') {
      // Close is the publication boundary; a losing abort cannot revoke bytes
      // that the browser has already exposed to the receiver.
      await interrupt.catch(() => undefined)
      return
    }
    try {
      await interrupt
      this.#state = 'aborted'
    } catch (abortFailure) {
      if (closeFailure !== undefined && abortFailure === closeFailure) {
        // Web Streams can reject concurrent abort with the close failure after
        // the failed close has already made the stream terminal.
        this.#state = 'aborted'
        return
      }
      const failure = closeFailure === undefined
        ? abortFailure
        : new AggregateError([closeFailure, abortFailure], 'Single-file close and abort failed')
      this.#state = 'failed'
      this.#settlementFailure = failure
      throw failure
    }
  }

  #requireOpen(): void {
    if (this.#state !== 'open') throw new Error('Single-file output transaction is not open')
    if (this.#settled) throw new Error('Single-file output transaction is settled')
  }

  #releaseWriter(): void {
    if (this.#writerReleased) return
    this.#writerReleased = true
    this.#writer.releaseLock()
  }
}

function requireMatchingRevision(
  request: OutputFileRequest,
  revision: OpenedOutputRevision,
): void {
  if (revision.shareInstance !== request.source.shareInstance ||
      revision.fileId !== request.source.fileId) {
    throw new OutputSessionBindingError(
      'authenticated revision does not belong to the requested catalog file',
    )
  }
  if (revision.exactSize !== request.expectedSize) {
    throw new OutputSessionBindingError(
      'authenticated revision size does not match the requested output size',
    )
  }
}

function writerAbort(
  writer: WritableStreamDefaultWriter<Uint8Array>,
  reason: unknown,
): Promise<void> {
  try {
    return writer.abort(reason)
  } catch (error) {
    return Promise.reject(error)
  }
}
