import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  RelayHeartbeat,
  RelayHeartbeatError,
  RELAY_HEARTBEAT_INTERVAL_MILLISECONDS as INTERVAL,
  RELAY_HEARTBEAT_TIMEOUT_MILLISECONDS as TIMEOUT,
} from '../../src/transport/relay/heartbeat'
import { encodeConnectionProbeAck, encodeOpaqueRoute } from '../../src/transport/relay/protocol'
import { OpaqueRelayFrameChannel } from '../../src/transport/relay/receiver/channel'
import { DEFAULT_RELAY_RECEIVE_CAPACITY } from '../../src/transport/relay/receiver/capacity'
import { RELAY_HEARTBEAT_CLOSE_CODE } from '../../src/transport/relay/receiver'
import { FakeSocket, relaySessionId } from './relay-receiver-fixture'

const WALL_CLOCK_ADJUSTMENT = 60 * 60 * 1_000

afterEach(() => vi.useRealTimers())

describe('relay heartbeat elapsed time across wall-clock adjustments', () => {
  it('still retires a silent relay at the timeout after the wall clock moves backward', async () => {
    vi.useFakeTimers()
    const failed = vi.fn()
    const trace = vi.fn()
    const heartbeat = new RelayHeartbeat({ bufferedAmount: 1_000_000, send: vi.fn() }, failed, trace)
    try {
      await vi.advanceTimersByTimeAsync(INTERVAL)
      vi.setSystemTime(Date.now() - WALL_CLOCK_ADJUSTMENT)
      await vi.advanceTimersByTimeAsync(TIMEOUT - INTERVAL - 1)
      expect(failed).not.toHaveBeenCalled()
      await vi.advanceTimersByTimeAsync(1)
      expect(failed).toHaveBeenCalledExactlyOnceWith(expect.any(RelayHeartbeatError))
      expect(trace.mock.calls.at(-1)?.[0]).toMatchObject({ stage: 'failed', elapsedMilliseconds: TIMEOUT })
      expect(vi.getTimerCount()).toBe(0)
    } finally {
      heartbeat.close()
    }
  })

  it('does not retire a recently active relay when the wall clock moves forward', async () => {
    vi.useFakeTimers()
    const failed = vi.fn()
    const heartbeat = new RelayHeartbeat({ bufferedAmount: 0, send: vi.fn() }, failed)
    try {
      await vi.advanceTimersByTimeAsync(INTERVAL)
      heartbeat.observeInbound()
      vi.setSystemTime(Date.now() + WALL_CLOCK_ADJUSTMENT)
      // The original silence callback must use the recent frame, not the changed calendar.
      await vi.advanceTimersByTimeAsync(TIMEOUT - INTERVAL)
      expect(failed).not.toHaveBeenCalled()
      await vi.advanceTimersByTimeAsync(INTERVAL - 1)
      expect(failed).not.toHaveBeenCalled()
      await vi.advanceTimersByTimeAsync(1)
      expect(failed).toHaveBeenCalledExactlyOnceWith(expect.any(RelayHeartbeatError))
      expect(vi.getTimerCount()).toBe(0)
    } finally {
      heartbeat.close()
    }
  })

  it.each([-WALL_CLOCK_ADJUSTMENT, WALL_CLOCK_ADJUSTMENT])(
    'records actual probe round-trip time after a %i ms wall-clock adjustment', async adjustment => {
      vi.useFakeTimers()
      const trace = vi.fn()
      const heartbeat = new RelayHeartbeat({ bufferedAmount: 0, send: vi.fn() }, vi.fn(), trace)
      try {
        await vi.advanceTimersByTimeAsync(INTERVAL)
        const roundTrip = 123
        await vi.advanceTimersByTimeAsync(roundTrip)
        vi.setSystemTime(Date.now() + adjustment)
        expect(heartbeat.receive(encodeConnectionProbeAck(1n))).toBe(true)
        expect(trace.mock.calls.at(-1)?.[0]).toMatchObject({
          stage: 'acknowledged', round: 1n, elapsedMilliseconds: roundTrip,
        })
      } finally {
        heartbeat.close()
      }
      expect(vi.getTimerCount()).toBe(0)
    },
  )
})

describe('relay channel availability across wall-clock adjustments', () => {
  it('withdraws a silent lane and releases pending reads and credit waits after a backward adjustment', async () => {
    vi.useFakeTimers()
    const socket = new FakeSocket(false)
    const channel = new OpaqueRelayFrameChannel(socket, relaySessionId, DEFAULT_RELAY_RECEIVE_CAPACITY)
    const reader = channel.frames.getReader()
    const pending = Promise.allSettled([reader.read(), channel.send(Uint8Array.of(7))])
    try {
      await vi.advanceTimersByTimeAsync(INTERVAL)
      vi.setSystemTime(Date.now() - WALL_CLOCK_ADJUSTMENT)
      await vi.advanceTimersByTimeAsync(TIMEOUT - INTERVAL)
      expect(channel.state).toBe('closed')
      expect(channel.unavailable.aborted).toBe(true)
      expect(socket.closeCode).toBe(RELAY_HEARTBEAT_CLOSE_CODE)
      const outcomes = await pending
      expect(outcomes).toEqual([
        { status: 'rejected', reason: expect.any(RelayHeartbeatError) },
        { status: 'rejected', reason: expect.any(RelayHeartbeatError) },
      ])
    } finally {
      await channel.close()
      await pending
      reader.releaseLock()
    }
    expect(vi.getTimerCount()).toBe(0)
  })

  it('keeps accepted content readable on a healthy lane after a forward adjustment', async () => {
    vi.useFakeTimers()
    const socket = new FakeSocket(false)
    const channel = new OpaqueRelayFrameChannel(socket, relaySessionId, DEFAULT_RELAY_RECEIVE_CAPACITY)
    const reader = channel.frames.getReader()
    const payload = Uint8Array.of(7, 8)
    try {
      await vi.advanceTimersByTimeAsync(INTERVAL)
      socket.message(encodeOpaqueRoute({ relaySessionId, ciphertext: payload }))
      vi.setSystemTime(Date.now() + WALL_CLOCK_ADJUSTMENT)
      await vi.advanceTimersByTimeAsync(TIMEOUT - INTERVAL)
      expect(channel.state).toBe('open')
      expect(channel.unavailable.aborted).toBe(false)
      expect(await reader.read()).toEqual({ done: false, value: payload })
    } finally {
      await channel.close()
      reader.releaseLock()
    }
    expect(vi.getTimerCount()).toBe(0)
  })
})
