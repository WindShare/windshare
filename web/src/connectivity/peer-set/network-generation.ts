import { encodeBase64Url } from '../../crypto/bytes'

/** One receiver network identity survives ProtocolSession replacement. */
export class PeerNetworkGeneration {
  #identity = newIdentity()
  #lastChangeAt = Number.NEGATIVE_INFINITY

  get id(): string { return encodeBase64Url(this.#identity) }
  copyBytes(): Uint8Array<ArrayBuffer> { return this.#identity.slice() }

  changed(now: number): void {
    if (now === this.#lastChangeAt) return
    this.#lastChangeAt = now
    this.#identity = newIdentity()
  }
}
function newIdentity(): Uint8Array<ArrayBuffer> {
  const identity = new Uint8Array(16)
  globalThis.crypto.getRandomValues(identity)
  if (!identity.some((value) => value !== 0)) identity[0] = 1
  return identity
}
