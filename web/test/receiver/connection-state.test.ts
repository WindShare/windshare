import { describe, expect, it } from 'vitest'
import { ReceiverConnectionState, type ReceiverConnectionSnapshot } from '../../src/receiver/connection-state'
import { GenerationRecoveryExhaustedError } from '../../src/receiver/generation-recovery'
import { V2StaleShareInstanceError } from '../../src/receiver/v2-session-factory'
import { V2RelayReceiverError } from '../../src/transport/relay/v2-receiver'
import { V2_RELAY_ERROR } from '../../src/transport/relay/v2-protocol'

describe('receiver connection observations', () => {
  it('keeps interruption and exhausted retries distinct from confirmed share ending', () => {
    const state = new ReceiverConnectionState()
    const events: ReceiverConnectionSnapshot[] = []
    state.subscribe(snapshot => events.push(snapshot))
    state.reconnecting()
    state.reconnecting()
    state.connected()
    state.failed(new GenerationRecoveryExhaustedError())
    expect(events).toEqual([{ kind: 'connected' }, { kind: 'reconnecting' }, { kind: 'connected' },
      { kind: 'unavailable', reason: 'recovery-exhausted' }])
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
