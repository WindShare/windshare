export const PEER_CONTINUATION_SLOTS = 4
const MAXIMUM_SEQUENCE = 0xffffffffffffffffn

export interface PeerAttemptOrdinal {
  readonly peerSlot: number
  readonly attemptSequence: bigint
}

export interface PeerSlotLease {
  next(): PeerAttemptOrdinal
  close(): void
}

export function requirePeerSlot(value: bigint): number {
  if (value < 0n || value >= BigInt(PEER_CONTINUATION_SLOTS)) {
    throw new RangeError('Peer continuation slot is outside the session capacity')
  }
  return Number(value)
}

/** Watermarks outlive physical paths; a closed lease never borrows its successor. */
export class PeerSlotPool {
  readonly #slots = Array.from({ length: PEER_CONTINUATION_SLOTS },
    () => ({ sequence: 0n, occupied: false }))

  acquire(): PeerSlotLease {
    const peerSlot = this.#slots.findIndex((slot) => !slot.occupied && slot.sequence < MAXIMUM_SEQUENCE)
    if (peerSlot < 0) throw new RangeError('Peer negotiation slots are occupied')
    const slot = this.#slots[peerSlot]!
    slot.occupied = true
    let closed = false
    return Object.freeze({
      next: () => {
        if (closed) throw new Error('Peer negotiation slot is closed')
        if (slot.sequence === MAXIMUM_SEQUENCE) throw new RangeError('Peer negotiation sequence is exhausted')
        slot.sequence += 1n
        return Object.freeze({ peerSlot, attemptSequence: slot.sequence })
      },
      close: () => {
        if (closed) return
        closed = true
        slot.occupied = false
      },
    })
  }
}
