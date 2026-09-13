import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  RelayHeartbeat,
  RelayHeartbeatError,
  V2_RELAY_HEARTBEAT_INTERVAL_MILLISECONDS as INTERVAL,
  V2_RELAY_HEARTBEAT_TIMEOUT_MILLISECONDS as TIMEOUT,
} from '../../src/transport/relay/heartbeat'
import {
  decodeV2ConnectionProbe,
  decodeV2ConnectionProbeAck,
  encodeV2ConnectionProbe,
  encodeV2ConnectionProbeAck,
} from '../../src/transport/relay/v2-protocol'

afterEach(() => vi.useRealTimers())

describe('connection probe wire contract', () => {
  it.each([
    [encodeV2ConnectionProbe, decodeV2ConnectionProbe, 72],
    [encodeV2ConnectionProbeAck, decodeV2ConnectionProbeAck, 65],
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
  it('bounds silent loss even while outbound data remains blocked', async () => {
    vi.useFakeTimers()
    const socket = { bufferedAmount: 1_000_000, send: vi.fn() }
    const failed = vi.fn()
    const trace = vi.fn()
    const heartbeat = new RelayHeartbeat(socket, failed, trace)
    await vi.advanceTimersByTimeAsync(INTERVAL)
    expect(socket.send).toHaveBeenCalledTimes(1)
    expect(decodeV2ConnectionProbe(socket.send.mock.calls[0]?.[0] as Uint8Array)).toBe(1n)
    await vi.advanceTimersByTimeAsync(TIMEOUT - 1)
    expect(failed).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(1)
    expect(failed).toHaveBeenCalledWith(expect.any(RelayHeartbeatError))
    expect(trace.mock.calls.map(call => call[0].stage)).toEqual(['probe', 'failed'])
    expect(trace.mock.calls[1]?.[0]).toMatchObject({ round: 1n, bufferedBytes: 1_000_000, elapsedMilliseconds: TIMEOUT })
    await vi.advanceTimersByTimeAsync(10 * TIMEOUT)
    expect(socket.send).toHaveBeenCalledTimes(1)
    heartbeat.close()
  })

  it('tolerates bounded read and write pressure without depending on content traffic', async () => {
    vi.useFakeTimers()
    const socket = { bufferedAmount: 0, send: vi.fn() }
    const failed = vi.fn()
    const heartbeat = new RelayHeartbeat(socket, failed)
    await vi.advanceTimersByTimeAsync(INTERVAL + 30_000)
    expect(heartbeat.receive(encodeV2ConnectionProbeAck(1n))).toBe(true)
    await vi.advanceTimersByTimeAsync(INTERVAL)
    expect(socket.send).toHaveBeenCalledTimes(2)
    expect(heartbeat.receive(encodeV2ConnectionProbeAck(2n))).toBe(true)
    expect(failed).not.toHaveBeenCalled()
    heartbeat.close()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('never treats duplicate responses or content traffic as a current acknowledgement', async () => {
    vi.useFakeTimers()
    const socket = { bufferedAmount: 0, send: vi.fn() }
    const failed = vi.fn()
    const heartbeat = new RelayHeartbeat(socket, failed)
    await vi.advanceTimersByTimeAsync(INTERVAL)
    heartbeat.receive(encodeV2ConnectionProbeAck(1n))
    await vi.advanceTimersByTimeAsync(INTERVAL)
    expect(heartbeat.receive(encodeV2ConnectionProbeAck(1n))).toBe(true)
    expect(heartbeat.receive(new TextEncoder().encode('WS2Ocontent'))).toBe(false)
    expect(heartbeat.receive(new Uint8Array(1))).toBe(false)
    await vi.advanceTimersByTimeAsync(TIMEOUT)
    expect(failed).toHaveBeenCalledTimes(1)
    heartbeat.receive(encodeV2ConnectionProbeAck(2n))
    expect(vi.getTimerCount()).toBe(0)
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

  it('cancels pending idle and response timers when its owner closes', async () => {
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
