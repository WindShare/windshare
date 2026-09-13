import { describe, expect, it } from 'vitest'
import { ReceiverConnectionState, type ReceiverConnectionSnapshot } from '../../src/receiver/connection-state'
import { V2StaleShareInstanceError } from '../../src/receiver/v2-session-factory'
import { V2RelayReceiverError } from '../../src/transport/relay/v2-receiver'
import { V2_RELAY_ERROR } from '../../src/transport/relay/v2-protocol'

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
    const stopped = new V2RelayReceiverError('stopped', { relayError: {
      code: V2_RELAY_ERROR.stopped, retryAfterMilliseconds: 0,
    } })
    state.failed(new AggregateError([stopped, new Error('timeout')]))
    expect(latest?.kind).toBe('unavailable')
    state.failed(new AggregateError([stopped, stopped]))
    expect(latest).toEqual({ kind: 'ended', reason: 'share-stopped' })
    state.failed(new V2StaleShareInstanceError('authenticated share changed'))
    expect(latest).toEqual({ kind: 'ended', reason: 'share-replaced' })
  })
})
