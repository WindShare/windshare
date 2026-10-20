import { expect, it } from 'vitest'
import { PEER_CONTINUATION_SLOTS, PeerSlotPool } from '../../src/session/peer-slots/slots'

it('reuses bounded slots without resetting sequences or reviving closed owners', () => {
  const pool = new PeerSlotPool()
  const leases = Array.from({ length: PEER_CONTINUATION_SLOTS }, () => pool.acquire())
  expect(leases.map((lease) => lease.next().peerSlot)).toEqual([0, 1, 2, 3])
  expect(() => pool.acquire()).toThrow('occupied')

  let previous = leases[1]!
  for (let sequence = 2n; sequence <= 32n; sequence++) {
    previous.close()
    const replacement = pool.acquire()
    expect(replacement.next()).toEqual({ peerSlot: 1, attemptSequence: sequence })
    expect(() => previous.next()).toThrow('closed')
    previous.close()
    expect(() => pool.acquire()).toThrow('occupied')
    previous = replacement
  }
})
