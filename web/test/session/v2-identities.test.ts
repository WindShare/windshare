import { describe, expect, it } from 'vitest'

import {
  createV2PeerAttemptIdentity,
  createV2PeerPathIdentityValue,
  createV2ProtocolOperationIdentity,
  createV2ProtocolSessionIdentity,
  equalV2DiagnosticIdentities,
} from '../../src/session/v2-identities'

function identity(seed: number): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(16)
  bytes[0] = seed
  return bytes
}

describe('v2 diagnostic identities', () => {
  it('copies fixed-width bytes and returns detached projections', () => {
    const source = identity(1)
    const value = createV2ProtocolSessionIdentity(source)
    source[0] = 9

    const first = value.copyBytes()
    expect(first).toEqual(identity(1))
    first[0] = 7
    expect(value.copyBytes()).toEqual(identity(1))
  })

  it('rejects absent and zero identities at the domain boundary', () => {
    expect(() => createV2ProtocolOperationIdentity(new Uint8Array(15))).toThrow(RangeError)
    expect(() => createV2PeerPathIdentityValue(new Uint8Array(16))).toThrow(RangeError)
    expect(() => createV2PeerAttemptIdentity(new Uint8Array(17))).toThrow(RangeError)
  })

  it('compares bytes only within the same semantic identity kind', () => {
    const operation = createV2ProtocolOperationIdentity(identity(2))
    expect(equalV2DiagnosticIdentities(
      operation,
      createV2ProtocolOperationIdentity(identity(2)),
    )).toBe(true)
    expect(equalV2DiagnosticIdentities(
      operation,
      createV2PeerAttemptIdentity(identity(2)),
    )).toBe(false)
  })
})
