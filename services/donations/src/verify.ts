import { createHash, createHmac, timingSafeEqual } from 'node:crypto'

export function sha256Hex(value: string): string {
  return createHash('sha256').update(value, 'utf8').digest('hex')
}

export function constantTimeEqual(left: string, right: string): boolean {
  // Hashing both inputs normalizes byte length so timingSafeEqual never leaks secret length.
  const leftDigest = createHash('sha256').update(left, 'utf8').digest()
  const rightDigest = createHash('sha256').update(right, 'utf8').digest()
  return timingSafeEqual(leftDigest, rightDigest)
}

export function md5Hex(value: string): string {
  // Afdian Open API requires lowercase MD5 signatures for query-order / query-sponsor.
  // eslint-disable-next-line sonarjs/hashing
  return createHash('md5').update(value, 'utf8').digest('hex')
}

export function hmacSha512Hex(secret: string, payload: string): string {
  return createHmac('sha512', secret).update(payload, 'utf8').digest('hex')
}
