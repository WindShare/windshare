import { expect, it, vi } from 'vitest'
import type { ProtocolTraceEvent } from '../../src/session/diagnostics'
import { EnvelopeError, EnvelopeOpener } from '../../src/session/envelope'
import { encodeBody, MESSAGE_KIND } from '../../src/session/message'
import type { LaneChange } from '../../src/session/runtime-types'
import { deferred, runtimeFixture } from './v2-send-fixture'

function detachments(events: readonly ProtocolTraceEvent[]) {
  return events.filter(event => event.eventName === 'lane_transition' && event.transition === 'detached')
}

it('retires the admitted lane once and releases both waiting operations and blocked sends', async () => {
  const { runtime, channel, events } = runtimeFixture()
  const changes: LaneChange[] = []
  runtime.subscribeLaneChanges(change => changes.push(change))
  channel.unblock()
  try {
    const operation = await runtime.beginOperation(MESSAGE_KIND.listChildren, encodeBody([]))
    const response = expect(operation.next()).rejects.toMatchObject({ scope: 'lane' })
    channel.block()
    const sending = expect(runtime.beginOperation(MESSAGE_KIND.listChildren, encodeBody([])))
      .rejects.toMatchObject({ scope: 'lane' })
    await channel.sending.promise
    await runtime.retireLane(1, 0)
    await Promise.all([response, sending])
    await runtime.retireLane(1, 0)
    expect(runtime.isClosed).toBe(false)
    expect(runtime.laneIds()).toEqual([])
    expect(channel.state).toBe('closed')
    expect(detachments(events)).toMatchObject([{
      termination: { kind: 'closed', reason: 'retired' },
      correlation: { lane: { id: 1, epoch: 0 } },
    }])
    expect(changes).toEqual([{ type: 'detached', laneId: 1, laneEpoch: 0 }])
  } finally {
    await runtime.close()
  }
  expect(detachments(events)).toHaveLength(1)
})

it('records session shutdown as closure without fabricating a physical failure', async () => {
  const { runtime, events } = runtimeFixture()
  await runtime.close()
  expect(detachments(events)).toMatchObject([{
    termination: { kind: 'closed', reason: 'session_closed' },
  }])
})

it('treats an unexplained receive EOF as transport loss even without a thrown exception', async () => {
  const { runtime, channel, events } = runtimeFixture()
  const detached = deferred<void>()
  runtime.subscribeLaneChanges(change => { if (change.type === 'detached') detached.resolve() })
  try {
    await channel.close()
    await detached.promise
    await runtime.retireLane(1, 0)
    expect(runtime.isClosed).toBe(false)
    expect(detachments(events)).toMatchObject([{
      termination: { kind: 'physical_failure', failure: { scope: 'lane' } },
    }])
  } finally {
    await runtime.close()
  }
})

it('does not overwrite an observed transport failure when retirement races its cleanup', async () => {
  const { runtime, channel, events } = runtimeFixture()
  const closing = deferred<void>()
  const cleanup = deferred<void>()
  const close = channel.close.bind(channel)
  vi.spyOn(channel, 'close').mockImplementation(async () => {
    closing.resolve()
    await cleanup.promise
    await close()
  })
  const failure = new Error('socket receive failed')
  try {
    channel.failIncoming(failure)
    await closing.promise
    const retirement = runtime.retireLane(1, 0)
    cleanup.resolve()
    await retirement
    expect(detachments(events)).toMatchObject([{
      termination: { kind: 'physical_failure', failure: { cause: { cause: failure } } },
    }])
    expect(runtime.isClosed).toBe(false)
  } finally {
    cleanup.resolve()
    await runtime.close()
  }
})

it('preserves an authentication failure discovered while an already-read frame is being retired', async () => {
  const { runtime, channel, events } = runtimeFixture()
  const verifying = deferred<void>()
  const verification = deferred<void>()
  const failure = new EnvelopeError('authentication failed')
  const open = vi.spyOn(EnvelopeOpener.prototype, 'open').mockImplementationOnce(async () => {
    verifying.resolve()
    await verification.promise
    throw failure
  })
  try {
    channel.receive(Uint8Array.of(0xff))
    await verifying.promise
    const retirement = runtime.retireLane(1, 0)
    verification.resolve()
    await retirement
    expect(runtime.isClosed).toBe(true)
    expect(detachments(events)).toMatchObject([{
      termination: { kind: 'authenticated_failure', failure: { scope: 'session', cause: failure } },
    }])
  } finally {
    verification.resolve()
    await runtime.close()
    open.mockRestore()
  }
})
