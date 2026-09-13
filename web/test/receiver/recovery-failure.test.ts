import { describe, expect, it } from 'vitest'
import { SenderObjectError } from '../../src/crypto/sender-object'
import { V2RelayReceiverError } from '../../src/transport/relay/v2-receiver'
import { V2_RELAY_ERROR, V2RelayProtocolError, type V2RelayErrorCode } from '../../src/transport/relay/v2-protocol'
import { V2StaleShareInstanceError } from '../../src/receiver/v2-session-factory'
import { V2CborError } from '../../src/protocol/cbor'
import { V2TranscriptError } from '../../src/session/v2-transcript'
import { V2EnvelopeError } from '../../src/session/v2-envelope'
import { V2SessionRuntimeError } from '../../src/session/v2-runtime-types'
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

  it.each([
    new V2RelayProtocolError('malformed', 'Invalid relay frame'),
    new SenderObjectError('signature', 'Invalid signature'),
    new V2CborError('Invalid descriptor encoding'),
    new V2TranscriptError('Invalid ServerHello'),
    new V2EnvelopeError('Invalid admission envelope'),
  ])('isolates $name to the rejected endpoint', failure => {
    const invalid = new RelayEndpointFailure('invalid-relay', failure)
    expect(isTerminalRecoveryFailure(invalid)).toBe(true)
    expect(isShareRecoveryFailure(invalid)).toBe(false)
    const mixed = new AggregateError([relayFailure(V2_RELAY_ERROR.notFound), invalid])
    expect(isTerminalRecoveryFailure(mixed)).toBe(false)
    expect(isShareRecoveryFailure(mixed)).toBe(false)
    expect(isTerminalRecoveryFailure(new AggregateError([invalid, invalid]))).toBe(true)
  })

  it.each([
    new V2StaleShareInstanceError('Authenticated descriptor changed'),
    new V2SessionRuntimeError('session', 'Established session failed'),
  ])('preserves $name authority through mixed relay outcomes', failure => {
    const mixed = new AggregateError([relayFailure(V2_RELAY_ERROR.notFound),
      new RelayEndpointFailure('conflicting-relay', failure)])
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
