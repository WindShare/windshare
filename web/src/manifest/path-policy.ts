import { PATH_POLICY_VERSION, type CanonicalPath } from '../contracts'
import { ManifestError } from './errors'
import {
  UNICODE_CASE_FOLD_DATA,
  UNICODE_CASE_FOLD_VERSION,
} from './unicode-case-fold-data'
import {
  normalizeNFC15,
  PINNED_NORMALIZATION_VERSION,
} from './unicode-normalization'

export const PATH_POLICY_UNICODE_VERSION = UNICODE_CASE_FOLD_VERSION

const WINDOWS_ILLEGAL_CHARACTERS = new Set('<>:"|?*\\')
const RESUME_JOURNAL_PREFIX = '.wsresume'
const MAX_DIAGNOSTIC_BYTES = 256
const encoder = new TextEncoder()

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

let caseFoldMappings: ReadonlyMap<number, string> | undefined

function foldMappings(): ReadonlyMap<number, string> {
  if (caseFoldMappings !== undefined) {
    return caseFoldMappings
  }
  const mappings = new Map<number, string>()
  for (const record of UNICODE_CASE_FOLD_DATA.trim().split(/\s+/u)) {
    const separator = record.indexOf('=')
    const source = Number.parseInt(record.slice(0, separator), 16)
    const replacement = record
      .slice(separator + 1)
      .split('.')
      .map((scalar) => String.fromCodePoint(Number.parseInt(scalar, 16)))
      .join('')
    mappings.set(source, replacement)
  }
  caseFoldMappings = mappings
  return mappings
}

function isWellFormedUnicode(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const unit = value.charCodeAt(index)
    if (unit >= 0xd800 && unit <= 0xdbff) {
      const next = value.charCodeAt(index + 1)
      if (!(next >= 0xdc00 && next <= 0xdfff)) {
        return false
      }
      index += 1
    } else if (unit >= 0xdc00 && unit <= 0xdfff) {
      return false
    }
  }
  return true
}

function isFormatCharacter(scalar: number): boolean {
  let low = 0
  let high = FORMAT_CHARACTER_RANGES.length
  while (low < high) {
    const middle = low + Math.floor((high - low) / 2)
    const range = FORMAT_CHARACTER_RANGES[middle]
    if (range === undefined) {
      return false
    }
    if (scalar < range[0]) {
      high = middle
    } else if (scalar > range[1]) {
      low = middle + 1
    } else {
      return true
    }
  }
  return false
}

function isControlOrFormatCharacter(scalar: number): boolean {
  return scalar <= 0x1f || (scalar >= 0x7f && scalar <= 0x9f) || isFormatCharacter(scalar)
}

function asciiLower(value: string): string {
  let lowered = ''
  for (const character of value) {
    const code = character.codePointAt(0) ?? 0
    lowered += code >= 0x41 && code <= 0x5a ? String.fromCodePoint(code + 0x20) : character
  }
  return lowered
}

function isWindowsReservedName(segment: string): boolean {
  const dot = segment.indexOf('.')
  const candidate = dot === -1 ? segment : segment.slice(0, dot)
  let end = candidate.length
  while (end > 0 && candidate[end - 1] === ' ') {
    end -= 1
  }
  return WINDOWS_RESERVED_NAMES.has(asciiLower(candidate.slice(0, end)))
}

function avoidSplitSurrogate(path: string, end: number): number {
  if (end <= 0 || end >= path.length) {
    return end
  }
  const last = path.charCodeAt(end - 1)
  return last >= 0xd800 && last <= 0xdbff ? end - 1 : end
}

function diagnosticJsonString(value: string): string {
  // JSON permits literal Unicode line/paragraph separators. Escaping them keeps a
  // valid path from injecting extra diagnostic lines and mirrors Go's quoted form.
  return JSON.stringify(value)
    .replaceAll('\u2028', '\\u2028')
    .replaceAll('\u2029', '\\u2029')
}

function quotedPathCandidate(path: string, end: number, truncated: boolean): string {
  return diagnosticJsonString(`${path.slice(0, end)}${truncated ? '…' : ''}`)
}

export function quotePathForDiagnostic(path: string): string {
  let end = avoidSplitSurrogate(path, Math.min(path.length, MAX_DIAGNOSTIC_BYTES))
  let truncated = end < path.length
  while (true) {
    const quoted = quotedPathCandidate(path, end, truncated)
    if (encoder.encode(quoted).byteLength <= MAX_DIAGNOSTIC_BYTES) {
      return quoted
    }
    truncated = true
    if (end === 0) {
      return diagnosticJsonString('…')
    }
    end = avoidSplitSurrogate(path, end - 1)
  }
}

export function fullCaseFold15(value: string): string {
  const mappings = foldMappings()
  let output = ''
  let unchangedStart = 0
  for (let offset = 0; offset < value.length; ) {
    const scalar = value.codePointAt(offset)
    if (scalar === undefined) {
      break
    }
    const width = scalar > 0xffff ? 2 : 1
    const replacement = mappings.get(scalar)
    if (replacement !== undefined) {
      output += value.slice(unchangedStart, offset)
      output += replacement
      unchangedStart = offset + width
    }
    offset += width
  }
  return output + value.slice(unchangedStart)
}

export function foldPathUnchecked(path: string): string {
  return normalizeNFC15(fullCaseFold15(path))
}

function validateSegment(segment: string, path: string): void {
  if (segment === '') {
    throw new ManifestError(
      'invalid-path',
      `Path ${quotePathForDiagnostic(path)} contains an empty segment`,
    )
  }
  if (segment === '.' || segment === '..') {
    throw new ManifestError(
      'invalid-path',
      `Path ${quotePathForDiagnostic(path)} contains a relative segment`,
    )
  }
  for (const character of segment) {
    const scalar = character.codePointAt(0) ?? 0
    if (isControlOrFormatCharacter(scalar)) {
      throw new ManifestError(
        'invalid-path',
        `Path ${quotePathForDiagnostic(path)} contains a control or format character`,
      )
    }
    if (WINDOWS_ILLEGAL_CHARACTERS.has(character)) {
      throw new ManifestError(
        'invalid-path',
        `Path ${quotePathForDiagnostic(path)} contains a cross-platform illegal character`,
      )
    }
  }
  if (segment.endsWith(' ') || segment.endsWith('.')) {
    throw new ManifestError(
      'invalid-path',
      `Path ${quotePathForDiagnostic(path)} contains a segment ending in a space or dot`,
    )
  }
  if (isWindowsReservedName(segment)) {
    throw new ManifestError(
      'invalid-path',
      `Path ${quotePathForDiagnostic(path)} contains a reserved Windows device name`,
    )
  }
}

export function validateCanonicalPath(path: string): CanonicalPath {
  if (path === '') {
    throw new ManifestError('invalid-path', 'Manifest path must not be empty')
  }
  if (!isWellFormedUnicode(path)) {
    throw new ManifestError('invalid-path', 'Manifest path contains invalid Unicode')
  }
  if (normalizeNFC15(path) !== path) {
    throw new ManifestError(
      'invalid-path',
      `Manifest path ${quotePathForDiagnostic(path)} is not NFC-normalized`,
    )
  }

  let firstSegment = ''
  for (let start = 0; ; ) {
    const slash = path.indexOf('/', start)
    const end = slash === -1 ? path.length : slash
    const segment = path.slice(start, end)
    validateSegment(segment, path)
    if (start === 0) {
      firstSegment = segment
    }
    if (slash === -1) {
      break
    }
    start = end + 1
  }

  if (foldPathUnchecked(firstSegment).startsWith(RESUME_JOURNAL_PREFIX)) {
    throw new ManifestError(
      'invalid-path',
      `Manifest path ${quotePathForDiagnostic(path)} uses a reserved tool prefix`,
    )
  }
  return path as CanonicalPath
}

export function canonicalizePath(path: string): CanonicalPath {
  if (!isWellFormedUnicode(path)) {
    throw new ManifestError('invalid-path', 'Manifest path contains invalid Unicode')
  }
  return validateCanonicalPath(normalizeNFC15(path))
}

export function pathCollisionKey(path: string): string {
  validateCanonicalPath(path)
  return foldPathUnchecked(path)
}

function compareExhaustedPaths(
  leftOffset: number,
  leftLength: number,
  rightOffset: number,
  rightLength: number,
): number | undefined {
  const leftEnded = leftOffset === leftLength
  const rightEnded = rightOffset === rightLength
  if (!leftEnded && !rightEnded) {
    return undefined
  }
  if (leftEnded && rightEnded) {
    return 0
  }
  return leftEnded ? -1 : 1
}

function scalarWidth(scalar: number): number {
  return scalar > 0xffff ? 2 : 1
}

export function compareCanonicalPaths(left: string, right: string): number {
  for (let leftOffset = 0, rightOffset = 0; ; ) {
    const exhausted = compareExhaustedPaths(
      leftOffset,
      left.length,
      rightOffset,
      right.length,
    )
    if (exhausted !== undefined) {
      return exhausted
    }
    const leftScalar = left.codePointAt(leftOffset) ?? 0
    const rightScalar = right.codePointAt(rightOffset) ?? 0
    if (leftScalar !== rightScalar) {
      return leftScalar < rightScalar ? -1 : 1
    }
    leftOffset += scalarWidth(leftScalar)
    rightOffset += scalarWidth(rightScalar)
  }
}

if (PATH_POLICY_VERSION !== `windshare/path/v1-unicode-${PATH_POLICY_UNICODE_VERSION}`) {
  throw new Error('Path policy version does not match the pinned Unicode case-fold table')
}

if (PINNED_NORMALIZATION_VERSION !== PATH_POLICY_UNICODE_VERSION) {
  throw new Error('Path policy version does not match the pinned Unicode normalization table')
}
