import { describe, expect, it } from 'vitest'
import { SenderObjectError } from '../../src/crypto/sender-object'
import { V2RelayReceiverError } from '../../src/transport/relay/v2-receiver'
import { V2_RELAY_ERROR, type V2RelayErrorCode } from '../../src/transport/relay/v2-protocol'
import { isShareRecoveryFailure, isTerminalRecoveryFailure, recoveryRetryAfter } from '../../src/receiver/recovery-failure'
import { RelayEndpointFailure } from '../../src/receiver/relay-race'

function relayFailure(code: V2RelayErrorCode, retryAfterMilliseconds = 0) {
  return new V2RelayReceiverError('Relay rejection', { relayError: { code, retryAfterMilliseconds } })
}

describe('receiver failure scope', () => {
  it('keeps NotFound and admission transient while remembering an endpoint stop', () => {
    const transient = relayFailure(V2_RELAY_ERROR.notFound)
    const stopped = new RelayEndpointFailure('stopped-relay', relayFailure(V2_RELAY_ERROR.stopped))
    expect(isTerminalRecoveryFailure(transient)).toBe(false)
    expect(isTerminalRecoveryFailure(relayFailure(V2_RELAY_ERROR.admission))).toBe(false)
    expect(isTerminalRecoveryFailure(stopped)).toBe(true)
    expect(isShareRecoveryFailure(stopped)).toBe(false)
    expect(isTerminalRecoveryFailure(new AggregateError([stopped, transient]))).toBe(false)
    expect(isTerminalRecoveryFailure(new AggregateError([stopped, stopped]))).toBe(true)
  })

  it('preserves authentication failure authority through mixed relay outcomes', () => {
    const invalid = new RelayEndpointFailure('invalid-relay', new SenderObjectError('signature', 'Invalid signature'))
    const mixed = new AggregateError([relayFailure(V2_RELAY_ERROR.notFound), invalid])
    expect(isTerminalRecoveryFailure(mixed)).toBe(true)
    expect(isShareRecoveryFailure(mixed)).toBe(true)
    expect(isShareRecoveryFailure(new Error('Output storage is full'))).toBe(false)
  })

  it('preserves the largest admission retry hint across endpoint wrappers', () => {
    expect(recoveryRetryAfter(new AggregateError([
      new RelayEndpointFailure('relay-a', relayFailure(V2_RELAY_ERROR.starting, 100)),
      relayFailure(V2_RELAY_ERROR.admission, 900),
    ]))).toBe(900)
  })
})
