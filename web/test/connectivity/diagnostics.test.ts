import { describe, expect, it } from 'vitest'

import {
  V2_BROWSER_CONNECTIVITY_ATTEMPT_STAGES,
  V2_PEER_OPERATION_ERROR_REGISTRY,
  V2_TYPED_PEER_ERROR_CODES,
  v2TypedErrorForPeerOperationCode,
} from '../../src/connectivity/diagnostics'
import { V2_PEER_OPERATION_CODE } from '../../src/session/v2-message'

describe('connectivity diagnostics vocabulary', () => {
  it('maps every authenticated peer operation code into a stable typed error', () => {
    expect(V2_PEER_OPERATION_ERROR_REGISTRY).toEqual([
      { code: V2_PEER_OPERATION_CODE.negotiation, typedErrorCode: 'peer-negotiation' },
      { code: V2_PEER_OPERATION_CODE.timeout, typedErrorCode: 'peer-timeout' },
      { code: V2_PEER_OPERATION_CODE.candidates, typedErrorCode: 'peer-candidates' },
      { code: V2_PEER_OPERATION_CODE.admission, typedErrorCode: 'peer-admission' },
    ])
    for (const entry of V2_PEER_OPERATION_ERROR_REGISTRY) {
      expect(v2TypedErrorForPeerOperationCode(entry.code)).toBe(entry.typedErrorCode)
    }
    expect(v2TypedErrorForPeerOperationCode(0xffff)).toBeUndefined()
  })

  it('keeps the observer vocabulary immutable and terminally explicit', () => {
    expect(Object.isFrozen(V2_BROWSER_CONNECTIVITY_ATTEMPT_STAGES)).toBe(true)
    expect(Object.isFrozen(V2_TYPED_PEER_ERROR_CODES)).toBe(true)
    expect(V2_BROWSER_CONNECTIVITY_ATTEMPT_STAGES.at(-1)).toBe('failed')
    expect(V2_TYPED_PEER_ERROR_CODES).toContain('signaling-contract')
    expect(V2_TYPED_PEER_ERROR_CODES).toContain('attempt-cancelled')
  })
})
