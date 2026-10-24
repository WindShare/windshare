import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  RelayHeartbeat,
  RelayHeartbeatError,
  RELAY_HEARTBEAT_INTERVAL_MILLISECONDS as INTERVAL,
  RELAY_HEARTBEAT_TIMEOUT_MILLISECONDS as TIMEOUT,
} from '../../src/transport/relay/heartbeat'
import {
  decodeConnectionProbe,
  decodeConnectionProbeAck,
  encodeConnectionProbe,
  encodeConnectionProbeAck,
} from '../../src/transport/relay/protocol'

afterEach(() => vi.useRealTimers())

describe('connection probe wire contract', () => {
  it.each([
    [encodeConnectionProbe, decodeConnectionProbe, 72],
    [encodeConnectionProbeAck, decodeConnectionProbeAck, 65],
  ] as const)('preserves the canonical nonce and rejects malformed controls', (encode, decode, magic) => {
    const encoded = encode(0x0102030405060708n)
    expect([...encoded]).toEqual([87, 83, 50, magic, 2, 0, 0, 0, 1, 2, 3, 4, 5, 6, 7, 8])
    expect(decode(encoded)).toBe(0x0102030405060708n)
    expect(() => encode(0n)).toThrow()
    expect(() => encode(-1n)).toThrow()
    expect(() => encode(0x1_0000_0000_0000_0000n)).toThrow()
    for (const offset of [0, 4, 5, 6, 7]) {
      const bad = encoded.slice()
      bad[offset] = (bad[offset] ?? 0) ^ 1
      expect(() => decode(bad)).toThrow()
    }
    expect(() => decode(encoded.subarray(0, 15))).toThrow()
    expect(() => decode(Uint8Array.from([...encoded, 0]))).toThrow()
    encoded.fill(0, 8)
    expect(() => decode(encoded)).toThrow()
  })
})

describe('relay heartbeat ownership', () => {
  it('fails after a full timeout without inbound frames even while outbound data remains blocked', async () => {
    vi.useFakeTimers()
    const socket = { bufferedAmount: 1_000_000, send: vi.fn() }
    const failed = vi.fn()
    const trace = vi.fn()
    const heartbeat = new RelayHeartbeat(socket, failed, trace)
    await vi.advanceTimersByTimeAsync(TIMEOUT - 1)
    expect(failed).not.toHaveBeenCalled()
    expect(socket.send.mock.calls.map(call => decodeConnectionProbe(call[0] as Uint8Array)))
      .toEqual([1n, 2n])
    await vi.advanceTimersByTimeAsync(1)
    expect(failed).toHaveBeenCalledWith(expect.any(RelayHeartbeatError))
    expect(trace.mock.calls.map(call => call[0].stage)).toEqual(['probe', 'probe', 'failed'])
    expect(trace.mock.calls[2]?.[0]).toMatchObject({ round: 2n, bufferedBytes: 1_000_000, elapsedMilliseconds: TIMEOUT })
    await vi.advanceTimersByTimeAsync(10 * TIMEOUT)
    expect(socket.send).toHaveBeenCalledTimes(2)
    heartbeat.close()
  })

  it('keeps probing and stays alive while content outlives delayed acknowledgements', async () => {
    vi.useFakeTimers()
    const socket = { bufferedAmount: 0, send: vi.fn() }
    const failed = vi.fn()
    const trace = vi.fn()
    const heartbeat = new RelayHeartbeat(socket, failed, trace)
    for (let elapsed = 0; elapsed < 4 * TIMEOUT; elapsed += INTERVAL) {
      await vi.advanceTimersByTimeAsync(INTERVAL)
      heartbeat.observeInbound()
    }
    expect(failed).not.toHaveBeenCalled()
    const sent = socket.send.mock.calls.length
    expect(sent).toBe((4 * TIMEOUT) / INTERVAL)
    // One coalesced acknowledgement covers every earlier probe.
    expect(heartbeat.receive(encodeConnectionProbeAck(BigInt(sent)))).toBe(true)
    const acknowledged = trace.mock.calls.filter(call => call[0].stage === 'acknowledged').map(call => call[0].round)
    expect(acknowledged).toEqual([BigInt(sent)])
    heartbeat.close()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('rejects acknowledgements that are repeated or name an unsent probe', async () => {
    vi.useFakeTimers()
    const socket = { bufferedAmount: 0, send: vi.fn() }
    const heartbeat = new RelayHeartbeat(socket, vi.fn())
    expect(() => heartbeat.receive(encodeConnectionProbeAck(1n))).toThrow(/no outstanding probe/)
    await vi.advanceTimersByTimeAsync(INTERVAL)
    expect(heartbeat.receive(encodeConnectionProbeAck(1n))).toBe(true)
    expect(() => heartbeat.receive(encodeConnectionProbeAck(1n))).toThrow(/no outstanding probe/)
    expect(heartbeat.receive(new TextEncoder().encode('WS2Ocontent'))).toBe(false)
    expect(heartbeat.receive(new Uint8Array(1))).toBe(false)
    heartbeat.close()
  })

  it('contains trace failure and ends immediately on a failed probe write', async () => {
    vi.useFakeTimers()
    const failure = new Error('socket closed')
    const failed = vi.fn()
    const heartbeat = new RelayHeartbeat(
      { bufferedAmount: 0, send: () => { throw failure } }, failed,
      () => { throw new Error('trace unavailable') },
    )
    await vi.advanceTimersByTimeAsync(INTERVAL)
    expect(failed).toHaveBeenCalledWith(failure)
    heartbeat.close()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('cancels pending probe and silence timers when its owner closes', async () => {
    vi.useFakeTimers()
    const socket = { bufferedAmount: 0, send: vi.fn() }
    const failed = vi.fn()
    const idle = new RelayHeartbeat(socket, failed)
    idle.close()
    await vi.advanceTimersByTimeAsync(INTERVAL)
    expect(socket.send).not.toHaveBeenCalled()
    const pending = new RelayHeartbeat(socket, failed)
    await vi.advanceTimersByTimeAsync(INTERVAL)
    pending.close()
    await vi.advanceTimersByTimeAsync(TIMEOUT)
    expect(failed).not.toHaveBeenCalled()
    expect(vi.getTimerCount()).toBe(0)
  })
})
