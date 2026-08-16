export const ZIP_OUTPUT_METADATA_BUFFER_BYTES = 256 * 1024

/**
 * Keeps small ZIP framing records behind one bounded backpressure boundary.
 * File payloads still cross the sink immediately, while adjacent headers and
 * descriptors no longer turn large directory trees into millions of stream jobs.
 */
export class ZipOutputSink {
  readonly #writer: WritableStreamDefaultWriter<Uint8Array>
  readonly #pending: Uint8Array[] = []
  #pendingBytes = 0

  constructor(output: WritableStream<Uint8Array>) {
    if (output.locked) throw new TypeError('ZIP output stream is already locked')
    this.#writer = output.getWriter()
  }

  async appendMetadata(chunk: Uint8Array): Promise<void> {
    requireBytes(chunk)
    if (chunk.byteLength > ZIP_OUTPUT_METADATA_BUFFER_BYTES) {
      throw new RangeError('ZIP metadata chunk exceeds the bounded stream write')
    }
    if (chunk.byteLength === ZIP_OUTPUT_METADATA_BUFFER_BYTES) {
      await this.flush()
      await this.#writer.write(chunk)
      return
    }
    if (this.#pendingBytes + chunk.byteLength > ZIP_OUTPUT_METADATA_BUFFER_BYTES) {
      await this.flush()
    }
    this.#pending.push(chunk)
    this.#pendingBytes += chunk.byteLength
    if (this.#pendingBytes === ZIP_OUTPUT_METADATA_BUFFER_BYTES) await this.flush()
  }

  async writePayload(chunk: Uint8Array): Promise<void> {
    requireBytes(chunk)
    await this.flush()
    await this.#writer.write(chunk)
  }

  async close(): Promise<void> {
    await this.flush()
    await this.#writer.close()
  }

  abort(reason: unknown): Promise<void> {
    this.#discardPending()
    return this.#writer.abort(reason)
  }

  releaseLock(): void {
    this.#writer.releaseLock()
  }

  async flush(): Promise<void> {
    if (this.#pendingBytes === 0) return
    const chunk = concatenate(this.#pending, this.#pendingBytes)
    // Once handed to the stream, these bytes belong to that write even if its
    // promise rejects; retaining them would make retry or abort semantics ambiguous.
    this.#discardPending()
    await this.#writer.write(chunk)
  }

  #discardPending(): void {
    this.#pending.length = 0
    this.#pendingBytes = 0
  }
}

function requireBytes(chunk: Uint8Array): void {
  if (!(chunk instanceof Uint8Array) || chunk.byteLength === 0) {
    throw new TypeError('ZIP output chunk must contain bytes')
  }
}

function concatenate(chunks: readonly Uint8Array[], byteLength: number): Uint8Array {
  const output = new Uint8Array(byteLength)
  let offset = 0
  for (const chunk of chunks) {
    output.set(chunk, offset)
    offset += chunk.byteLength
  }
  return output
}
