import { describe, expect, it } from 'vitest'

import {
  createPeerAttemptIdentity,
  createPeerPathIdentityValue,
  createProtocolOperationIdentity,
  createProtocolSessionIdentity,
  equalDiagnosticIdentities,
} from '../../src/session/identities'

function identity(seed: number): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(16)
  bytes[0] = seed
  return bytes
}

describe('v2 diagnostic identities', () => {
  it('copies fixed-width bytes and returns detached projections', () => {
    const source = identity(1)
    const value = createProtocolSessionIdentity(source)
    source[0] = 9

    const first = value.copyBytes()
    expect(first).toEqual(identity(1))
    first[0] = 7
    expect(value.copyBytes()).toEqual(identity(1))
  })

  it('rejects absent and zero identities at the domain boundary', () => {
    expect(() => createProtocolOperationIdentity(new Uint8Array(15))).toThrow(RangeError)
    expect(() => createPeerPathIdentityValue(new Uint8Array(16))).toThrow(RangeError)
    expect(() => createPeerAttemptIdentity(new Uint8Array(17))).toThrow(RangeError)
  })

  it('compares bytes only within the same semantic identity kind', () => {
    const operation = createProtocolOperationIdentity(identity(2))
    expect(equalDiagnosticIdentities(
      operation,
      createProtocolOperationIdentity(identity(2)),
    )).toBe(true)
    expect(equalDiagnosticIdentities(
      operation,
      createPeerAttemptIdentity(identity(2)),
    )).toBe(false)
  })
})
