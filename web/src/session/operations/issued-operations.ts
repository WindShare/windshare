import { equalBytes } from '../../crypto/bytes'
import { MESSAGE_KIND, type MessageKind } from '../message'
import { SessionRuntimeError } from '../runtime-types'

const PREFIX_BYTES = 8
const ID_BYTES = 16
const KIND_SHIFT = 56n
const MAXIMUM_SEQUENCE = (1n << KIND_SHIFT) - 1n
const PREFIX_ATTEMPTS = 4
const REQUEST_KINDS = new Set<MessageKind>([
  MESSAGE_KIND.listChildren, MESSAGE_KIND.openRevisions,
  MESSAGE_KIND.renewLease, MESSAGE_KIND.releaseLease,
  MESSAGE_KIND.requestBlocks, MESSAGE_KIND.laneAttach, MESSAGE_KIND.peerOffer,
])

/**
 * Operation IDs remain opaque on the wire. Per-kind contiguous issuance lets the
 * receiver recognize its own abandoned work after detailed replay state expires,
 * without retaining one record per request for the entire session.
 */
export class IssuedOperations {
  readonly #randomBytes: (length: number) => Uint8Array
  readonly #issued = new Map<MessageKind, bigint>()
  #prefix: Uint8Array<ArrayBuffer> | undefined

  constructor(randomBytes: (length: number) => Uint8Array = length => crypto.getRandomValues(new Uint8Array(length))) {
    this.#randomBytes = randomBytes
  }

  issue(kind: MessageKind): Uint8Array<ArrayBuffer> {
    if (!REQUEST_KINDS.has(kind)) throw new SessionRuntimeError('operation', 'Message kind cannot begin a receiver operation')
    const sequence = (this.#issued.get(kind) ?? 0n) + 1n
    if (sequence > MAXIMUM_SEQUENCE) throw new SessionRuntimeError('session', 'Operation identity space is exhausted')
    this.#prefix ??= this.#newPrefix()
    const id = new Uint8Array(ID_BYTES)
    id.set(this.#prefix)
    new DataView(id.buffer).setBigUint64(PREFIX_BYTES, (BigInt(kind) << KIND_SHIFT) | sequence)
    this.#issued.set(kind, sequence)
    return id
  }

  requestKind(id: Uint8Array): MessageKind | undefined {
    if (id.byteLength !== ID_BYTES || this.#prefix === undefined ||
      !equalBytes(id.subarray(0, PREFIX_BYTES), this.#prefix)) return undefined
    const encoded = new DataView(id.buffer, id.byteOffset, id.byteLength).getBigUint64(PREFIX_BYTES)
    const kind = Number(encoded >> KIND_SHIFT) as MessageKind
    const sequence = encoded & MAXIMUM_SEQUENCE
    const issued = this.#issued.get(kind)
    return sequence > 0n && issued !== undefined && sequence <= issued ? kind : undefined
  }

  #newPrefix(): Uint8Array<ArrayBuffer> {
    for (let attempt = 0; attempt < PREFIX_ATTEMPTS; attempt += 1) {
      const prefix = this.#randomBytes(PREFIX_BYTES)
      if (prefix.byteLength === PREFIX_BYTES && prefix.some(value => value !== 0)) return new Uint8Array(prefix)
    }
    throw new SessionRuntimeError('session', 'Random identity source returned invalid bytes')
  }
}
