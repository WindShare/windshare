const STACK_LOCATION_SUFFIX = /:\d+:\d+$/u

const ASCII_SPACE = 0x20
const ASCII_DOUBLE_QUOTE = 0x22
const ASCII_HASH = 0x23
const ASCII_SINGLE_QUOTE = 0x27
const ASCII_LEFT_PAREN = 0x28
const ASCII_RIGHT_PAREN = 0x29
const ASCII_PLUS = 0x2b
const ASCII_HYPHEN = 0x2d
const ASCII_DOT = 0x2e
const ASCII_SLASH = 0x2f
const ASCII_ZERO = 0x30
const ASCII_NINE = 0x39
const ASCII_LESS_THAN = 0x3c
const ASCII_GREATER_THAN = 0x3e
const ASCII_QUESTION = 0x3f
const ASCII_AT = 0x40
const ASCII_UPPER_A = 0x41
const ASCII_UPPER_Z = 0x5a
const ASCII_LEFT_BRACKET = 0x5b
const ASCII_RIGHT_BRACKET = 0x5d
const ASCII_UNDERSCORE = 0x5f
const ASCII_BACKTICK = 0x60
const ASCII_LOWER_A = 0x61
const ASCII_LOWER_Z = 0x7a
const ASCII_LEFT_BRACE = 0x7b
const ASCII_RIGHT_BRACE = 0x7d

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
    const tokenEnd = findUrlEnd(text, schemeStart, authorityStart)
    output += text.slice(cursor, schemeStart)
    output += sanitizeUrlToken(text.slice(schemeStart, tokenEnd), authorityStart - schemeStart)
    cursor = tokenEnd
  }
  return output + text.slice(cursor)
}

/**
 * Strips URL credentials (`userinfo@`) and any trailing query/fragment from a
 * single raw, malformed, or placeholder endpoint string without free-text
 * tokenization.
 */
export function stripRawUrlCredentials(raw: string): string {
  const cut = firstIndexOf(raw, '?', '#')
  const body = cut === -1 ? raw : raw.slice(0, cut)
  const authorityStart = findRawAuthorityStart(body)
  const pathStart = body.indexOf('/', authorityStart)
  const authorityEnd = pathStart === -1 ? body.length : pathStart
  if (authorityEnd <= authorityStart) return body
  const atIndex = body.lastIndexOf('@', authorityEnd - 1)
  if (atIndex < authorityStart) return body
  return body.slice(0, authorityStart) + body.slice(atIndex + 1)
}

function findRawAuthorityStart(body: string): number {
  let start = 0
  while (start < body.length && body.charCodeAt(start) <= ASCII_SPACE) {
    start += 1
  }
  const sep = body.indexOf('://', start)
  // Require a valid scheme before `://` so a `://` inside a path cannot hide
  // scheme-less `userinfo@` credentials earlier in the string.
  if (sep > start && isValidScheme(body, start, sep)) return sep + 3
  if (body.startsWith('//', start)) return start + 2
  return 0
}

function isValidScheme(text: string, start: number, end: number): boolean {
  if (start >= end || !isAsciiLetter(text.charCodeAt(start))) return false
  for (let index = start + 1; index < end; index += 1) {
    if (!isSchemeChar(text.charCodeAt(index))) return false
  }
  return true
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

function findUrlEnd(text: string, schemeStart: number, authorityStart: number): number {
  const closingQuote = openingQuoteCode(text, schemeStart)
  let index = authorityStart
  let inAuthority = true
  let parenDepth = 0
  while (index < text.length) {
    const code = text.charCodeAt(index)
    if (isIpv6HostStart(text, index, authorityStart, inAuthority, code)) {
      index = scanIpv6Host(text, index + 1)
      continue
    }
    if (isUrlTerminator(code, closingQuote, parenDepth)) break
    parenDepth = nextParenDepth(code, parenDepth)
    if (isAuthorityTerminator(code)) inAuthority = false
    index += 1
  }
  return index
}

function openingQuoteCode(text: string, schemeStart: number): number {
  if (schemeStart === 0) return 0
  const previous = text.charCodeAt(schemeStart - 1)
  if (
    previous === ASCII_SINGLE_QUOTE ||
    previous === ASCII_DOUBLE_QUOTE ||
    previous === ASCII_BACKTICK
  ) {
    return previous
  }
  return 0
}

function isIpv6HostStart(
  text: string,
  index: number,
  authorityStart: number,
  inAuthority: boolean,
  code: number,
): boolean {
  return inAuthority &&
    code === ASCII_LEFT_BRACKET &&
    (index === authorityStart || text.charCodeAt(index - 1) === ASCII_AT)
}

function isUrlTerminator(code: number, closingQuote: number, parenDepth: number): boolean {
  if (isHardDelimiter(code) || code === ASCII_LEFT_BRACKET || code === ASCII_RIGHT_BRACKET) {
    return true
  }
  // RFC 3986 and valid relay admission queries permit `(`, `)`, and `'`; break
  // on `'` only when paired with an opening quote, and break on unmatched `)`
  // only outside quote-delimited URLs so stack frames keep their closing `)`.
  if (code === ASCII_SINGLE_QUOTE) return closingQuote === ASCII_SINGLE_QUOTE
  if (code === ASCII_RIGHT_PAREN) return closingQuote === 0 && parenDepth === 0
  return false
}

function nextParenDepth(code: number, parenDepth: number): number {
  if (code === ASCII_LEFT_PAREN) return parenDepth + 1
  if (code === ASCII_RIGHT_PAREN && parenDepth > 0) return parenDepth - 1
  return parenDepth
}

function isAuthorityTerminator(code: number): boolean {
  return code === ASCII_SLASH || code === ASCII_QUESTION || code === ASCII_HASH
}

function scanIpv6Host(text: string, start: number): number {
  let index = start
  while (index < text.length) {
    const code = text.charCodeAt(index)
    if (isIpv6Delimiter(code)) return index
    index += 1
    if (code === ASCII_RIGHT_BRACKET) return index
  }
  return index
}

function sanitizeUrlToken(token: string, authorityOffset: number): string {
  const stripIndex = firstIndexOfFrom(token, authorityOffset, '?', '#')
  const bodyEnd = stripIndex === -1 ? token.length : stripIndex
  const pathStart = token.indexOf('/', authorityOffset)
  const authorityEnd = pathStart !== -1 && pathStart < bodyEnd ? pathStart : bodyEnd
  const atIndex = authorityEnd > authorityOffset
    ? token.lastIndexOf('@', authorityEnd - 1)
    : -1
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

function isHardDelimiter(code: number): boolean {
  return code <= ASCII_SPACE || code === ASCII_DOUBLE_QUOTE || code === ASCII_BACKTICK ||
    code === ASCII_LESS_THAN || code === ASCII_GREATER_THAN ||
    code === ASCII_LEFT_BRACE || code === ASCII_RIGHT_BRACE
}

function isIpv6Delimiter(code: number): boolean {
  return isHardDelimiter(code) || code === ASCII_SINGLE_QUOTE ||
    code === ASCII_LEFT_PAREN || code === ASCII_RIGHT_PAREN ||
    code === ASCII_LEFT_BRACKET
}

function isAsciiLetter(code: number): boolean {
  return (code >= ASCII_UPPER_A && code <= ASCII_UPPER_Z) ||
    (code >= ASCII_LOWER_A && code <= ASCII_LOWER_Z)
}

function isSchemeChar(code: number): boolean {
  return isAsciiLetter(code) || (code >= ASCII_ZERO && code <= ASCII_NINE) ||
    code === ASCII_PLUS || code === ASCII_HYPHEN || code === ASCII_DOT
}

function isWordChar(code: number): boolean {
  return isAsciiLetter(code) || (code >= ASCII_ZERO && code <= ASCII_NINE) ||
    code === ASCII_UNDERSCORE
}
