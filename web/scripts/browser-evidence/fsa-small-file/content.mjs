export const CONTENT_ALGORITHM = 'windshare/ordinal-offset-mod251/v1'

const CONTENT_MODULUS = 251
const ORDINAL_MULTIPLIER = 131

export function contentBytes(ordinal, sizeBytes) {
  if (!Number.isSafeInteger(ordinal) || ordinal < 0) throw new Error('ordinal must be a non-negative safe integer')
  if (!Number.isSafeInteger(sizeBytes) || sizeBytes < 0) throw new Error('sizeBytes must be a non-negative safe integer')
  const bytes = new Uint8Array(sizeBytes)
  const ordinalOffset = Math.imul(ordinal, ORDINAL_MULTIPLIER) % CONTENT_MODULUS
  for (let offset = 0; offset < bytes.length; offset += 1) {
    bytes[offset] = (ordinalOffset + offset) % CONTENT_MODULUS
  }
  return bytes
}
