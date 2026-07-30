import type { CredentialBrokerSecretStore } from './contracts.ts'
import { arraysOverlap, invalidBrokerResponse } from './pipe-protocol.ts'

const MINIMUM_CREDENTIAL_BYTES = 32
const MAXIMUM_CREDENTIAL_BYTES = 512

export function adoptCredentialSecret(input: {
  readonly payload: Uint8Array
  readonly declaredByteLength: number
  readonly secretStore: CredentialBrokerSecretStore | undefined
}): Uint8Array {
  if (
    input.declaredByteLength !== input.payload.byteLength ||
    !isCredentialBytes(input.payload)
  ) invalidBrokerResponse()
  const credential = input.secretStore?.adopt(input.payload) ?? Buffer.from(input.payload)
  if (
    !(credential instanceof Uint8Array) || arraysOverlap(credential, input.payload) ||
    credential.byteLength !== input.payload.byteLength ||
    !sameBytes(credential, input.payload)
  ) {
    if (credential instanceof Uint8Array) credential.fill(0)
    invalidBrokerResponse()
  }
  return credential
}

function isCredentialBytes(value: Uint8Array): boolean {
  if (
    value.byteLength < MINIMUM_CREDENTIAL_BYTES ||
    value.byteLength > MAXIMUM_CREDENTIAL_BYTES
  ) return false
  for (const byte of value) {
    const alphaNumeric = byte >= 0x30 && byte <= 0x39 ||
      byte >= 0x41 && byte <= 0x5a || byte >= 0x61 && byte <= 0x7a
    if (!alphaNumeric && byte !== 0x2d && byte !== 0x5f) return false
  }
  return true
}

function sameBytes(left: Uint8Array, right: Uint8Array): boolean {
  if (left.byteLength !== right.byteLength) return false
  for (let index = 0; index < left.byteLength; index += 1) {
    if (left[index] !== right[index]) return false
  }
  return true
}
