import { describe, expect, it } from 'vitest'

import { LANE_REJECT } from '../../src/session/lane-codec'
import {
  classifyPeerAttemptFailure,
  type PeerAttemptFailure,
  type PeerLocalContractCode,
  type PeerLocalPolicyCode,
  type PeerLocalTransientReason,
} from '../../src/connectivity/peer-failure'

describe('v2 peer failure classifier', () => {
  const transientReasons: readonly PeerLocalTransientReason[] = [
    'negotiation-timeout',
    'admission-timeout',
    'transport-loss',
    'signaling-delivery-loss',
    'lane-installation-failed',
  ]

  it.each(transientReasons)('retries audited local transient %s', (reason) => {
    const phase = reason === 'negotiation-timeout' ? 'negotiation' : 'admission'
    expect(classifyPeerAttemptFailure({
      kind: 'local-transient',
      phase,
      reason,
    })).toEqual({
      type: 'retry-attempt',
      reason: 'local-transient',
    })
  })

  it('retries only the two authenticated lane rejection dispositions that permit replacement', () => {
    expect(classifyPeerAttemptFailure({
      kind: 'authenticated-lane-rejection',
      rejection: {
        code: LANE_REJECT.grantExpired,
        retryAfterMilliseconds: 0,
      },
    })).toEqual({
      type: 'retry-attempt',
      reason: 'grant-expired',
    })
    expect(classifyPeerAttemptFailure({
      kind: 'authenticated-lane-rejection',
      rejection: {
        code: LANE_REJECT.admissionLimited,
        retryAfterMilliseconds: 12_345,
      },
    })).toEqual({
      type: 'retry-attempt',
      reason: 'admission-limited',
      authenticatedRetryAfterMilliseconds: 12_345,
    })
  })

  const finalLaneRejections = [
    LANE_REJECT.unknownSession,
    LANE_REJECT.staleEpoch,
    LANE_REJECT.grantConsumed,
    LANE_REJECT.stopping,
    LANE_REJECT.grantMismatch,
  ] as const

  it.each(finalLaneRejections)('stops the path for authenticated lane rejection %s', (code) => {
    expect(classifyPeerAttemptFailure({
      kind: 'authenticated-lane-rejection',
      rejection: { code, retryAfterMilliseconds: 0 },
    })).toEqual({
      type: 'stop-path',
      reason: 'lane-rejection-final',
    })
  })

  const policyCodes: readonly PeerLocalPolicyCode[] = [
    'unsupported-capability',
    'candidate-limit',
    'unexpected-data-channel',
    'configured-policy-refusal',
  ]

  it.each(policyCodes)('stops the path for local policy %s', (code) => {
    expect(classifyPeerAttemptFailure({
      kind: 'local-policy',
      code,
    })).toEqual({ type: 'stop-path', reason: 'local-policy' })
  })

  const contractCodes: readonly PeerLocalContractCode[] = [
    'invalid-adapter-result',
    'invalid-lane-response',
    'invalid-proof',
    'invalid-signature',
    'identity-mismatch',
    'unknown-local-failure',
  ]

  it.each(contractCodes)('stops the path for local contract failure %s', (code) => {
    expect(classifyPeerAttemptFailure({
      kind: 'local-contract',
      code,
    })).toEqual({ type: 'stop-path', reason: 'local-contract' })
  })

  it('keeps an authenticated peer operation final at path scope', () => {
    expect(classifyPeerAttemptFailure({
      kind: 'authenticated-peer-operation',
      code: 0x5004,
    })).toEqual({
      type: 'stop-path',
      reason: 'peer-operation-final',
    })
  })

  it('retries only closed transient peer reasons and keeps unknown reasons path-terminal', () => {
    expect(classifyPeerAttemptFailure({ kind: 'authenticated-peer-operation', code: 0x5002 }))
      .toMatchObject({ type: 'retry-attempt' })
    expect(classifyPeerAttemptFailure({ kind: 'authenticated-peer-operation', code: 0x5fff }))
      .toMatchObject({ type: 'stop-path' })
  })

  it('reflects only an explicit sealed ProtocolSession terminal snapshot', () => {
    const terminal = Object.freeze({
      authority: 'protocol-session-terminal',
      code: 'binding-conflict',
    } as const)
    expect(classifyPeerAttemptFailure({
      kind: 'session-terminal',
      terminal,
    })).toEqual({
      type: 'stop-session',
      reason: 'session-terminal',
      terminal,
    })
  })

  it.each([
    new Error('runtime closed'),
    { kind: 'session-terminal', terminal: { code: 'runtime-closed' } },
    { kind: 'local-transient', phase: 'admission', reason: 'unknown' },
    { kind: 'local-transient', phase: 'admission', reason: 'negotiation-timeout' },
    { kind: 'local-transient', phase: 'negotiation', reason: 'admission-timeout' },
    {
      kind: 'authenticated-lane-rejection',
      rejection: {
        code: LANE_REJECT.grantExpired,
        retryAfterMilliseconds: 1,
      },
    },
    {
      kind: 'authenticated-lane-rejection',
      rejection: {
        code: LANE_REJECT.admissionLimited,
        retryAfterMilliseconds: 30_001,
      },
    },
    {
      kind: 'lifecycle-cancelled',
      owner: 'runtime-stop',
      cause: { kind: 'session-terminal', authority: true },
    },
  ])('fails unknown or malformed input closed at path scope', (failure) => {
    expect(classifyPeerAttemptFailure(failure)).toEqual({
      type: 'stop-path',
      reason: 'untyped-failure',
    })
  })

  it('has no classifier path for lifecycle cancellation', () => {
    const cancellation = {
      type: 'lifecycle-cancelled',
      owner: 'last-activation',
    }
    expect(classifyPeerAttemptFailure(cancellation)).toEqual({
      type: 'stop-path',
      reason: 'untyped-failure',
    })
  })

  it('keeps the public failure union exhaustive without arbitrary causes', () => {
    const failure: PeerAttemptFailure = {
      kind: 'local-contract',
      code: 'unknown-local-failure',
    }
    expect(Object.keys(failure)).toEqual(['kind', 'code'])
  })
})
