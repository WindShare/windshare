const WINDOWS_DEVICE_SEGMENT = /^(?:aux|clock\$|com(?:[1-9¹²³])|con|conin\$|conout\$|lpt(?:[1-9¹²³])|nul|prn)(?:\..*)?$/iu
const WINDOWS_FORBIDDEN_CHARACTER = /[<>"|?*]/u
export const PORTABLE_PATH_MAXIMUM_BYTES = 4_096
export const PORTABLE_PATH_MAXIMUM_SEGMENT_BYTES = 255
export const PORTABLE_PATH_MAXIMUM_DEPTH = 64

export function requirePortableRelativePath(
  value: unknown,
  label: string,
  maximumBytes = PORTABLE_PATH_MAXIMUM_BYTES,
): string {
  if (typeof value !== 'string' || value.length === 0 || !hasOnlyUnicodeScalars(value)) {
    throw new Error(`${label} must be non-empty Unicode scalar text`)
  }
  if (value !== value.normalize('NFC')) throw new Error(`${label} must use canonical Unicode NFC`)
  const segments = value.split('/')
  if (
    Buffer.byteLength(value, 'utf8') > maximumBytes || segments.length > PORTABLE_PATH_MAXIMUM_DEPTH ||
    value.includes('\\') || value.includes(':') || value.startsWith('/') ||
    containsControlCharacter(value) || WINDOWS_FORBIDDEN_CHARACTER.test(value) ||
    segments.some((segment) =>
      segment === '' || segment === '.' || segment === '..' ||
      Buffer.byteLength(segment, 'utf8') > PORTABLE_PATH_MAXIMUM_SEGMENT_BYTES ||
      segment.endsWith('.') || segment.endsWith(' ') || WINDOWS_DEVICE_SEGMENT.test(segment) ||
      containsNonAsciiCasedScalar(segment))
  ) throw new Error(`${label} must be a portable normalized relative POSIX path`)
  return value
}

export function portablePathCollisionKey(path: string): string {
  // Cased non-ASCII scalars are rejected above, making this frozen ASCII fold
  // independent of JS/Go Unicode-table versions while preserving caseless NFC.
  return path.replace(/[A-Z]/gu, (character) => character.toLowerCase())
}

export function comparePortablePaths(left: string, right: string): number {
  return Buffer.compare(Buffer.from(left, 'utf8'), Buffer.from(right, 'utf8'))
}

function containsControlCharacter(value: string): boolean {
  return [...value].some((scalar) => {
    const codePoint = scalar.codePointAt(0) ?? 0
    return codePoint <= 0x1f || codePoint === 0x7f
  })
}

function containsNonAsciiCasedScalar(value: string): boolean {
  return [...value].some((scalar) =>
    (scalar.codePointAt(0) ?? 0) > 0x7f && scalar.toUpperCase() !== scalar.toLowerCase())
}

function hasOnlyUnicodeScalars(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const current = value.charCodeAt(index)
    if (current >= 0xd800 && current <= 0xdbff) {
      const following = value.charCodeAt(index + 1)
      if (following < 0xdc00 || following > 0xdfff) return false
      index += 1
    } else if (current >= 0xdc00 && current <= 0xdfff) {
      return false
    }
  }
  return true
}
