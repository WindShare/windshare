import { concatBytes, decodeBase64Url, encodeBase64Url, equalBytes } from '../../crypto/bytes'
import { createEd25519Verifier } from '../../crypto/ed25519'
import { decodeCanonicalCbor, requireArray, requireBytes } from '../../protocol/cbor'

const SIGNATURE_BYTES = 64
const MAX_CERTIFICATE_BYTES = 4096
const CERTIFICATE_SECONDS = 180 * 24 * 60 * 60
export const CLOCK_SKEW_SECONDS = 300
const encoder = new TextEncoder()

export interface AuthorityGeneration {
  readonly number: number; readonly key: Uint8Array; readonly notBefore: number; readonly expiresAt: number
  readonly certificate: string
}

export function base64(value: unknown, maximum: number): Uint8Array<ArrayBuffer> {
  if (typeof value !== 'string' || value.length > Math.ceil(maximum * 4 / 3)) throw new TypeError('Invalid authority encoding')
  const bytes = decodeBase64Url(value)
  if (!bytes || bytes.length > maximum || encodeBase64Url(bytes) !== value) throw new TypeError('Invalid authority encoding')
  return bytes
}

export function unsigned(value: unknown): number {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0) throw new TypeError('Invalid authority counter')
  return value
}

export async function verifySigned(encoded: unknown, maximum: number, domain: string, key: Uint8Array): Promise<Uint8Array> {
  const bytes = base64(encoded, maximum)
  if (bytes.length <= SIGNATURE_BYTES || key.length !== 32) throw new TypeError('Invalid authority signature')
  const payload = bytes.subarray(0, -SIGNATURE_BYTES)
  if (!await createEd25519Verifier(key).verify(concatBytes([encoder.encode(domain), payload]), bytes.subarray(-SIGNATURE_BYTES))) {
    throw new TypeError('Authority signature rejected')
  }
  return payload
}

export async function verifyGeneration(certificate: string, root: string): Promise<AuthorityGeneration> {
  const issuer = base64(root, 32)
  if (issuer.length !== 32 || issuer.every(byte => byte === 0)) throw new TypeError('Invalid authority root')
  const bytes = await verifySigned(certificate, MAX_CERTIFICATE_BYTES, 'windshare/authority-certificate/v1\0', issuer)
  const fields = requireArray(decodeCanonicalCbor(bytes, MAX_CERTIFICATE_BYTES, 'Authority certificate'), 6, 'Authority certificate')
  if (fields.length !== 6 || fields[0] !== 1 || !equalBytes(requireBytes(fields[1], 32, 'Issuer'), issuer)) throw new TypeError('Invalid authority certificate')
  const number = unsigned(fields[2]), key = requireBytes(fields[3], 32, 'Online key', true)
  const notBefore = unsigned(fields[4]), expiresAt = unsigned(fields[5])
  if (number === 0 || expiresAt <= notBefore || expiresAt - notBefore > CERTIFICATE_SECONDS) throw new TypeError('Invalid authority generation')
  return Object.freeze({ number, key, notBefore, expiresAt, certificate })
}
