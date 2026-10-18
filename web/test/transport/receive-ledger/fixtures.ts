import { RelayReceiveLedger, type ReceiveLedgerConfig } from '../../../src/transport/relay/receive-ledger'

export const ZERO = { frames: 0n, encodedBytes: 0n }
export const MAX_ENVELOPE_BYTES = 65_556

export function sessionId(value = 1): Uint8Array {
  const id = new Uint8Array(8)
  new DataView(id.buffer).setBigUint64(0, BigInt(value), false)
  return id
}

export function createLedger(config: Partial<ReceiveLedgerConfig> = {}): RelayReceiveLedger {
  return new RelayReceiveLedger({
    connectionId: 'connection-1',
    capacity: { frames: 256n, encodedBytes: 16n << 20n },
    maxSessions: 8,
    ...config,
  })
}

export function notice(totalFrames: bigint, totalEncodedBytes: bigint, id = 1) {
  return { kind: 'settlement-notice' as const, relaySessionId: sessionId(id), totalFrames, totalEncodedBytes }
}
