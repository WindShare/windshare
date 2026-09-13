import { equalBytes } from '../../crypto/bytes'
import {
  decodeV2SessionCredit,
  V2_RELAY_SENDER_WINDOW_BYTES,
  V2_RELAY_SENDER_WINDOW_FRAMES,
  V2RelayProtocolError,
} from './v2-protocol'

/** One serialized socket writer consumes a relay-owned, initially empty window. */
export class ReceiverCredit {
  readonly #relaySessionId: Uint8Array
  readonly #waiters = new Set<() => void>()
  #frames = 0
  #bytes = 0

  constructor(relaySessionId: Uint8Array) {
    this.#relaySessionId = relaySessionId.slice()
  }

  receive(encoded: Uint8Array): boolean {
    if (encoded.byteLength < 4 || String.fromCharCode(...encoded.subarray(0, 4)) !== 'WS2W') return false
    const credit = decodeV2SessionCredit(encoded)
    if (!equalBytes(credit.relaySessionId, this.#relaySessionId)) {
      throw new V2RelayProtocolError('malformed', 'Relay credited another receiver session')
    }
    if (
      this.#frames + credit.frames > V2_RELAY_SENDER_WINDOW_FRAMES ||
      this.#bytes + credit.bytes > V2_RELAY_SENDER_WINDOW_BYTES
    ) {
      throw new V2RelayProtocolError('malformed', 'Relay credit exceeds the receiver window')
    }
    this.#frames += credit.frames
    this.#bytes += credit.bytes
    for (const wake of this.#waiters) wake()
    return true
  }

  async waitForCapacity(encodedBytes: number, signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    while (!this.#hasCapacity(encodedBytes)) {
      await new Promise<void>((resolve, reject) => {
        const cleanup = () => {
          this.#waiters.delete(changed)
          signal.removeEventListener('abort', aborted)
        }
        const changed = () => { cleanup(); resolve() }
        const aborted = () => { cleanup(); reject(signal.reason) }
        this.#waiters.add(changed)
        signal.addEventListener('abort', aborted, { once: true })
      })
      signal.throwIfAborted()
    }
  }

  // The writer waits without debiting, then consumes synchronously with send().
  // Cancellation while awaiting local buffer space therefore cannot lose credit.
  consume(encodedBytes: number): void {
    if (!this.#hasCapacity(encodedBytes)) {
      throw new V2RelayProtocolError('malformed', 'Relay frame exceeds available receiver credit')
    }
    this.#frames -= 1
    this.#bytes -= encodedBytes
  }

  #hasCapacity(encodedBytes: number): boolean {
    return this.#frames > 0 && this.#bytes >= encodedBytes
  }
}
