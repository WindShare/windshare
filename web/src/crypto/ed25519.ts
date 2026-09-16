import { copyBytes } from './bytes'
import { type CryptoRuntime, defaultCryptoRuntime } from './webcrypto'
import { nativeEd25519Failure, qualifyNativeEd25519, withEd25519Deadline } from './ed25519-native'

export const ED25519_PUBLIC_KEY_BYTES = 32
export const ED25519_SIGNATURE_BYTES = 64

export type Ed25519Backend = 'native' | 'noble'
export type Ed25519Decision =
  | 'qualified' | 'unsupported' | 'qualification-failed' | 'timeout' | 'operation-failed'
  | 'invalid-key' | 'invalid-signature' | 'backend-error'

export interface Ed25519Observation {
  readonly verifierId: number
  readonly backend: Ed25519Backend
  readonly reason: Ed25519Decision
  readonly transition: 'selected' | 'fallback' | 'rejected' | 'failed'
}

export interface Ed25519VerifierOptions {
  readonly runtime?: CryptoRuntime
  readonly observe?: (event: Ed25519Observation) => void
}

/** A sender identity owns its key snapshot and backend resources across all message types. */
export interface Ed25519Verifier {
  readonly publicKey: Uint8Array<ArrayBuffer>
  verify(message: Uint8Array, signature: Uint8Array): Promise<boolean>
}

type Curves = typeof import('@noble/curves/ed25519.js')
type Point = InstanceType<Curves['ed25519']['Point']>
interface Backend {
  readonly curves: Curves
  nativeKey: CryptoKey | undefined
}

let nextVerifierId = 0

export function createEd25519Verifier(
  publicKey: Uint8Array,
  options: Ed25519VerifierOptions = {},
): Ed25519Verifier {
  if (publicKey.byteLength !== ED25519_PUBLIC_KEY_BYTES) {
    throw new TypeError('Ed25519 public key must be 32 bytes')
  }
  return new SenderVerifier(copyBytes(publicKey), options)
}

class SenderVerifier implements Ed25519Verifier {
  readonly #publicKey: Uint8Array<ArrayBuffer>
  readonly #runtime: CryptoRuntime
  readonly #observe: Ed25519VerifierOptions['observe']
  readonly #id = ++nextVerifierId
  #backend: Promise<Backend> | undefined

  constructor(publicKey: Uint8Array<ArrayBuffer>, options: Ed25519VerifierOptions) {
    this.#publicKey = publicKey
    this.#runtime = options.runtime ?? defaultCryptoRuntime()
    this.#observe = options.observe
  }

  get publicKey(): Uint8Array<ArrayBuffer> { return this.#publicKey.slice() }

  async verify(message: Uint8Array, signature: Uint8Array): Promise<boolean> {
    // Snapshot before the first await, including lazy backend initialization.
    const ownedMessage = copyBytes(message)
    const ownedSignature = copyBytes(signature)
    this.#backend ??= this.#initialize()
    const backend = await this.#backend
    const point = signaturePoint(backend.curves, ownedSignature)
    if (point === undefined) {
      this.#emit(backend.nativeKey === undefined ? 'noble' : 'native', 'rejected', 'invalid-signature')
      return false
    }
    const nativeResult = await this.#verifyNative(backend, ownedMessage, ownedSignature)
    if (nativeResult !== undefined) return nativeResult
    // A is in the prime-order subgroup. Requiring R in that subgroup makes
    // Noble's cofactored equation equivalent to Go/WebCrypto's cofactorless one.
    const valid = point.isTorsionFree() &&
      backend.curves.ed25519.verify(ownedSignature, ownedMessage, this.#publicKey, { zip215: false })
    if (!valid) this.#emit('noble', 'rejected', 'invalid-signature')
    return valid
  }

  async #verifyNative(
    backend: Backend, message: Uint8Array<ArrayBuffer>, signature: Uint8Array<ArrayBuffer>,
  ): Promise<boolean | undefined> {
    const nativeKey = backend.nativeKey
    if (nativeKey !== undefined) {
      try {
        const valid = await withEd25519Deadline(this.#runtime.subtle.verify(
          'Ed25519', nativeKey, signature, message,
        ))
        if (!valid) this.#emit('native', 'rejected', 'invalid-signature')
        // Authentication failure is final. Trying another acceptance policy here
        // would turn the protocol into the union of two verifiers.
        return valid
      } catch (cause) {
        const reason = nativeEd25519Failure(cause)
        if (reason === undefined) {
          this.#emit('native', 'failed', 'backend-error')
          throw cause
        }
        if (backend.nativeKey !== undefined) {
          backend.nativeKey = undefined
          this.#emit('noble', 'fallback', reason)
        }
      }
    }
    return undefined
  }

  async #initialize(): Promise<Backend> {
    const curves = await import('@noble/curves/ed25519.js')
    try {
      const point = curves.ed25519.Point.fromBytes(this.#publicKey, false)
      if (point.isSmallOrder() || !point.isTorsionFree()) throw new Error('invalid subgroup')
    } catch (cause) {
      this.#emit('noble', 'rejected', 'invalid-key')
      throw new TypeError('Ed25519 sender key must be canonical, nonzero and prime-order', { cause })
    }
    const qualification = await qualifyNativeEd25519(this.#runtime.subtle)
    if (qualification !== 'qualified') {
      this.#emit('noble', 'selected', qualification)
      return { curves, nativeKey: undefined }
    }
    try {
      const nativeKey = await withEd25519Deadline(this.#runtime.subtle.importKey(
        'raw', this.#publicKey, 'Ed25519', false, ['verify'],
      ))
      this.#emit('native', 'selected', 'qualified')
      return { curves, nativeKey }
    } catch (cause) {
      const reason = nativeEd25519Failure(cause)
      if (reason === undefined) {
        this.#emit('native', 'failed', 'backend-error')
        throw cause
      }
      this.#emit('noble', 'selected', reason)
      return { curves, nativeKey: undefined }
    }
  }

  #emit(backend: Ed25519Backend, transition: Ed25519Observation['transition'], reason: Ed25519Decision): void {
    this.#observe?.({ verifierId: this.#id, backend, transition, reason })
  }
}

function signaturePoint(curves: Curves, signature: Uint8Array): Point | undefined {
  if (signature.byteLength !== ED25519_SIGNATURE_BYTES) return undefined
  try {
    // The library's scalar decoder rejects S >= L without reducing the input.
    curves.ed25519.Point.Fn.fromBytes(signature.subarray(ED25519_PUBLIC_KEY_BYTES))
    const point = curves.ed25519.Point.fromBytes(signature.subarray(0, ED25519_PUBLIC_KEY_BYTES), false)
    return point.isSmallOrder() ? undefined : point
  } catch {
    return undefined
  }
}
