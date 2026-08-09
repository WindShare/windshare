import { decodeBase64Url, encodeBase64Url } from '../../crypto/bytes'
import { STABLE_IDENTITY_BYTES, type CanonicalBytes } from './model'

export function createOperationID(): string {
  return createRandomIdentity('operation')
}

export function createDestinationReservationID(): string {
  return createRandomIdentity('destination reservation')
}

export function createWorkspaceID(): string {
  return createRandomIdentity('workspace')
}

export function createPortablePlanID(): string {
  return createRandomIdentity('portable plan')
}

export function createTransferJobID(): string {
  return createRandomIdentity('transfer job')
}

export function createOutputSessionID(): string {
  return createRandomIdentity('output session')
}

export function createRandomIdentity(label: string): string {
  if (globalThis.crypto?.getRandomValues === undefined) {
    throw new DOMException('Secure ' + label + ' identity generation is unavailable', 'NotSupportedError')
  }
  const value = new Uint8Array(STABLE_IDENTITY_BYTES)
  globalThis.crypto.getRandomValues(value)
  if (value.every((byte) => byte === 0)) throw new Error('Generated ' + label + ' identity was all zeroes')
  return encodeBase64Url(value)
}

export function requireIdentity(value: string, width: number, label: string): string {
  const bytes = requireIdentityBytes(value, width, label)
  return encodeBase64Url(bytes)
}

export function requireIdentityBytes(value: string, width: number, label: string): CanonicalBytes {
  if (typeof value !== 'string') throw new TypeError(label + ' must be a canonical base64url identity')
  const decoded = decodeBase64Url(value)
  if (decoded === undefined || decoded.byteLength !== width ||
      decoded.every((byte) => byte === 0) || encodeBase64Url(decoded) !== value) {
    throw new TypeError(label + ' must be a non-zero canonical ' + width + '-byte identity')
  }
  return Uint8Array.from(decoded)
}
