import { describe, expect, it } from 'vitest'

import {
  createFailureIdentity,
  fingerprintFailureFact,
  peerFailureFact,
  unclassifiedFailureFact,
} from '../../../src/diagnostics/incident'

describe('failure fact fingerprinting', () => {
  it('groups by closed semantic fields while excluding correlation identities', () => {
    const left = peerFailureFact({
      stage: 'peer_attempt',
      recoveryDisposition: 'retryable',
      scope: 'attempt-transient',
      code: 'peer-timeout',
      retryable: true,
      correlation: {
        peerPathId: createFailureIdentity('peer_path', identityBytes(1)),
        peerAttemptId: createFailureIdentity('peer_attempt', identityBytes(2)),
        lane: { id: 7, epoch: 1 },
      },
    })
    const right = peerFailureFact({
      stage: 'peer_attempt',
      recoveryDisposition: 'retryable',
      scope: 'attempt-transient',
      code: 'peer-timeout',
      retryable: true,
      correlation: {
        peerPathId: createFailureIdentity('peer_path', identityBytes(9)),
        peerAttemptId: createFailureIdentity('peer_attempt', identityBytes(10)),
        lane: { id: 200, epoch: 300 },
      },
    })

    expect(fingerprintFailureFact(left)).toBe(fingerprintFailureFact(right))
    expect(fingerprintFailureFact(left)).toBe(
      'peer_failure:peer_attempt:retryable:attempt-transient:peer-timeout:1',
    )
  })

  it('changes for each semantic discriminator and is stable across calls', () => {
    const baseline = unclassifiedFailureFact({
      stage: 'browse',
      recoveryDisposition: 'terminal',
    })
    const stageChanged = unclassifiedFailureFact({
      stage: 'join',
      recoveryDisposition: 'terminal',
    })
    const recoveryChanged = unclassifiedFailureFact({
      stage: 'browse',
      recoveryDisposition: 'retryable',
    })

    expect(fingerprintFailureFact(baseline)).toBe(
      fingerprintFailureFact(baseline),
    )
    expect(fingerprintFailureFact(stageChanged)).not.toBe(
      fingerprintFailureFact(baseline),
    )
    expect(fingerprintFailureFact(recoveryChanged)).not.toBe(
      fingerprintFailureFact(baseline),
    )
  })
})

function identityBytes(seed: number): Uint8Array {
  const bytes = new Uint8Array(16)
  bytes[0] = seed
  return bytes
}
