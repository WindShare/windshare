import {
  RELAY_PROTOCOL_VERSION,
  validateShareId,
} from './protocol'

const ABSOLUTE_AUTHORITY_URL_PATTERN = /^[A-Za-z][A-Za-z0-9+.-]*:\/\/[^/?#]+/u
const INVALID_PERCENT_ENCODING_PATTERN = /%(?![0-9A-Fa-f]{2})/u
const CANONICAL_IPV4_PATTERN = /^(?:0|[1-9]\d{0,2})(?:\.(?:0|[1-9]\d{0,2})){3}$/u
const NUMERIC_HOST_PATTERN = /^[\d.]+$/u
const RAW_QUERY_PATTERN = /^[A-Za-z\d._~!$&()*+,;=:@/?%-]*$/u
const RAW_USERINFO_PATTERN = /^[A-Za-z\d._~:@-]*$/u
const RAW_PATH_ASCII_PATTERN = /^[A-Za-z\d._~$&+,/:;=@%-]$/u
const DNS_LABEL_PATTERN = /^[A-Za-z\d-]+$/u
const RELAY_URL_BOUNDARY_WHITESPACE = new Set([
  0x0009, 0x000a, 0x000b, 0x000c, 0x000d, 0x0020, 0x0085, 0x00a0, 0x1680,
  0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006, 0x2007, 0x2008,
  0x2009, 0x200a, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff,
])
const MAX_DNS_LABEL_BYTES = 63
const MAX_DNS_NAME_BYTES = 253
const UTF8_ENCODER = new TextEncoder()
const UTF8_DECODER = new TextDecoder()

function containsControlCharacter(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const unit = value.charCodeAt(index)
    if (unit <= 0x1f || unit === 0x7f) {
      return true
    }
  }
  return false
}

function hasRelayURLBoundaryWhitespace(value: string): boolean {
  return RELAY_URL_BOUNDARY_WHITESPACE.has(value.charCodeAt(0)) ||
    RELAY_URL_BOUNDARY_WHITESPACE.has(value.charCodeAt(value.length - 1))
}

function encodedPath(value: string, authorityEnd: number): string {
  const suffix = value.slice(authorityEnd)
  const query = suffix.search(/[?#]/u)
  return query === -1 ? suffix : suffix.slice(0, query)
}

function hasUnsafeRawPath(value: string, authorityEnd: number): boolean {
  for (const character of encodedPath(value, authorityEnd)) {
    if (character.charCodeAt(0) <= 0x7f && !RAW_PATH_ASCII_PATTERN.test(character)) {
      return true
    }
  }
  return false
}

function hasDotPathSegment(value: string, authorityEnd: number): boolean {
  return encodedPath(value, authorityEnd).split('/').some((segment) => {
    try {
      const decoded = decodeURIComponent(segment)
      return decoded === '.' || decoded === '..'
    } catch {
      return true
    }
  })
}

function rawHostname(authority: string): string {
  const credentials = authority.lastIndexOf('@')
  const hostPort = credentials === -1 ? authority : authority.slice(credentials + 1)
  if (hostPort.startsWith('[')) {
    const bracket = hostPort.indexOf(']')
    return bracket === -1 ? '' : hostPort.slice(0, bracket + 1)
  }
  const port = hostPort.lastIndexOf(':')
  return port === -1 ? hostPort : hostPort.slice(0, port)
}

function hasNonCanonicalNumericHost(authority: string): boolean {
  const hostname = rawHostname(authority).toLowerCase()
  if (hostname.startsWith('[')) {
    return false
  }
  if (hostname.startsWith('0x') || hostname.includes('.0x')) {
    return true
  }
  if (!NUMERIC_HOST_PATTERN.test(hostname)) {
    return false
  }
  if (!CANONICAL_IPV4_PATTERN.test(hostname)) {
    return true
  }
  return hostname.split('.').some((part) => Number(part) > 255)
}

function hasInvalidRelayDomain(hostname: string): boolean {
  if (hostname.startsWith('[') || CANONICAL_IPV4_PATTERN.test(hostname)) {
    return false
  }
  const name = hostname.endsWith('.') ? hostname.slice(0, -1) : hostname
  if (name.length === 0 || name.length > MAX_DNS_NAME_BYTES) {
    return true
  }
  return name.split('.').some((label) => {
    const lower = label.toLowerCase()
    return label.length === 0 ||
      label.length > MAX_DNS_LABEL_BYTES ||
      !DNS_LABEL_PATTERN.test(label) ||
      label.startsWith('-') ||
      label.endsWith('-') ||
      lower.startsWith('xn--') ||
      (label.length > 3 && label.slice(2, 4) === '--')
  })
}

function hasNonASCIICharacter(value: string): boolean {
  return [...value].some((character) => character.charCodeAt(0) > 0x7f)
}

// net/url rejects raw userinfo outside RFC 3986 and loses the original spelling
// of percent escapes. WHATWG repairs or preserves those inputs, so reject them
// before either parser can derive a different credential-bearing endpoint.
function hasUnsafeRawUserinfo(authority: string): boolean {
  const separator = authority.lastIndexOf('@')
  if (separator === -1) {
    return false
  }
  return !RAW_USERINFO_PATTERN.test(authority.slice(0, separator))
}

// Go emits RawQuery verbatim, so browser-only percent-encoding would select a
// different credential spelling. Callers can provide the percent-encoded form.
function hasUnsafeRawQuery(value: string): boolean {
  const question = value.indexOf('?')
  if (question === -1) {
    return false
  }
  const hash = value.indexOf('#', question + 1)
  const query = value.slice(question + 1, hash === -1 ? undefined : hash)
  return !RAW_QUERY_PATTERN.test(query)
}

/** Builds the receiver endpoint without accepting WHATWG URL auto-repairs. */
export function relayWebSocketUrl(relayUrl: string, shareId: string): string {
  validateShareId(shareId)
  const authority = ABSOLUTE_AUTHORITY_URL_PATTERN.exec(relayUrl)
  const authorityText = authority === null
    ? undefined
    : authority[0].slice(authority[0].indexOf('//') + 2)
  if (
    hasRelayURLBoundaryWhitespace(relayUrl) ||
    authority === null ||
    relayUrl.includes('\\') ||
    INVALID_PERCENT_ENCODING_PATTERN.test(relayUrl) ||
    containsControlCharacter(relayUrl) ||
    UTF8_DECODER.decode(UTF8_ENCODER.encode(relayUrl)) !== relayUrl ||
    hasUnsafeRawPath(relayUrl, authority[0].length) ||
    hasDotPathSegment(relayUrl, authority[0].length) ||
    authorityText === undefined ||
    hasUnsafeRawUserinfo(authorityText) ||
    rawHostname(authorityText).includes('%') ||
    hasNonASCIICharacter(rawHostname(authorityText)) ||
    hasInvalidRelayDomain(rawHostname(authorityText)) ||
    hasUnsafeRawQuery(relayUrl) ||
    hasNonCanonicalNumericHost(authorityText)
  ) {
    throw new TypeError('invalid relay URL')
  }
  let url: URL
  try {
    url = new URL(relayUrl)
  } catch {
    // WHATWG errors may echo credentials or query tokens from the relay hint.
    throw new TypeError('invalid relay URL')
  }
  if (url.host === '') {
    throw new TypeError('relay URL must include a host')
  }
  if (
    url.hostname.startsWith('[::ffff:') ||
    (CANONICAL_IPV4_PATTERN.test(url.hostname) && rawHostname(authorityText) !== url.hostname)
  ) {
    throw new TypeError('invalid relay URL')
  }
  if (url.protocol === 'http:') {
    url.protocol = 'ws:'
  } else if (url.protocol === 'https:') {
    url.protocol = 'wss:'
  } else if (url.protocol !== 'ws:' && url.protocol !== 'wss:') {
    throw new TypeError('relay URL must use http, https, ws, or wss')
  }
  const basePath = url.pathname.endsWith('/') ? url.pathname.slice(0, -1) : url.pathname
  url.pathname = `${basePath}/${RELAY_PROTOCOL_VERSION}/ws/${shareId}`
  url.hash = ''
  return url.toString()
}
