import { concatBytes } from '../../crypto/bytes'
import { sha256 } from '../../crypto/digest'
import { type CryptoRuntime, defaultCryptoRuntime } from '../../crypto/webcrypto'

export const V2_RELAY_WEBSOCKET_PATH = '/v2/ws'

const TEXT_ENCODER = new TextEncoder()
const RELAY_IDENTITY_DOMAIN = TEXT_ENCODER.encode('windshare/v2 relay-identity\0')
const INVALID_PERCENT_ENCODING = /%(?![0-9A-Fa-f]{2})/u
const QUERY_CHARACTER = /^[A-Za-z0-9\-._~!$&()*+,;=:@/?]$/u

export interface V2RelayEndpoint {
  readonly dialEndpoint: string
  readonly relayIdentityEndpoint: string
  readonly relayIdentity: Uint8Array<ArrayBuffer>
}

function boundaryWhitespace(character: string | undefined): boolean {
  if (character === undefined) return false
  const codePoint = character.codePointAt(0) ?? 0
  return (
    (codePoint >= 0x09 && codePoint <= 0x0d) ||
    codePoint === 0x20 ||
    codePoint === 0x85 ||
    codePoint === 0xa0 ||
    codePoint === 0x1680 ||
    (codePoint >= 0x2000 && codePoint <= 0x200a) ||
    codePoint === 0x2028 ||
    codePoint === 0x2029 ||
    codePoint === 0x202f ||
    codePoint === 0x205f ||
    codePoint === 0x3000 ||
    codePoint === 0xfeff
  )
}

function validDNSName(hostname: string): boolean {
  const name = hostname.endsWith('.') ? hostname.slice(0, -1) : hostname
  if (name.length === 0 || name.length > 253) return false
  for (const label of name.split('.')) {
    if (
      label.length === 0 ||
      label.length > 63 ||
      label.startsWith('-') ||
      label.endsWith('-') ||
      label.startsWith('xn--') ||
      (label.length > 3 && label.slice(2, 4) === '--') ||
      !/^[a-z0-9-]+$/u.test(label)
    ) {
      return false
    }
  }
  return true
}

function validateQuery(query: string): void {
  for (let index = 0; index < query.length; index += 1) {
    const character = query[index]!
    if (character.charCodeAt(0) >= 0x80) {
      throw new TypeError('v2 relay query must be ASCII')
    }
    if (character === '%') {
      if (
        index + 2 >= query.length ||
        !/^[0-9A-Fa-f]{2}$/u.test(query.slice(index + 1, index + 3))
      ) {
        throw new TypeError('v2 relay query contains an invalid percent escape')
      }
      index += 2
    } else if (!QUERY_CHARACTER.test(character)) {
      throw new TypeError('v2 relay query contains a forbidden character')
    }
  }
}

function validateRawPath(path: string): void {
  if (INVALID_PERCENT_ENCODING.test(path)) {
    throw new TypeError('v2 relay base path contains an invalid percent escape')
  }
  for (const segment of path.split('/')) {
    let decoded: string
    try {
      decoded = decodeURIComponent(segment)
    } catch (cause) {
      throw new TypeError('v2 relay base path is not valid UTF-8', { cause })
    }
    if (decoded === '.' || decoded === '..') {
      throw new TypeError('v2 relay base path must not contain dot segments')
    }
  }
}

function validateSpelling(raw: string): void {
  const characters = Array.from(raw)
  if (
    raw.length === 0 ||
    boundaryWhitespace(characters[0]) ||
    boundaryWhitespace(characters.at(-1)) ||
    raw.includes('\\') ||
    raw.includes('#')
  ) {
    throw new TypeError('v2 relay base spelling is invalid')
  }
  for (let index = 0; index < raw.length; index += 1) {
    const unit = raw.charCodeAt(index)
    if (unit <= 0x1f || unit === 0x7f) {
      throw new TypeError('v2 relay base contains a control byte')
    }
  }
}

function rawPathAndQuery(raw: string): { readonly path: string; readonly query: string } {
  const schemeEnd = raw.indexOf('://')
  if (schemeEnd <= 0 || !/^[A-Za-z][A-Za-z0-9+.-]*$/u.test(raw.slice(0, schemeEnd))) {
    throw new TypeError('v2 relay base must be an absolute authority URL')
  }
  const authorityStart = schemeEnd + 3
  const pathStart = raw.indexOf('/', authorityStart)
  const queryStart = raw.indexOf('?', authorityStart)
  let authorityEnd = raw.length
  if (pathStart !== -1) authorityEnd = pathStart
  if (queryStart !== -1) authorityEnd = Math.min(authorityEnd, queryStart)
  if (authorityEnd === authorityStart || queryStart === raw.length - 1) {
    throw new TypeError('v2 relay base must contain an authority and no empty query marker')
  }
  let path = ''
  if (pathStart !== -1 && (queryStart === -1 || pathStart < queryStart)) {
    const pathEnd = queryStart === -1 ? raw.length : queryStart
    path = raw.slice(pathStart, pathEnd)
  }
  const query = queryStart === -1 ? '' : raw.slice(queryStart + 1)
  return { path, query }
}

function parseURL(raw: string): URL {
  let parsed: URL
  try {
    parsed = new URL(raw)
  } catch (cause) {
    throw new TypeError('v2 relay base URL is invalid', { cause })
  }
  if (parsed.username !== '' || parsed.password !== '') {
    throw new TypeError('v2 relay base must not contain userinfo')
  }
  return parsed
}

function canonicalScheme(protocol: string): 'ws' | 'wss' {
  let scheme: 'ws' | 'wss'
  switch (protocol.toLowerCase()) {
    case 'http:':
    case 'ws:':
      scheme = 'ws'
      break
    case 'https:':
    case 'wss:':
      scheme = 'wss'
      break
    default:
      throw new TypeError('v2 relay base scheme is unsupported')
  }
  return scheme
}

function canonicalHost(parsed: URL): string {
  const hostname = parsed.hostname.toLowerCase()
  const unbracketed = hostname.startsWith('[') && hostname.endsWith(']')
    ? hostname.slice(1, -1)
    : hostname
  const isIPv6 = unbracketed.includes(':')
  const isIPv4 = /^\d+\.\d+\.\d+\.\d+$/u.test(unbracketed)
  if (
    Array.from(unbracketed).some((character) => character.charCodeAt(0) >= 0x80) ||
    (!isIPv4 && !isIPv6 && !validDNSName(unbracketed))
  ) {
    throw new TypeError('v2 relay host is invalid')
  }
  const port = parsed.port
  const address = isIPv6 ? `[${unbracketed}]` : unbracketed
  const portSuffix = port === '' ? '' : ':' + port
  return address + portSuffix
}

function canonicalAddress(raw: string): {
  readonly dial: string
  readonly identity: string
} {
  validateSpelling(raw)
  const rawParts = rawPathAndQuery(raw)
  validateRawPath(rawParts.path)
  validateQuery(rawParts.query)
  const parsed = parseURL(raw)
  const scheme = canonicalScheme(parsed.protocol)
  const host = canonicalHost(parsed)
  const escapedPath = parsed.pathname.endsWith('/')
    ? parsed.pathname.slice(0, -1)
    : parsed.pathname
  const identity = `${scheme}://${host}${escapedPath}${V2_RELAY_WEBSOCKET_PATH}`
  const dial = rawParts.query === '' ? identity : `${identity}?${rawParts.query}`
  return { dial, identity }
}

export async function canonicalV2RelayEndpoint(
  raw: string,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<V2RelayEndpoint> {
  const canonical = canonicalAddress(raw)
  const relayIdentity = await sha256(
    concatBytes([RELAY_IDENTITY_DOMAIN, TEXT_ENCODER.encode(canonical.identity)]),
    runtime,
  )
  return Object.freeze({
    dialEndpoint: canonical.dial,
    relayIdentityEndpoint: canonical.identity,
    relayIdentity,
  })
}
