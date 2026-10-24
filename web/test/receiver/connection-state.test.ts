import { describe, expect, it } from 'vitest'
import { ReceiverConnectionState, type ReceiverConnectionSnapshot } from '../../src/receiver/connection-state'
import { StaleShareInstanceError } from '../../src/receiver/session-factory'
import { RelayReceiverError } from '../../src/transport/relay/receiver'
import { RELAY_ERROR } from '../../src/transport/relay/protocol'

describe('receiver connection observations', () => {
  it('distinguishes an active attempt from scheduled recovery without duplicate observations', () => {
    const state = new ReceiverConnectionState()
    const events: ReceiverConnectionSnapshot[] = []
    state.subscribe(snapshot => events.push(snapshot))
    state.reconnecting({ kind: 'connecting' })
    state.reconnecting({ kind: 'connecting' })
    state.reconnecting({ kind: 'waiting', reason: 'capacity', retryAt: 75_000 })
    state.connected()
    expect(events).toEqual([{ kind: 'connected' },
      { kind: 'reconnecting', activity: { kind: 'connecting' } },
      { kind: 'reconnecting', activity: { kind: 'waiting', reason: 'capacity', retryAt: 75_000 } },
      { kind: 'connected' }])
  })

  it('requires authenticated replacement or all stopped endpoints and isolates observers', () => {
    const state = new ReceiverConnectionState()
    expect(() => state.subscribe(() => { throw new Error('observer failed') })).not.toThrow()
    let latest: ReceiverConnectionSnapshot | undefined
    state.subscribe(snapshot => { latest = snapshot })
    const stopped = new RelayReceiverError('stopped', { relayError: {
      code: RELAY_ERROR.stopped, retryAfterMilliseconds: 0,
    } })
    state.failed(new AggregateError([stopped, new Error('timeout')]))
    expect(latest?.kind).toBe('unavailable')
    state.failed(new AggregateError([stopped, stopped]))
    expect(latest).toEqual({ kind: 'ended', reason: 'share-stopped' })
    state.failed(new StaleShareInstanceError('authenticated share changed'))
    expect(latest).toEqual({ kind: 'ended', reason: 'share-replaced' })
  })
})
