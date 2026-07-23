import {
  type BeginOutputFileResult,
  type FileAbortDisposition,
  type OutputCapabilities,
  type OutputDirectory,
  type OutputFile,
  type OutputFileTransaction,
  type OutputSession,
  type OutputSessionIdentity,
  VerifiedDurableRanges,
  outputCapabilities,
  outputSessionIdentity,
  snapshotOutputDirectory,
  snapshotOutputFile,
} from '../../transfer/output-session'
import type { JobOutcome } from '../../transfer/outcome'

export const SINGLE_FILE_STREAM_BACKEND = 'single-file-stream'

type StreamState = 'open' | 'closing' | 'committed' | 'aborting' | 'aborted' | 'failed'

export class SingleFileStreamOutputSession implements OutputSession {
  readonly identity: OutputSessionIdentity
  readonly capabilities: OutputCapabilities = outputCapabilities({
    durability: 'None',
    randomWrite: false,
    fileFailureIsolation: false,
    modificationTime: false,
  })

  readonly #writer: WritableStreamDefaultWriter<Uint8Array>
  #state: StreamState = 'open'
  #transaction: SingleFileStreamTransaction | undefined
  #closePromise: Promise<void> | undefined
  #abortPromise: Promise<void> | undefined
  #settlementFailure: unknown
  #writerReleased = false

  constructor(outputSessionId: string, output: WritableStream<Uint8Array>) {
    if (output.locked) throw new TypeError('Single-file output stream is already locked')
    this.identity = outputSessionIdentity({
      backend: SINGLE_FILE_STREAM_BACKEND,
      outputSessionId,
    })
    this.#writer = output.getWriter()
  }

  async ensureDirectory(): Promise<void> {
    this.#requireOpen()
  }

  async finalizeDirectory(directory: OutputDirectory, signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    this.#requireOpen()
    snapshotOutputDirectory(directory)
  }

  async beginFile(input: OutputFile): Promise<BeginOutputFileResult> {
    this.#requireOpen()
    if (this.#transaction !== undefined) {
      throw new Error('Single-file output accepts exactly one file')
    }
    const file = snapshotOutputFile(input)
    const ownership = Object.freeze({
      ...this.identity,
      canonicalPath: file.path,
      ownedFileIdentity: `${this.identity.outputSessionId}:stream`,
    })
    const transaction = new SingleFileStreamTransaction(this, file, ownership)
    this.#transaction = transaction
    return Object.freeze({
      transaction,
      durableRanges: new VerifiedDurableRanges(ownership, file.source, file.exactSize, []),
    })
  }

  async finishJob(outcome: JobOutcome, signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    if (outcome.status === 'Aborted') throw new Error('Cannot finish an aborted output job')
    if (this.#state === 'committed') return
    if (this.#state === 'closing') return this.#closePromise
    if (this.#state !== 'open') return
    if (this.#transaction !== undefined && !this.#transaction.settled) {
      throw new Error('Cannot finish single-file output while its file is active')
    }
    if (this.#transaction === undefined) {
      const detach = interruptOnAbort(signal, (reason) => this.abortJob(reason))
      try {
        await this.commitOutput()
      } finally {
        detach()
      }
    }
  }

  abortJob(reason: unknown): Promise<void> {
    return this.abortOutput(reason)
  }

  async writeOutput(data: Uint8Array): Promise<void> {
    this.#requireOpen()
    await this.#writer.write(data)
  }

  commitOutput(): Promise<void> {
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

  abortOutput(reason: unknown): Promise<void> {
    if (this.#state === 'committed' || this.#state === 'aborted') return Promise.resolve()
    if (this.#abortPromise !== undefined) return this.#abortPromise
    if (this.#state === 'failed') return Promise.reject(this.#settlementFailure)
    const close = this.#closePromise
    this.#state = 'aborting'
    const operation = close === undefined
      ? this.#abortOpenOutput(reason)
      : this.#interruptClose(reason, close)
    this.#abortPromise = operation
    // Cancellation can be triggered by an AbortSignal listener with no direct
    // awaiter, while later callers still need the original rejecting promise.
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
        // Web Streams rejects a concurrent writer.abort() with the close error
        // after the failed close has already made the stream terminal.
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

  #releaseWriter(): void {
    if (this.#writerReleased) return
    this.#writerReleased = true
    this.#writer.releaseLock()
  }

  #requireOpen(): void {
    if (this.#state !== 'open') throw new Error('Single-file output session is not open')
  }
}

class SingleFileStreamTransaction implements OutputFileTransaction {
  readonly #session: SingleFileStreamOutputSession
  readonly #file: OutputFile
  readonly #ownership: ConstructorParameters<typeof VerifiedDurableRanges>[0]
  #tail: Promise<unknown> = Promise.resolve()
  #nextOffset = 0n
  #started = false
  #settled = false

  constructor(
    session: SingleFileStreamOutputSession,
    file: OutputFile,
    ownership: ConstructorParameters<typeof VerifiedDurableRanges>[0],
  ) {
    this.#session = session
    this.#file = file
    this.#ownership = ownership
  }

  get settled(): boolean {
    return this.#settled
  }

  writeRange(offset: bigint, data: Uint8Array): Promise<void> {
    const snapshot = data.slice()
    return this.#enqueue(async () => {
      this.#requireOpen()
      if (offset !== this.#nextOffset || offset + BigInt(snapshot.byteLength) > this.#file.exactSize) {
        throw new RangeError('Single-file stream requires contiguous ascending ranges')
      }
      if (snapshot.byteLength === 0) return
      // Once a write is attempted, a browser stream may have emitted a prefix even
      // if its promise later rejects, so rollback must be treated conservatively.
      this.#started = true
      await this.#session.writeOutput(snapshot)
      this.#nextOffset += BigInt(snapshot.byteLength)
    })
  }

  checkpoint(): Promise<VerifiedDurableRanges> {
    return this.#enqueue(async () => {
      this.#requireOpen()
      return new VerifiedDurableRanges(
        this.#ownership,
        this.#file.source,
        this.#file.exactSize,
        [],
      )
    })
  }

  commit(): Promise<void> {
    return this.#enqueue(async () => {
      this.#requireOpen()
      if (this.#nextOffset !== this.#file.exactSize) {
        throw new Error('Single-file stream is incomplete')
      }
      this.#started = true
      await this.#session.commitOutput()
      this.#settled = true
    })
  }

  abort(reason: unknown): Promise<FileAbortDisposition> {
    return this.#enqueue(async () => {
      if (this.#settled) return this.#started ? 'JobOutputCompromised' : 'FileIsolated'
      const disposition = this.#started ? 'JobOutputCompromised' : 'FileIsolated'
      try {
        await this.#session.abortOutput(reason)
      } finally {
        this.#settled = true
      }
      return disposition
    })
  }

  #enqueue<T>(operation: () => Promise<T>): Promise<T> {
    const result = this.#tail.then(operation, operation)
    this.#tail = result
    return result
  }

  #requireOpen(): void {
    if (this.#settled) throw new Error('Single-file stream transaction is settled')
  }
}

function interruptOnAbort(
  signal: AbortSignal,
  interrupt: (reason: unknown) => Promise<void>,
): () => void {
  const abort = () => {
    interrupt(signal.reason ?? new DOMException('Single-file output aborted', 'AbortError'))
      .catch(() => undefined)
  }
  signal.addEventListener('abort', abort, { once: true })
  return () => signal.removeEventListener('abort', abort)
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
