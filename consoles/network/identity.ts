export const KEY_BYTES = 32
export const ACCOUNT_BYTES = 16
export const SHARE_BYTES = 12

export function encode(bytes: ArrayLike<number>): string {
  return btoa(String.fromCharCode(...Array.from(bytes))).replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', '')
}

export function decode(text: string, length: number, label: string): number[] {
  const value = text.trim()
  if (/^[A-Za-z0-9_-]+$/.test(value) && value.length === Math.ceil(length * 4 / 3)) {
    const bytes = Array.from(atob(value.replaceAll('-', '+').replaceAll('_', '/')), char => char.charCodeAt(0))
    if (bytes.length === length && bytes.some(byte => byte !== 0) && encode(bytes) === value) return bytes
  }
  throw new Error('Enter a complete, valid ' + label + '.')
}
