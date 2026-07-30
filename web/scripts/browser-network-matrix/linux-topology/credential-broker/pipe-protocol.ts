export const MAXIMUM_BROKER_FRAME_BYTES = 1_048_576

const BROKER_METADATA_LENGTH_BYTES = 4

export class BoundedBrokerCapture {
  readonly #chunks: Buffer[] = []
  #byteLength = 0

  get byteLength(): number { return this.#byteLength }

  consume(chunk: Uint8Array): void {
    try {
      this.#byteLength += chunk.byteLength
      if (this.#byteLength > MAXIMUM_BROKER_FRAME_BYTES) {
        throw new Error('credential broker response exceeded its pipe authority')
      }
      this.#chunks.push(Buffer.from(chunk))
    } finally {
      // Containment transfers each secret-bearing chunk, so retaining its source
      // allocation would leave a second credential owner outside the broker.
      chunk.fill(0)
    }
  }

  take(): Buffer {
    const result = Buffer.concat(this.#chunks, this.#byteLength)
    this.erase()
    return result
  }

  erase(): void {
    for (const chunk of this.#chunks) chunk.fill(0)
    this.#chunks.length = 0
    this.#byteLength = 0
  }
}

export function encodeBrokerPipeFrame(
  metadata: Readonly<Record<string, unknown>>,
  payload: Uint8Array,
): Buffer {
  const metadataBytes = Buffer.from(`${JSON.stringify(metadata)}\n`, 'utf8')
  const total = BROKER_METADATA_LENGTH_BYTES + metadataBytes.byteLength + payload.byteLength
  if (metadataBytes.byteLength === 0 || total > MAXIMUM_BROKER_FRAME_BYTES) {
    throw new Error('credential broker request exceeded its anonymous pipe authority')
  }
  const frame = Buffer.alloc(total)
  frame.writeUInt32BE(metadataBytes.byteLength, 0)
  metadataBytes.copy(frame, BROKER_METADATA_LENGTH_BYTES)
  frame.set(payload, BROKER_METADATA_LENGTH_BYTES + metadataBytes.byteLength)
  return frame
}

export function parseBrokerPipeFrame(bytes: Buffer): {
  readonly metadata: unknown
  readonly payload: Uint8Array
} {
  if (bytes.byteLength <= BROKER_METADATA_LENGTH_BYTES) invalidBrokerResponse()
  const metadataByteLength = bytes.readUInt32BE(0)
  const payloadOffset = BROKER_METADATA_LENGTH_BYTES + metadataByteLength
  if (metadataByteLength === 0 || payloadOffset > bytes.byteLength) invalidBrokerResponse()
  let encoded: string
  try {
    encoded = new TextDecoder('utf-8', { fatal: true }).decode(
      bytes.subarray(BROKER_METADATA_LENGTH_BYTES, payloadOffset),
    )
  } catch {
    invalidBrokerResponse()
  }
  if (!encoded.endsWith('\n') || encoded.slice(0, -1).includes('\n') || encoded.includes('\r')) {
    invalidBrokerResponse()
  }
  let value: unknown
  try {
    value = JSON.parse(encoded)
  } catch {
    invalidBrokerResponse()
  }
  if (encoded !== `${JSON.stringify(value)}\n`) invalidBrokerResponse()
  return Object.freeze({ metadata: value, payload: bytes.subarray(payloadOffset) })
}

export function copyBrokerPipeResponse(input: {
  readonly request: Uint8Array
  readonly output: unknown
  readonly signal: AbortSignal
}): Buffer {
  const { output } = input
  if (!(output instanceof Uint8Array) || output.byteLength > MAXIMUM_BROKER_FRAME_BYTES) {
    if (output instanceof Uint8Array) output.fill(0)
    throw new Error('credential broker response exceeded its pipe authority')
  }
  if (arraysOverlap(input.request, output)) {
    output.fill(0)
    throw new Error('credential broker pipe crossed request and response ownership')
  }
  let response: Buffer
  try {
    response = Buffer.from(output)
  } finally {
    output.fill(0)
  }
  if (input.signal.aborted) {
    response.fill(0)
    throw new Error('credential broker operation was terminated')
  }
  return response
}

export function arraysOverlap(left: Uint8Array, right: Uint8Array): boolean {
  if (left.buffer !== right.buffer) return false
  const leftEnd = left.byteOffset + left.byteLength
  const rightEnd = right.byteOffset + right.byteLength
  return left.byteOffset < rightEnd && right.byteOffset < leftEnd
}

export function invalidBrokerResponse(): never {
  throw new Error('credential broker response is not canonical or authenticated')
}
