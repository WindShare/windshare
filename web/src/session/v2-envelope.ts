import { concatBytes, copyBytes } from '../crypto/bytes'
import { type CryptoRuntime, defaultCryptoRuntime } from '../crypto/webcrypto'

export const V2_ENVELOPE_HEADER_BYTES = 28
export const V2_ENVELOPE_TAG_BYTES = 16
export const V2_ENVELOPE_MAXIMUM_BYTES = 65_536
export const V2_ENVELOPE_MAXIMUM_PLAINTEXT_BYTES =
  V2_ENVELOPE_MAXIMUM_BYTES - V2_ENVELOPE_HEADER_BYTES - V2_ENVELOPE_TAG_BYTES

const TEXT_ENCODER = new TextEncoder()
const ENVELOPE_DOMAIN = TEXT_ENCODER.encode('windshare/v2 operation-envelope\0')
const MAXIMUM_SEQUENCE = 0xffff_ffff_ffff_ffffn

export type V2EnvelopeDirection = 0 | 1

export interface V2EnvelopeBinding {
  readonly shareInstance: Uint8Array
  readonly protocolSessionId: Uint8Array
  readonly laneId: number
  readonly laneEpoch: number
  readonly direction: V2EnvelopeDirection
}

export interface V2OpenedEnvelope {
  readonly sequence: bigint
  readonly plaintext: Uint8Array<ArrayBuffer>
}

export class V2EnvelopeError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'V2EnvelopeError'
  }
}

export class V2EnvelopeSealer {
  readonly #binding: V2EnvelopeBinding
  readonly #key: Promise<CryptoKey>
  readonly #randomBytes: (length: number) => Uint8Array
  readonly #runtime: CryptoRuntime
  #sequence = 0n
  #exhausted = false

  constructor(
    keyBytes: Uint8Array,
    binding: V2EnvelopeBinding,
    options: {
      readonly randomBytes?: (length: number) => Uint8Array
      readonly runtime?: CryptoRuntime
    } = {},
  ) {
    this.#binding = snapshotBinding(binding)
    this.#runtime = options.runtime ?? defaultCryptoRuntime()
    this.#randomBytes = options.randomBytes ?? secureRandomBytes
    this.#key = importAesKey(keyBytes, ['encrypt'], this.#runtime)
  }

  get nextSequence(): bigint {
    if (this.#exhausted) throw new V2EnvelopeError('Envelope sequence is exhausted')
    return this.#sequence
  }

  async seal(plaintext: Uint8Array): Promise<Uint8Array<ArrayBuffer>> {
    if (plaintext.byteLength > V2_ENVELOPE_MAXIMUM_PLAINTEXT_BYTES) {
      throw new V2EnvelopeError('Envelope plaintext exceeds the frame limit')
    }
    const sequence = this.nextSequence
    const nonce = this.#randomBytes(12)
    if (nonce.byteLength !== 12) throw new V2EnvelopeError('Envelope nonce has an invalid width')
    const ciphertextLength = plaintext.byteLength + V2_ENVELOPE_TAG_BYTES
    const header = new Uint8Array(V2_ENVELOPE_HEADER_BYTES)
    const view = new DataView(header.buffer)
    header[0] = 2
    header[1] = this.#binding.direction
    view.setBigUint64(4, sequence, false)
    view.setUint32(12, ciphertextLength, false)
    header.set(nonce, 16)
    let ciphertext: ArrayBuffer
    try {
      ciphertext = await this.#runtime.subtle.encrypt(
        {
          name: 'AES-GCM',
          iv: copyBytes(nonce),
          additionalData: envelopeAuthenticationData(this.#binding, sequence, ciphertextLength),
          tagLength: V2_ENVELOPE_TAG_BYTES * 8,
        },
        await this.#key,
        copyBytes(plaintext),
      )
    } catch (cause) {
      throw new V2EnvelopeError('Envelope sealing failed', { cause })
    }
    const advanced = advanceEnvelopeSequence(this.#sequence)
    this.#sequence = advanced.sequence
    this.#exhausted = advanced.exhausted
    return concatBytes([header, new Uint8Array(ciphertext)])
  }
}

export class V2EnvelopeOpener {
  readonly #binding: V2EnvelopeBinding
  readonly #key: Promise<CryptoKey>
  readonly #runtime: CryptoRuntime
  #sequence = 0n
  #exhausted = false

  constructor(
    keyBytes: Uint8Array,
    binding: V2EnvelopeBinding,
    runtime: CryptoRuntime = defaultCryptoRuntime(),
  ) {
    this.#binding = snapshotBinding(binding)
    this.#runtime = runtime
    this.#key = importAesKey(keyBytes, ['decrypt'], runtime)
  }

  async open(frame: Uint8Array): Promise<V2OpenedEnvelope> {
    if (this.#exhausted) throw new V2EnvelopeError('Envelope sequence is exhausted')
    const header = parseHeader(frame, this.#binding.direction)
    if (header.sequence !== this.#sequence) {
      throw new V2EnvelopeError('Envelope sequence is not the next expected value')
    }
    let plaintext: ArrayBuffer
    try {
      plaintext = await this.#runtime.subtle.decrypt(
        {
          name: 'AES-GCM',
          iv: copyBytes(header.nonce),
          additionalData: envelopeAuthenticationData(
            this.#binding,
            header.sequence,
            header.ciphertextLength,
          ),
          tagLength: V2_ENVELOPE_TAG_BYTES * 8,
        },
        await this.#key,
        frame.slice(V2_ENVELOPE_HEADER_BYTES),
      )
    } catch (cause) {
      // Authentication failures never consume the legitimate sender sequence.
      throw new V2EnvelopeError('Envelope authentication failed', { cause })
    }
    const advanced = advanceEnvelopeSequence(this.#sequence)
    this.#sequence = advanced.sequence
    this.#exhausted = advanced.exhausted
    return Object.freeze({ sequence: header.sequence, plaintext: new Uint8Array(plaintext) })
  }
}

function advanceEnvelopeSequence(sequence: bigint): {
  readonly sequence: bigint
  readonly exhausted: boolean
} {
  return sequence === MAXIMUM_SEQUENCE
    ? { sequence, exhausted: true }
    : { sequence: sequence + 1n, exhausted: false }
}

interface ParsedHeader {
  readonly sequence: bigint
  readonly ciphertextLength: number
  readonly nonce: Uint8Array<ArrayBuffer>
}

function parseHeader(frame: Uint8Array, direction: V2EnvelopeDirection): ParsedHeader {
  if (
    frame.byteLength < V2_ENVELOPE_HEADER_BYTES + V2_ENVELOPE_TAG_BYTES ||
    frame.byteLength > V2_ENVELOPE_MAXIMUM_BYTES ||
    frame[0] !== 2 ||
    frame[1] !== direction ||
    frame[2] !== 0 ||
    frame[3] !== 0
  ) {
    throw new V2EnvelopeError('Envelope header is malformed or has the wrong direction')
  }
  const view = new DataView(frame.buffer, frame.byteOffset, frame.byteLength)
  const ciphertextLength = view.getUint32(12, false)
  if (
    ciphertextLength < V2_ENVELOPE_TAG_BYTES ||
    V2_ENVELOPE_HEADER_BYTES + ciphertextLength !== frame.byteLength
  ) {
    throw new V2EnvelopeError('Envelope ciphertext length is inconsistent')
  }
  return Object.freeze({
    sequence: view.getBigUint64(4, false),
    ciphertextLength,
    nonce: frame.slice(16, V2_ENVELOPE_HEADER_BYTES),
  })
}

function snapshotBinding(binding: V2EnvelopeBinding): V2EnvelopeBinding {
  if (
    binding.shareInstance.byteLength !== 16 ||
    binding.protocolSessionId.byteLength !== 16 ||
    !binding.shareInstance.some((item) => item !== 0) ||
    !binding.protocolSessionId.some((item) => item !== 0) ||
    !Number.isInteger(binding.laneId) || binding.laneId <= 0 || binding.laneId > 0xffff_ffff ||
    !Number.isInteger(binding.laneEpoch) || binding.laneEpoch < 0 || binding.laneEpoch > 0xffff_ffff ||
    (binding.direction !== 0 && binding.direction !== 1)
  ) {
    throw new V2EnvelopeError('Envelope binding is invalid')
  }
  return Object.freeze({
    shareInstance: binding.shareInstance.slice(),
    protocolSessionId: binding.protocolSessionId.slice(),
    laneId: binding.laneId,
    laneEpoch: binding.laneEpoch,
    direction: binding.direction,
  })
}

function envelopeAuthenticationData(
  binding: V2EnvelopeBinding,
  sequence: bigint,
  ciphertextLength: number,
): Uint8Array<ArrayBuffer> {
  const geometry = new Uint8Array(16)
  const view = new DataView(geometry.buffer)
  view.setUint32(0, binding.laneId, false)
  view.setUint32(4, binding.laneEpoch, false)
  view.setBigUint64(8, sequence, false)
  const length = new Uint8Array(4)
  new DataView(length.buffer).setUint32(0, ciphertextLength, false)
  return concatBytes([
    ENVELOPE_DOMAIN,
    Uint8Array.of(2, binding.direction),
    binding.shareInstance,
    binding.protocolSessionId,
    geometry,
    length,
  ])
}

function importAesKey(
  keyBytes: Uint8Array,
  usages: KeyUsage[],
  runtime: CryptoRuntime,
): Promise<CryptoKey> {
  if (keyBytes.byteLength !== 32) throw new V2EnvelopeError('Traffic key must be 32 bytes')
  return runtime.subtle.importKey('raw', copyBytes(keyBytes), 'AES-GCM', false, usages)
}

function secureRandomBytes(length: number): Uint8Array<ArrayBuffer> {
  const output = new Uint8Array(length)
  globalThis.crypto.getRandomValues(output)
  return output
}
