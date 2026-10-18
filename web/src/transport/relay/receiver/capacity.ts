import type { RelayReceiveCapacity } from '../delivery-handshake-codec'
import { ReceiveLedgerError } from '../receive-ledger'

export const DEFAULT_RELAY_RECEIVE_CAPACITY: RelayReceiveCapacity = Object.freeze({
  frames: 256n,
  encodedBytes: 16n * 1024n * 1024n,
})

/** The declaration and storage owner must share a snapshot before dialing yields. */
export function receiverCapacity(capacity = DEFAULT_RELAY_RECEIVE_CAPACITY): RelayReceiveCapacity {
  if (![capacity.frames, capacity.encodedBytes].every(value =>
    typeof value === 'bigint' && value > 0n && value <= BigInt(Number.MAX_SAFE_INTEGER))) {
    throw new ReceiveLedgerError('config')
  }
  return Object.freeze({ ...capacity })
}
