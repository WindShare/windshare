import { concatBytes, copyBytes, equalBytes } from '../crypto/bytes'
import {
  createX25519KeyAgreement,
  type X25519KeyAgreement,
  verifyEd25519Signature,
} from '../crypto/curve25519'
import { sha256 } from '../crypto/digest'
import { deriveSuite02SessionAuthKey } from '../crypto/suite02-key-derivation'
import { type CryptoRuntime, defaultCryptoRuntime } from '../crypto/webcrypto'
import type { V2ShareDescriptor } from '../catalog/v2-records'

export const V2_CLIENT_HELLO_BYTES = 133
export const V2_SERVER_HELLO_BYTES = 173
export const V2_HANDSHAKE_NONCE_BYTES = 32
export const V2_TRAFFIC_KEY_BYTES = 32

const TEXT_ENCODER = new TextEncoder()
const CLIENT_HELLO_MAGIC = TEXT_ENCODER.encode('WS2C')
const SERVER_HELLO_MAGIC = TEXT_ENCODER.encode('WS2S')
const CLIENT_HELLO_DOMAIN = TEXT_ENCODER.encode('windshare/v2 client-hello\0')
const SERVER_HELLO_DOMAIN = TEXT_ENCODER.encode('windshare/v2 server-hello\0')
const PROTOCOL_SESSION_DOMAIN = TEXT_ENCODER.encode('windshare/v2 protocol-session\0')
const HANDSHAKE_LABEL = 'windshare/v2 handshake'
const RECEIVER_TRAFFIC_LABEL = 'windshare/v2 traffic/receiver-to-sender'
const SENDER_TRAFFIC_LABEL = 'windshare/v2 traffic/sender-to-receiver'
const EMPTY_SALT = new Uint8Array(0)

export interface V2ReceiverHandshake {
  readonly clientHello: Uint8Array<ArrayBuffer>
  readonly receiverInstanceId: Uint8Array<ArrayBuffer>
  acceptServerHello(encoded: Uint8Array): Promise<V2SessionKeys>
  discard(): void
}

export interface V2SessionKeys {
  readonly protocolSessionId: Uint8Array<ArrayBuffer>
  readonly transcriptHash: Uint8Array<ArrayBuffer>
  readonly receiverToSenderKey: Uint8Array<ArrayBuffer>
  readonly senderToReceiverKey: Uint8Array<ArrayBuffer>
  readonly initialLaneId: number
  readonly initialLaneEpoch: 0
}

export interface V2ReceiverHandshakeOptions {
  readonly descriptor: V2ShareDescriptor
  readonly readSecret: Uint8Array
  readonly randomBytes?: (length: number) => Uint8Array
  readonly runtime?: CryptoRuntime
}

export class V2TranscriptError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'V2TranscriptError'
  }
}

export async function createV2ReceiverHandshake(
  options: V2ReceiverHandshakeOptions,
): Promise<V2ReceiverHandshake> {
  const runtime = options.runtime ?? defaultCryptoRuntime()
  const randomBytes = options.randomBytes ?? secureRandomBytes
  const receiverInstanceId = nonzeroRandom(randomBytes, 16, 'receiver instance ID')
  const receiverNonce = nonzeroRandom(randomBytes, V2_HANDSHAKE_NONCE_BYTES, 'receiver nonce')
  const sessionAuthKey = await deriveSuite02SessionAuthKey(
    options.readSecret,
    options.descriptor.shareInstance,
    runtime,
  )
  let keyAgreement: X25519KeyAgreement
  try {
    keyAgreement = await createX25519KeyAgreement({
      runtime,
      ...(options.randomBytes === undefined ? {} : { randomBytes: options.randomBytes }),
    })
  } catch (cause) {
    sessionAuthKey.fill(0)
    throw new V2TranscriptError('X25519 key agreement is unavailable for suite-02', { cause })
  }
  const receiverPublic = keyAgreement.publicKey
  let clientHello: Uint8Array<ArrayBuffer>
  try {
    const body = concatBytes([
      CLIENT_HELLO_MAGIC,
      Uint8Array.of(2),
      options.descriptor.shareInstance,
      receiverInstanceId,
      receiverNonce,
      receiverPublic,
    ])
    const proof = await hmacSha256(
      sessionAuthKey,
      concatBytes([CLIENT_HELLO_DOMAIN, await sha256(body, runtime)]),
      runtime,
    )
    clientHello = concatBytes([body, proof])
    if (clientHello.byteLength !== V2_CLIENT_HELLO_BYTES) {
      throw new V2TranscriptError('ClientHello construction violated the frozen width')
    }
  } catch (error) {
    keyAgreement.destroy()
    sessionAuthKey.fill(0)
    throw error
  }
  let consumed = false
  return Object.freeze({
    clientHello,
    receiverInstanceId,
    discard: () => {
      consumed = true
      keyAgreement.destroy()
      sessionAuthKey.fill(0)
    },
    acceptServerHello: async (encoded: Uint8Array) => {
      if (consumed) throw new V2TranscriptError('ServerHello authority is already consumed')
      consumed = true
      try {
        return await acceptServerHello(
          encoded,
          clientHello,
          keyAgreement,
          sessionAuthKey,
          options.descriptor.senderPublicKey,
          runtime,
        )
      } finally {
        keyAgreement.destroy()
        sessionAuthKey.fill(0)
      }
    },
  })
}

async function acceptServerHello(
  encoded: Uint8Array,
  clientHello: Uint8Array,
  keyAgreement: X25519KeyAgreement,
  sessionAuthKey: Uint8Array,
  senderSigningKey: Uint8Array,
  runtime: CryptoRuntime,
): Promise<V2SessionKeys> {
  if (
    encoded.byteLength !== V2_SERVER_HELLO_BYTES ||
    !equalBytes(encoded.subarray(0, 4), SERVER_HELLO_MAGIC) ||
    encoded[4] !== 2
  ) {
    throw new V2TranscriptError('ServerHello has an invalid header or length')
  }
  const body = encoded.subarray(0, V2_SERVER_HELLO_BYTES - 64)
  const clientDigest = await sha256(clientHello, runtime)
  if (!equalBytes(body.subarray(5, 37), clientDigest)) {
    throw new V2TranscriptError('ServerHello is bound to another ClientHello')
  }
  const signaturePreimage = concatBytes([SERVER_HELLO_DOMAIN, await sha256(body, runtime)])
  let signatureValid: boolean
  try {
    signatureValid = await verifyEd25519Signature(
      senderSigningKey,
      signaturePreimage,
      encoded.subarray(body.length),
      runtime,
    )
  } catch (cause) {
    throw new V2TranscriptError('Unable to verify ServerHello signature', { cause })
  }
  if (!signatureValid) throw new V2TranscriptError('ServerHello sender signature is invalid')
  const senderPublicBytes = body.slice(69, 101)
  const view = new DataView(body.buffer, body.byteOffset, body.byteLength)
  const initialLaneId = view.getUint32(101, false)
  const initialLaneEpoch = view.getUint32(105, false)
  if (initialLaneId === 0 || initialLaneEpoch !== 0) {
    throw new V2TranscriptError('ServerHello initial lane identity is invalid')
  }
  let shared: Uint8Array<ArrayBuffer>
  try {
    shared = await keyAgreement.deriveSharedSecret(senderPublicBytes)
  } catch (cause) {
    throw new V2TranscriptError('X25519 session agreement failed', { cause })
  }
  let handshakeSecret: Uint8Array<ArrayBuffer> | undefined
  let receiverToSenderKey: Uint8Array<ArrayBuffer> | undefined
  try {
    const transcriptHash = await sha256(concatBytes([clientHello, encoded]), runtime)
    const protocolSessionId = (await sha256(
      concatBytes([PROTOCOL_SESSION_DOMAIN, transcriptHash]),
      runtime,
    )).slice(0, 16)
    handshakeSecret = await hkdf(
      shared,
      sessionAuthKey,
      domainInfo(HANDSHAKE_LABEL, transcriptHash),
      runtime,
    )
    receiverToSenderKey = await hkdf(
      handshakeSecret,
      EMPTY_SALT,
      domainInfo(RECEIVER_TRAFFIC_LABEL, transcriptHash),
      runtime,
    )
    const senderToReceiverKey = await hkdf(
      handshakeSecret,
      EMPTY_SALT,
      domainInfo(SENDER_TRAFFIC_LABEL, transcriptHash),
      runtime,
    )
    return Object.freeze({
      protocolSessionId,
      transcriptHash,
      receiverToSenderKey,
      senderToReceiverKey,
      initialLaneId,
      initialLaneEpoch: 0,
    })
  } catch (error) {
    receiverToSenderKey?.fill(0)
    throw error
  } finally {
    shared.fill(0)
    handshakeSecret?.fill(0)
  }
}

async function hkdf(
  secret: Uint8Array,
  salt: Uint8Array,
  info: Uint8Array,
  runtime: CryptoRuntime,
): Promise<Uint8Array<ArrayBuffer>> {
  const material = await runtime.subtle.importKey('raw', copyBytes(secret), 'HKDF', false, ['deriveBits'])
  const result = await runtime.subtle.deriveBits(
    { name: 'HKDF', hash: 'SHA-256', salt: copyBytes(salt), info: copyBytes(info) },
    material,
    V2_TRAFFIC_KEY_BYTES * 8,
  )
  return new Uint8Array(result)
}

async function hmacSha256(
  keyBytes: Uint8Array,
  input: Uint8Array,
  runtime: CryptoRuntime,
): Promise<Uint8Array<ArrayBuffer>> {
  const key = await runtime.subtle.importKey(
    'raw',
    copyBytes(keyBytes),
    { name: 'HMAC', hash: 'SHA-256' },
    false,
    ['sign'],
  )
  return new Uint8Array(await runtime.subtle.sign('HMAC', key, copyBytes(input)))
}


function domainInfo(label: string, context: Uint8Array): Uint8Array<ArrayBuffer> {
  return concatBytes([TEXT_ENCODER.encode(label), Uint8Array.of(0), context])
}

function secureRandomBytes(length: number): Uint8Array<ArrayBuffer> {
  const output = new Uint8Array(length)
  globalThis.crypto.getRandomValues(output)
  return output
}

function nonzeroRandom(
  source: (length: number) => Uint8Array,
  length: number,
  label: string,
): Uint8Array<ArrayBuffer> {
  for (let attempt = 0; attempt < 4; attempt += 1) {
    const value = source(length)
    if (value.byteLength === length && value.some((item) => item !== 0)) return value.slice()
  }
  throw new V2TranscriptError(`${label} source did not produce a valid identity`)
}
