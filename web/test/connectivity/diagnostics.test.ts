import { describe, expect, it } from 'vitest'

import {
  BROWSER_CONNECTIVITY_ATTEMPT_STAGES,
  BROWSER_CONNECTIVITY_RECOVERY_STAGES,
  PEER_OPERATION_ERROR_REGISTRY,
  TYPED_PEER_ERROR_CODES,
  typedErrorForPeerOperationCode,
} from '../../src/connectivity/diagnostics'
import { PEER_OPERATION_CODE } from '../../src/session/message'

describe('connectivity diagnostics vocabulary', () => {
  it('maps every authenticated peer operation code into a stable typed error', () => {
    expect(PEER_OPERATION_ERROR_REGISTRY).toEqual([
      { code: PEER_OPERATION_CODE.negotiation, typedErrorCode: 'peer-negotiation' },
      { code: PEER_OPERATION_CODE.timeout, typedErrorCode: 'peer-timeout' },
      { code: PEER_OPERATION_CODE.candidates, typedErrorCode: 'peer-candidates' },
      { code: PEER_OPERATION_CODE.admission, typedErrorCode: 'peer-admission' },
      { code: PEER_OPERATION_CODE.capacity, typedErrorCode: 'peer-busy' },
    ])
    for (const entry of PEER_OPERATION_ERROR_REGISTRY) {
      expect(typedErrorForPeerOperationCode(entry.code)).toBe(entry.typedErrorCode)
    }
    expect(typedErrorForPeerOperationCode(0xffff)).toBeUndefined()
  })

  it('keeps the closed attempt and recovery vocabularies exact and immutable', () => {
    expect(Object.isFrozen(BROWSER_CONNECTIVITY_ATTEMPT_STAGES)).toBe(true)
    expect(Object.isFrozen(BROWSER_CONNECTIVITY_RECOVERY_STAGES)).toBe(true)
    expect(Object.isFrozen(TYPED_PEER_ERROR_CODES)).toBe(true)
    expect(BROWSER_CONNECTIVITY_ATTEMPT_STAGES).toEqual([
      'started',
      'negotiation-deadline-armed',
      'negotiation-deadline-expired',
      'offer-created',
      'offer-sent',
      'answer-received',
      'datachannel-open',
      'admission-deadline-armed',
      'admission-deadline-expired',
      'grant-requested',
      'grant-received',
      'lane-hello-sent',
      'admission-response-received',
      'admission-response-settled',
      'lane-attached',
      'admitted',
      'failed',
    ])
    expect(BROWSER_CONNECTIVITY_RECOVERY_STAGES).toEqual([
      'parallelism',
      'wave-started',
      'retry-decided',
      'backoff-scheduled',
      'attempt-replaced',
      'wave-quiesced',
      'wave-rearmed',
      'peer-detached',
      'session-budget-exhausted',
      'path-stopped',
      'session-stopped',
    ])
    expect(TYPED_PEER_ERROR_CODES).toContain('signaling-contract')
    expect(TYPED_PEER_ERROR_CODES).toContain('attempt-cancelled')
  })
})
