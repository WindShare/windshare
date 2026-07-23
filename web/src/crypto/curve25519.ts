import { copyBytes } from './bytes'
import { type CryptoRuntime, defaultCryptoRuntime } from './webcrypto'

export const CURVE25519_KEY_BYTES = 32
export const ED25519_SIGNATURE_BYTES = 64

export interface X25519KeyAgreement {
  readonly publicKey: Uint8Array<ArrayBuffer>
  deriveSharedSecret(peerPublicKey: Uint8Array): Promise<Uint8Array<ArrayBuffer>>
  destroy(): void
}

export interface X25519KeyAgreementOptions {
  readonly runtime?: CryptoRuntime
  readonly randomBytes?: (length: number) => Uint8Array
}

type NobleCurve25519 = typeof import('@noble/curves/ed25519.js')

let nobleCurve25519: Promise<NobleCurve25519> | undefined

export async function createX25519KeyAgreement(
  options: X25519KeyAgreementOptions = {},
): Promise<X25519KeyAgreement> {
  const runtime = options.runtime ?? defaultCryptoRuntime()
  if (options.randomBytes === undefined) {
    try {
      return await createNativeX25519KeyAgreement(runtime)
    } catch (cause) {
      if (!isUnsupportedAlgorithm(cause)) throw cause
    }
  }

  // WebCrypto deliberately has no caller-supplied entropy hook. Selecting the
  // portable backend for injected randomness keeps deterministic tests honest
  // while production still prefers the browser's native implementation.
  return createPortableX25519KeyAgreement(options.randomBytes ?? secureRandomBytes)
}

export async function verifyEd25519Signature(
  publicKey: Uint8Array,
  message: Uint8Array,
  signature: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<boolean> {
  requireWidth(publicKey, CURVE25519_KEY_BYTES, 'Ed25519 public key')
  requireWidth(signature, ED25519_SIGNATURE_BYTES, 'Ed25519 signature')
  try {
    const key = await runtime.subtle.importKey(
      'raw',
      copyBytes(publicKey),
      'Ed25519',
      false,
      ['verify'],
    )
    return await runtime.subtle.verify(
      'Ed25519',
      key,
      copyBytes(signature),
      copyBytes(message),
    )
  } catch (cause) {
    if (!isUnsupportedAlgorithm(cause)) throw cause
  }

  const { ed25519 } = await loadNobleCurve25519()
  // Go's crypto/ed25519 follows RFC 8032, so accepting ZIP-215's additional
  // encodings here would make browser and sender authentication disagree.
  return ed25519.verify(
    copyBytes(signature),
    copyBytes(message),
    copyBytes(publicKey),
    { zip215: false },
  )
}

async function createNativeX25519KeyAgreement(
  runtime: CryptoRuntime,
): Promise<X25519KeyAgreement> {
  const keyPair = await runtime.subtle.generateKey(
    'X25519',
    false,
    ['deriveBits'],
  ) as CryptoKeyPair
  const publicKey = new Uint8Array(await runtime.subtle.exportKey('raw', keyPair.publicKey))
  requireWidth(publicKey, CURVE25519_KEY_BYTES, 'X25519 public key')
  return new NativeX25519KeyAgreement(runtime, keyPair.privateKey, publicKey)
}

async function createPortableX25519KeyAgreement(
  randomBytes: (length: number) => Uint8Array,
): Promise<X25519KeyAgreement> {
  const { x25519 } = await loadNobleCurve25519()
  const seed = requirePrivateSeed(randomBytes(CURVE25519_KEY_BYTES))
  try {
    const generated = x25519.keygen(seed)
    const secretKey = copyBytes(generated.secretKey)
    const publicKey = copyBytes(generated.publicKey)
    generated.secretKey.fill(0)
    requireWidth(publicKey, CURVE25519_KEY_BYTES, 'X25519 public key')
    return new PortableX25519KeyAgreement(x25519, secretKey, publicKey)
  } finally {
    seed.fill(0)
  }
}

class NativeX25519KeyAgreement implements X25519KeyAgreement {
  readonly #runtime: CryptoRuntime
  readonly #publicKey: Uint8Array<ArrayBuffer>
  #privateKey: CryptoKey | undefined

  constructor(runtime: CryptoRuntime, privateKey: CryptoKey, publicKey: Uint8Array) {
    this.#runtime = runtime
    this.#privateKey = privateKey
    this.#publicKey = copyBytes(publicKey)
  }

  get publicKey(): Uint8Array<ArrayBuffer> {
    return this.#publicKey.slice()
  }

  async deriveSharedSecret(peerPublicKey: Uint8Array): Promise<Uint8Array<ArrayBuffer>> {
    requireWidth(peerPublicKey, CURVE25519_KEY_BYTES, 'X25519 peer public key')
    const privateKey = this.#takePrivateKey()
    try {
      const peer = await this.#runtime.subtle.importKey(
        'raw',
        copyBytes(peerPublicKey),
        'X25519',
        false,
        [],
      )
      const shared = new Uint8Array(await this.#runtime.subtle.deriveBits(
        { name: 'X25519', public: peer },
        privateKey,
        CURVE25519_KEY_BYTES * 8,
      ))
      requireSharedSecret(shared)
      return shared
    } finally {
      this.#privateKey = undefined
    }
  }

  destroy(): void {
    this.#privateKey = undefined
  }

  #takePrivateKey(): CryptoKey {
    const privateKey = this.#privateKey
    if (privateKey === undefined) throw new Error('X25519 key agreement is already consumed')
    this.#privateKey = undefined
    return privateKey
  }
}

interface PortableX25519 {
  getSharedSecret(secretKey: Uint8Array, publicKey: Uint8Array): Uint8Array
}

class PortableX25519KeyAgreement implements X25519KeyAgreement {
  readonly #x25519: PortableX25519
  readonly #publicKey: Uint8Array<ArrayBuffer>
  #secretKey: Uint8Array<ArrayBuffer> | undefined

  constructor(x25519: PortableX25519, secretKey: Uint8Array, publicKey: Uint8Array) {
    this.#x25519 = x25519
    this.#secretKey = copyBytes(secretKey)
    secretKey.fill(0)
    this.#publicKey = copyBytes(publicKey)
  }

  get publicKey(): Uint8Array<ArrayBuffer> {
    return this.#publicKey.slice()
  }

  async deriveSharedSecret(peerPublicKey: Uint8Array): Promise<Uint8Array<ArrayBuffer>> {
    requireWidth(peerPublicKey, CURVE25519_KEY_BYTES, 'X25519 peer public key')
    const secretKey = this.#takeSecretKey()
    try {
      const derived = this.#x25519.getSharedSecret(secretKey, copyBytes(peerPublicKey))
      try {
        const shared = copyBytes(derived)
        requireSharedSecret(shared)
        return shared
      } finally {
        derived.fill(0)
      }
    } finally {
      secretKey.fill(0)
    }
  }

  destroy(): void {
    this.#secretKey?.fill(0)
    this.#secretKey = undefined
  }

  #takeSecretKey(): Uint8Array<ArrayBuffer> {
    const secretKey = this.#secretKey
    if (secretKey === undefined) throw new Error('X25519 key agreement is already consumed')
    this.#secretKey = undefined
    return secretKey
  }
}

function requirePrivateSeed(value: Uint8Array): Uint8Array<ArrayBuffer> {
  requireWidth(value, CURVE25519_KEY_BYTES, 'X25519 private seed')
  if (!value.some((byte) => byte !== 0)) {
    throw new TypeError('X25519 private seed must not be all zero')
  }
  return copyBytes(value)
}

function requireSharedSecret(value: Uint8Array): void {
  requireWidth(value, CURVE25519_KEY_BYTES, 'X25519 shared secret')
  if (!value.some((byte) => byte !== 0)) {
    value.fill(0)
    throw new TypeError('X25519 peer public key produced an all-zero shared secret')
  }
}

function requireWidth(value: Uint8Array, width: number, label: string): void {
  if (value.byteLength !== width) {
    throw new TypeError(`${label} must be ${width} bytes`)
  }
}

function isUnsupportedAlgorithm(cause: unknown): boolean {
  return typeof cause === 'object' && cause !== null &&
    'name' in cause && cause.name === 'NotSupportedError'
}

function secureRandomBytes(length: number): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(length)
  globalThis.crypto.getRandomValues(bytes)
  return bytes
}

function loadNobleCurve25519(): Promise<NobleCurve25519> {
  // Lazy loading keeps the audited fallback outside native-engine startup and
  // pays its bundle/runtime cost only where WebCrypto lacks the frozen curves.
  nobleCurve25519 ??= import('@noble/curves/ed25519.js')
  return nobleCurve25519
}
