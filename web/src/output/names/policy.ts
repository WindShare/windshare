import { isCatalogName } from '../../catalog/path-policy'

const WINDOWS_ILLEGAL_CHARACTERS = new Set('<>:"|?*')
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

/** Generated fallback names must remain usable through conventional filesystem APIs. */
export function isPortableOutputName(name: string): boolean {
  if (!isCatalogName(name) || name.endsWith('.') || name.endsWith(' ') || isWindowsReservedName(name)) return false
  for (const character of name) {
    if (WINDOWS_ILLEGAL_CHARACTERS.has(character)) return false
  }
  return true
}
function isWindowsReservedName(segment: string): boolean {
  // Conservative Win32 compatibility belongs to generated disk names. Some
  // Windows versions accept extensions; NBSP must never be treated as U+0020.
  const stem = trimTrailingSpaces(segment.split('.', 1)[0] ?? '')
  return WINDOWS_RESERVED_NAMES.has(asciiLower(stem))
}

// A loop rather than `trimEnd()` (which also strips NBSP and other Unicode
// spaces) or `/ +$/` (rejected by sonarjs/super-linear-regex).
function trimTrailingSpaces(value: string): string {
  let end = value.length
  while (end > 0 && value.charCodeAt(end - 1) === 0x20 /* U+0020 SPACE */) end -= 1
  return value.slice(0, end)
}

function asciiLower(value: string): string {
  let lowered = ''
  for (const character of value) {
    const code = character.codePointAt(0) ?? 0
    lowered += code >= 0x41 && code <= 0x5a ? String.fromCodePoint(code + 0x20) : character
  }
  return lowered
}
