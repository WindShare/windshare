const BASE64URL_PATTERN = /^[A-Za-z0-9_-]+$/u

export function copyBytes(bytes: Uint8Array): Uint8Array<ArrayBuffer> {
  return Uint8Array.from(bytes)
}

export function equalBytes(left: Uint8Array, right: Uint8Array): boolean {
  if (left.byteLength !== right.byteLength) {
    return false
  }
  let difference = 0
  for (let index = 0; index < left.byteLength; index += 1) {
    difference |= (left[index] ?? 0) ^ (right[index] ?? 0)
  }
  return difference === 0
}

export function encodeBase64Url(bytes: Uint8Array): string {
  let binary = ''
  for (const byte of bytes) {
    binary += String.fromCharCode(byte)
  }
  const encoded = btoa(binary).replaceAll('+', '-').replaceAll('/', '_')
  const padding = encoded.indexOf('=')
  return padding === -1 ? encoded : encoded.slice(0, padding)
}

export function decodeBase64Url(value: string): Uint8Array<ArrayBuffer> | undefined {
  if (!BASE64URL_PATTERN.test(value) || value.length % 4 === 1) {
    return undefined
  }
  const padded = value.replaceAll('-', '+').replaceAll('_', '/').padEnd(
    value.length + ((4 - (value.length % 4)) % 4),
    '=',
  )
  try {
    const binary = atob(padded)
    const decoded = new Uint8Array(binary.length)
    for (let index = 0; index < binary.length; index += 1) {
      decoded[index] = binary.charCodeAt(index)
    }
    return encodeBase64Url(decoded) === value ? decoded : undefined
  } catch {
    return undefined
  }
}

export function encodeUint32(value: number): Uint8Array<ArrayBuffer> {
  const encoded = new Uint8Array(4)
  new DataView(encoded.buffer).setUint32(0, value, false)
  return encoded
}

export function encodeUint64(value: number): Uint8Array<ArrayBuffer> {
  const encoded = new Uint8Array(8)
  new DataView(encoded.buffer).setBigUint64(0, BigInt(value), false)
  return encoded
}

export function concatBytes(parts: readonly Uint8Array[]): Uint8Array<ArrayBuffer> {
  const length = parts.reduce((total, part) => total + part.byteLength, 0)
  const joined = new Uint8Array(length)
  let offset = 0
  for (const part of parts) {
    joined.set(part, offset)
    offset += part.byteLength
  }
  return joined
}

export function bytesToHex(bytes: Uint8Array): string {
  let result = ''
  for (const byte of bytes) {
    result += byte.toString(16).padStart(2, '0')
  }
  return result
}
