import { foldPortableName15, PINNED_CASE_FOLD_VERSION } from '../unicode/case-fold'
import {
  normalizeNFC15,
  PINNED_NORMALIZATION_VERSION,
} from '../unicode/unicode-normalization'

export const V2_PATH_POLICY = 'windshare/path/v1-unicode-15.0.0'
export const V2_PATH_POLICY_UNICODE_VERSION = PINNED_CASE_FOLD_VERSION
export const V2_CATALOG_NAME_BYTES = 255
export const V2_CATALOG_PATH_BYTES = 32 * 1024
export const V2_CATALOG_PATH_DEPTH = 256

const TEXT_ENCODER = new TextEncoder()
const RESERVED_OUTPUT_PREFIX = '.wsresume'
const WINDOWS_ILLEGAL_CHARACTERS = new Set('<>:"|?*\\')
const FORMAT_CHARACTER_RANGES: readonly (readonly [number, number])[] = [
  [0x00ad, 0x00ad],
  [0x0600, 0x0605],
  [0x061c, 0x061c],
  [0x06dd, 0x06dd],
  [0x070f, 0x070f],
  [0x0890, 0x0891],
  [0x08e2, 0x08e2],
  [0x180e, 0x180e],
  [0x200b, 0x200f],
  [0x202a, 0x202e],
  [0x2060, 0x2064],
  [0x2066, 0x206f],
  [0xfeff, 0xfeff],
  [0xfff9, 0xfffb],
  [0x110bd, 0x110bd],
  [0x110cd, 0x110cd],
  [0x13430, 0x1343f],
  [0x1bca0, 0x1bca3],
  [0x1d173, 0x1d17a],
  [0xe0001, 0xe0001],
  [0xe0020, 0xe007f],
]
const WINDOWS_RESERVED_NAMES = (() => {
  const names = new Set(['con', 'prn', 'aux', 'nul', 'conin$', 'conout$'])
  for (let suffix = 1; suffix <= 9; suffix += 1) {
    names.add(`com${suffix}`)
    names.add(`lpt${suffix}`)
  }
  for (const suffix of ['¹', '²', '³']) {
    names.add(`com${suffix}`)
    names.add(`lpt${suffix}`)
  }
  return names
})()

export function isPortableCatalogName(name: string): boolean {
  if (
    name.length === 0 ||
    name === '.' ||
    name === '..' ||
    !isWellFormedUnicode(name) ||
    normalizeNFC15(name) !== name ||
    name.includes('/') ||
    name.includes('\\') ||
    name.endsWith('.') ||
    name.endsWith(' ') ||
    TEXT_ENCODER.encode(name).byteLength > V2_CATALOG_NAME_BYTES ||
    isWindowsReservedName(name)
  ) return false
  for (const character of name) {
    const scalar = character.codePointAt(0) ?? 0
    if (isControlOrFormatCharacter(scalar) || WINDOWS_ILLEGAL_CHARACTERS.has(character)) return false
  }
  return true
}

export function catalogNameCollisionKey(name: string): string {
  if (!isPortableCatalogName(name)) throw new TypeError('Catalog name violates the frozen path policy')
  return foldPortableName15(name)
}

/** Canonicalizes a locally composed catalog path; wire entries remain single segments. */
export function canonicalizePortableCatalogPath(path: string): string {
  if (!isWellFormedUnicode(path)) throw new TypeError('Catalog path contains invalid Unicode')
  const canonical = normalizeNFC15(path)
  const segments = canonical.split('/')
  snapshotPortableCatalogPath(segments)
  return segments.join('/')
}

/** Validates an already segmented path without hiding traversal ownership in a joined string. */
export function snapshotPortableCatalogPath(path: readonly string[]): readonly string[] {
  if (path.length === 0 || path.length > V2_CATALOG_PATH_DEPTH) {
    throw new TypeError('Catalog path violates the frozen path policy')
  }
  let pathBytes = path.length - 1
  for (const segment of path) {
    if (!isPortableCatalogName(segment)) {
      throw new TypeError('Catalog path violates the frozen path policy')
    }
    pathBytes += TEXT_ENCODER.encode(segment).byteLength
  }
  if (
    pathBytes > V2_CATALOG_PATH_BYTES ||
    foldPortableName15(path[0] ?? '').startsWith(RESERVED_OUTPUT_PREFIX)
  ) throw new TypeError('Catalog path violates the frozen path policy')
  return Object.freeze([...path])
}

export function catalogPathCollisionKey(path: string): string {
  const canonical = canonicalizePortableCatalogPath(path)
  return foldPortableName15(canonical)
}

function isWellFormedUnicode(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const unit = value.charCodeAt(index)
    if (unit >= 0xd800 && unit <= 0xdbff) {
      const next = value.charCodeAt(index + 1)
      if (!(next >= 0xdc00 && next <= 0xdfff)) return false
      index += 1
    } else if (unit >= 0xdc00 && unit <= 0xdfff) return false
  }
  return true
}

function isFormatCharacter(scalar: number): boolean {
  let low = 0
  let high = FORMAT_CHARACTER_RANGES.length
  while (low < high) {
    const middle = low + Math.floor((high - low) / 2)
    const range = FORMAT_CHARACTER_RANGES[middle]
    if (range === undefined) return false
    if (scalar < range[0]) high = middle
    else if (scalar > range[1]) low = middle + 1
    else return true
  }
  return false
}

function isControlOrFormatCharacter(scalar: number): boolean {
  return scalar <= 0x1f || (scalar >= 0x7f && scalar <= 0x9f) || isFormatCharacter(scalar)
}

function isWindowsReservedName(segment: string): boolean {
  const stem = segment.split('.', 1)[0] ?? ''
  return WINDOWS_RESERVED_NAMES.has(asciiLower(stem))
}

function asciiLower(value: string): string {
  let lowered = ''
  for (const character of value) {
    const code = character.codePointAt(0) ?? 0
    lowered += code >= 0x41 && code <= 0x5a ? String.fromCodePoint(code + 0x20) : character
  }
  return lowered
}

if (
  V2_PATH_POLICY !== `windshare/path/v1-unicode-${PINNED_CASE_FOLD_VERSION}` ||
  PINNED_NORMALIZATION_VERSION !== PINNED_CASE_FOLD_VERSION
) throw new Error('Catalog path policy does not match its pinned Unicode tables')
