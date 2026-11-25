import { describe, expect, it } from 'vitest'
import { SenderObjectError } from '../../src/crypto/sender-object'
import { RelayReceiverError } from '../../src/transport/relay/receiver'
import { RELAY_ERROR, RelayProtocolError, type RelayErrorCode } from '../../src/transport/relay/protocol'
import { StaleShareInstanceError } from '../../src/receiver/session-factory'
import { CborError } from '../../src/protocol/cbor'
import { TranscriptError } from '../../src/session/transcript'
import { EnvelopeError } from '../../src/session/envelope'
import { OperationRetryError, SessionRuntimeError } from '../../src/session/runtime-types'
import { BlockLaneAttemptsError } from '../../src/content/broker'
import { BlockInactivityTimeoutError } from '../../src/content/scheduling/block-response-wait'
import {
  isLaneRecoveryFailure,
  isShareRecoveryFailure,
  isTerminalRecoveryFailure,
  recoveryRetry,
  stalledBlockLanes,
} from '../../src/receiver/recovery-failure'
import { isRecoverableTransferInterruption } from '../../src/transfer/fault'
import { RelayEndpointFailure } from '../../src/receiver/relay-selection/race'

function relayFailure(code: RelayErrorCode, retryAfterMilliseconds = 0) {
  return new RelayReceiverError('Relay rejection', { relayError: { code, retryAfterMilliseconds } })
}

describe('receiver failure scope', () => {
  it('keeps NotFound and admission transient while remembering an endpoint stop', () => {
    const transient = relayFailure(RELAY_ERROR.notFound)
    const stopped = new RelayEndpointFailure('stopped-relay', relayFailure(RELAY_ERROR.stopped))
    expect(isTerminalRecoveryFailure(transient)).toBe(false)
    expect(isTerminalRecoveryFailure(relayFailure(RELAY_ERROR.admission))).toBe(false)
    expect(isTerminalRecoveryFailure(stopped)).toBe(true)
    expect(isShareRecoveryFailure(stopped)).toBe(false)
    expect(isTerminalRecoveryFailure(new AggregateError([stopped, transient]))).toBe(false)
    expect(isTerminalRecoveryFailure(new AggregateError([stopped, stopped]))).toBe(true)
  })

  it.each([
    new RelayProtocolError('malformed', 'Invalid relay frame'),
    new SenderObjectError('signature', 'Invalid signature'),
    new CborError('Invalid descriptor encoding'),
    new TranscriptError('Invalid ServerHello'),
    new EnvelopeError('Invalid admission envelope'),
  ])('isolates $name to the rejected endpoint', failure => {
    const invalid = new RelayEndpointFailure('invalid-relay', failure)
    expect(isTerminalRecoveryFailure(invalid)).toBe(true)
    expect(isShareRecoveryFailure(invalid)).toBe(false)
    const mixed = new AggregateError([relayFailure(RELAY_ERROR.notFound), invalid])
    expect(isTerminalRecoveryFailure(mixed)).toBe(false)
    expect(isShareRecoveryFailure(mixed)).toBe(false)
    expect(isTerminalRecoveryFailure(new AggregateError([invalid, invalid]))).toBe(true)
  })

  it.each([
    new StaleShareInstanceError('Authenticated descriptor changed'),
    new SessionRuntimeError('session', 'Established session failed'),
  ])('preserves $name authority through mixed relay outcomes', failure => {
    const mixed = new AggregateError([relayFailure(RELAY_ERROR.notFound),
      new RelayEndpointFailure('conflicting-relay', failure)])
    expect(isTerminalRecoveryFailure(mixed)).toBe(true)
    expect(isShareRecoveryFailure(mixed)).toBe(true)
    expect(isShareRecoveryFailure(new Error('Output storage is full'))).toBe(false)
  })

  it('keeps endpoint-scoped hints out of the connection-wide retry deadline', () => {
    const scoped = new RelayEndpointFailure('relay-a', relayFailure(RELAY_ERROR.starting, 900))
    expect(recoveryRetry(scoped, 10)).toBeUndefined()
    expect(recoveryRetry(new AggregateError([scoped, relayFailure(RELAY_ERROR.admission, 100)]), 10)).toEqual({
      retryAt: 110, reason: 'server',
    })
  })

  it('preserves distinct lane epochs while deduplicating exact stalled lane identities', () => {
    const combined = new BlockLaneAttemptsError([
      new BlockInactivityTimeoutError('awaiting_first_fragment', 15_000, 0, { lane: { laneId: 7, laneEpoch: 1 } }),
      new BlockInactivityTimeoutError('receiving_fragments', 15_000, 0, { lane: { laneId: 7, laneEpoch: 1 } }),
      new BlockInactivityTimeoutError('receiving_fragments', 15_000, 0, { lane: { laneId: 7, laneEpoch: 2 } }),
      new BlockInactivityTimeoutError('awaiting_first_fragment', 15_000, 0),
    ])
    expect(stalledBlockLanes(combined)).toEqual([
      { laneId: 7, laneEpoch: 1 },
      { laneId: 7, laneEpoch: 2 },
    ])
  })

  it('keeps OperationRetryError distinct from lane faults while preserving recoverability', () => {
    const retry = new OperationRetryError()
    expect(retry).not.toBeInstanceOf(SessionRuntimeError)
    expect(isLaneRecoveryFailure(retry)).toBe(true)
    expect(isRecoverableTransferInterruption(retry)).toBe(true)
    expect(isTerminalRecoveryFailure(retry)).toBe(false)
    expect(isShareRecoveryFailure(retry)).toBe(false)
  })
})
