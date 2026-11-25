const STACK_LOCATION_SUFFIX = /:\d+:\d+$/u

/**
 * Strips URL credentials (`userinfo@`), queries (`?…`), and fragments (`#…`)
 * from arbitrary diagnostic text while preserving host/path visibility and
 * stack-frame `:line:col` locations.
 */
export function sanitizeDiagnosticText(text: string): string {
  if (!text.includes('://') || (!text.includes('?') && !text.includes('#') && !text.includes('@'))) {
    return text
  }
  let output = ''
  let cursor = 0
  while (cursor < text.length) {
    const sep = text.indexOf('://', cursor)
    if (sep === -1) break
    const schemeStart = findSchemeStart(text, sep)
    if (schemeStart === -1) {
      output += text.slice(cursor, sep + 3)
      cursor = sep + 3
      continue
    }
    const authorityStart = sep + 3
    const tokenEnd = findUrlEnd(text, authorityStart)
    output += text.slice(cursor, schemeStart)
    output += sanitizeUrlToken(text.slice(schemeStart, tokenEnd), authorityStart - schemeStart)
    cursor = tokenEnd
  }
  return output + text.slice(cursor)
}

/** Strips URL credentials and any trailing query/fragment from raw or placeholder relay strings. */
export function stripRawUrlCredentials(raw: string): string {
  const sanitized = sanitizeDiagnosticText(raw)
  const cut = firstIndexOf(sanitized, '?', '#')
  return cut === -1 ? sanitized : sanitized.slice(0, cut)
}

function findSchemeStart(text: string, sep: number): number {
  let index = sep
  while (index > 0 && isSchemeChar(text.charCodeAt(index - 1))) {
    index -= 1
  }
  if (index === sep || !isAsciiLetter(text.charCodeAt(index))) return -1
  if (index > 0 && isWordChar(text.charCodeAt(index - 1))) return -1
  return index
}

function findUrlEnd(text: string, authorityStart: number): number {
  let index = authorityStart
  let inAuthority = true
  while (index < text.length) {
    const code = text.charCodeAt(index)
    if (inAuthority && code === 0x5b && (index === authorityStart || text.charCodeAt(index - 1) === 0x40)) {
      index = scanIpv6Host(text, index + 1)
      continue
    }
    if (isDelimiter(code) || code === 0x5b || code === 0x5d) break
    if (code === 0x2f || code === 0x3f || code === 0x23) inAuthority = false
    index += 1
  }
  return index
}

function scanIpv6Host(text: string, start: number): number {
  let index = start
  while (index < text.length) {
    const code = text.charCodeAt(index)
    if (isDelimiter(code) || code === 0x5b) return index
    index += 1
    if (code === 0x5d) return index
  }
  return index
}

function sanitizeUrlToken(token: string, authorityOffset: number): string {
  const stripIndex = firstIndexOfFrom(token, authorityOffset, '?', '#')
  const bodyEnd = stripIndex === -1 ? token.length : stripIndex
  const pathStart = token.indexOf('/', authorityOffset)
  const authorityEnd = pathStart !== -1 && pathStart < bodyEnd ? pathStart : bodyEnd
  const atIndex = token.lastIndexOf('@', authorityEnd - 1)
  const prefix = atIndex >= authorityOffset
    ? token.slice(0, authorityOffset) + token.slice(atIndex + 1, bodyEnd)
    : token.slice(0, bodyEnd)
  if (stripIndex === -1) return prefix
  const stripped = token.slice(stripIndex)
  if (stripped.startsWith('?') && !stripped.includes('#')) {
    const location = STACK_LOCATION_SUFFIX.exec(stripped)
    if (location !== null) return prefix + location[0]
  }
  return prefix
}

function firstIndexOf(text: string, first: string, second: string): number {
  return firstIndexOfFrom(text, 0, first, second)
}

function firstIndexOfFrom(text: string, start: number, first: string, second: string): number {
  const a = text.indexOf(first, start)
  const b = text.indexOf(second, start)
  if (a === -1) return b
  if (b === -1) return a
  return Math.min(a, b)
}

function isDelimiter(code: number): boolean {
  return code <= 0x20 || code === 0x22 || code === 0x27 || code === 0x60 ||
    code === 0x3c || code === 0x3e || code === 0x28 || code === 0x29 ||
    code === 0x7b || code === 0x7d
}

function isAsciiLetter(code: number): boolean {
  return (code >= 0x41 && code <= 0x5a) || (code >= 0x61 && code <= 0x7a)
}

function isSchemeChar(code: number): boolean {
  return isAsciiLetter(code) || (code >= 0x30 && code <= 0x39) ||
    code === 0x2b || code === 0x2d || code === 0x2e
}

function isWordChar(code: number): boolean {
  return isAsciiLetter(code) || (code >= 0x30 && code <= 0x39) || code === 0x5f
}
