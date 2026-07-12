import {
  CIPHER_SUITE_V1,
  READ_SECRET_BYTES,
  SHARE_ID_BASE64URL_CHARACTERS,
  SHARE_ID_BYTES,
  type CapabilityLink,
  type CipherSuite,
  type ReadSecret,
  type RelayHint,
  type ShareId,
} from '../contracts'
import { decodeBase64Url, encodeBase64Url, equalBytes } from './bytes'
import { CryptoError } from './errors'

const KEY_BYTES = 1 + READ_SECRET_BYTES
const KEY_CHARACTERS = 23
const MAX_CAPABILITY_KEY_CHARACTERS = 128
const MAX_ENCODED_CAPABILITY_KEY_CHARACTERS = MAX_CAPABILITY_KEY_CHARACTERS * 3
const RELAY_PARAMETER = 'r'
const ABSOLUTE_AUTHORITY_URL_PATTERN = /^[A-Za-z][A-Za-z0-9+.-]*:\/\/[^/?#]+/u
const INVALID_PERCENT_ENCODING_PATTERN = /%(?![0-9A-Fa-f]{2})/u
const ENCODED_PATH_SEPARATOR_PATTERN = /%2f/giu
const REPEATED_PATH_SEPARATOR_PATTERN = /\/+/gu

interface ParsedBareLink {
  readonly shareId: ShareId
  readonly relayHints: readonly RelayHint[]
  readonly embeddedKey: string | undefined
}

interface DecodedKey {
  readonly suite: CipherSuite
  readonly readSecret: ReadSecret
}

export interface SplitCapabilityLink {
  readonly bareUrl: string
  readonly key: string
}

function malformedLink(message: string): never {
  throw new CryptoError('malformed-link', `Malformed capability link: ${message}`)
}

function containsUrlControlCharacter(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const unit = value.charCodeAt(index)
    if (unit <= 0x1f || unit === 0x7f) {
      return true
    }
  }
  return false
}

function parseAbsoluteUrl(raw: string, label: string): URL {
  const trimmed = raw.trim()
  // WHATWG URL parsing repairs missing authority delimiters and backslashes.
  // Rejecting those repairs keeps browser acceptance aligned with Go net/url.
  if (
    !ABSOLUTE_AUTHORITY_URL_PATTERN.test(trimmed) ||
    trimmed.includes('\\') ||
    INVALID_PERCENT_ENCODING_PATTERN.test(trimmed) ||
    containsUrlControlCharacter(trimmed)
  ) {
    return malformedLink(`${label} must be an absolute URL with valid encoding`)
  }
  let url: URL
  try {
    url = new URL(trimmed)
  } catch {
    return malformedLink(`${label} must be an absolute URL with valid encoding`)
  }
  if (url.host === '') {
    return malformedLink(`${label} must include a host`)
  }
  // Go's strict query parser rejects raw semicolons because treating them as
  // data or separators is ecosystem-dependent. Require %3B so both runtimes
  // derive exactly the same ordered relay list from a copied link.
  if (url.search.includes(';')) {
    return malformedLink(`${label} query must percent-encode semicolons`)
  }
  try {
    // URLSearchParams replaces malformed UTF-8 with U+FFFD, while Go preserves
    // the invalid bytes. Validate before that lossy repair can create two relay
    // identities for one copied query string.
    decodeURIComponent(url.search.slice(1).replaceAll('+', ' '))
  } catch {
    return malformedLink(`${label} query must use valid UTF-8 encoding`)
  }
  return url
}

function decodeFragment(fragment: string): string {
  if (fragment.length > MAX_ENCODED_CAPABILITY_KEY_CHARACTERS) {
    throw new CryptoError('malformed-key', 'Capability key has an invalid encoded length')
  }
  try {
    return decodeURIComponent(fragment)
  } catch {
    return malformedLink('fragment encoding is invalid')
  }
}

function validatedShareId(value: string): ShareId {
  if (value.length !== SHARE_ID_BASE64URL_CHARACTERS) {
    throw new CryptoError('malformed-share-id', 'Capability link has an invalid share ID')
  }
  const bytes = decodeBase64Url(value)
  if (bytes?.byteLength !== SHARE_ID_BYTES) {
    throw new CryptoError('malformed-share-id', 'Capability link has an invalid share ID')
  }
  return value as ShareId
}

function parseBareLink(raw: string): ParsedBareLink {
  const url = parseAbsoluteUrl(raw, 'the URL')

  // net/url resolves an escaped slash before selecting the final path segment.
  // WHATWG URL.pathname retains it, so decode only that separator before splitting.
  const pathname = url.pathname.replace(ENCODED_PATH_SEPARATOR_PATTERN, '/')
  let pathStart = 0
  let pathEnd = pathname.length
  while (pathname[pathStart] === '/') {
    pathStart += 1
  }
  while (pathEnd > pathStart && pathname[pathEnd - 1] === '/') {
    pathEnd -= 1
  }
  const trimmedPath = pathname.slice(pathStart, pathEnd)
  const slash = trimmedPath.lastIndexOf('/')
  const encodedShareId = slash === -1 ? trimmedPath : trimmedPath.slice(slash + 1)
  let shareIdText: string
  try {
    shareIdText = decodeURIComponent(encodedShareId)
  } catch {
    throw new CryptoError('malformed-share-id', 'Capability link has an invalid share ID')
  }
  const shareId = validatedShareId(shareIdText)

  const relayHints = Object.freeze(
    url.searchParams.getAll(RELAY_PARAMETER).map((relay) => relay as RelayHint),
  )
  const embeddedKey = url.hash === '' ? undefined : decodeFragment(url.hash.slice(1))
  return Object.freeze({
    shareId,
    relayHints,
    embeddedKey,
  })
}

function keyPayload(input: string): string {
  const trimmed = input.trim()
  const hash = trimmed.indexOf('#')
  return (hash === -1 ? trimmed : trimmed.slice(hash + 1)).trim()
}

export function decodeCapabilityKey(input: string): DecodedKey {
  const encoded = keyPayload(input)
  if (encoded === '' || encoded.length > MAX_CAPABILITY_KEY_CHARACTERS) {
    throw new CryptoError('malformed-key', 'Capability key has an invalid length')
  }
  const raw = decodeBase64Url(encoded)
  if (raw === undefined || raw.byteLength === 0) {
    throw new CryptoError('malformed-key', 'Capability key is not canonical base64url')
  }
  if (raw[0] !== CIPHER_SUITE_V1) {
    throw new CryptoError(
      'unsupported-suite',
      'Capability link uses an unsupported cipher suite; upgrade required',
    )
  }
  if (encoded.length !== KEY_CHARACTERS || raw.byteLength !== KEY_BYTES) {
    throw new CryptoError('malformed-key', 'Capability key has an invalid suite length')
  }
  return Object.freeze({
    suite: CIPHER_SUITE_V1,
    readSecret: raw.slice(1) as ReadSecret,
  })
}

export function encodeCapabilityKey(suite: number, readSecret: Uint8Array): string {
  if (suite !== CIPHER_SUITE_V1) {
    throw new CryptoError(
      'unsupported-suite',
      'Capability link uses an unsupported cipher suite; upgrade required',
    )
  }
  if (readSecret.byteLength !== READ_SECRET_BYTES) {
    throw new CryptoError(
      'invalid-key-material',
      `Read secret must be exactly ${READ_SECRET_BYTES} bytes`,
    )
  }
  const raw = new Uint8Array(KEY_BYTES)
  raw[0] = suite
  raw.set(readSecret, 1)
  return encodeBase64Url(raw)
}

function formattedUrl(base: string, link: CapabilityLink, includeKey: boolean): string {
  const url = parseAbsoluteUrl(base, 'frontend base URL')
  const shareId = validatedShareId(link.shareId)
  // Go URL.JoinPath collapses literal separators while retaining escaped slashes.
  const basePath = url.pathname.replace(REPEATED_PATH_SEPARATOR_PATTERN, '/')
  let basePathEnd = basePath.length
  while (basePathEnd > 0 && basePath[basePathEnd - 1] === '/') {
    basePathEnd -= 1
  }
  url.pathname = `${basePath.slice(0, basePathEnd)}/${shareId}`
  url.search = ''
  for (const relay of link.relayHints) {
    url.searchParams.append(RELAY_PARAMETER, relay)
  }
  const key = encodeCapabilityKey(link.suite, link.readSecret)
  url.hash = includeKey ? key : ''
  return url.toString()
}

export function formatCapabilityLink(base: string, link: CapabilityLink): string {
  return formattedUrl(base, link, true)
}

export function splitCapabilityLink(
  base: string,
  link: CapabilityLink,
): SplitCapabilityLink {
  return Object.freeze({
    bareUrl: formattedUrl(base, link, false),
    key: encodeCapabilityKey(link.suite, link.readSecret),
  })
}

function assembleLink(bare: ParsedBareLink, key: DecodedKey): CapabilityLink {
  return Object.freeze({
    suite: key.suite,
    shareId: bare.shareId,
    readSecret: key.readSecret,
    relayHints: bare.relayHints,
  })
}

export function parseCapabilityLink(raw: string): CapabilityLink {
  const bare = parseBareLink(raw)
  if (bare.embeddedKey === undefined || bare.embeddedKey === '') {
    throw new CryptoError(
      'missing-key',
      'Capability link does not contain a key fragment; supply the separate key',
    )
  }
  return assembleLink(bare, decodeCapabilityKey(bare.embeddedKey))
}

export function mergeCapabilityLink(bareLink: string, keyInput: string): CapabilityLink {
  const bare = parseBareLink(bareLink)
  const supplied = decodeCapabilityKey(keyInput)
  if (bare.embeddedKey !== undefined && bare.embeddedKey !== '') {
    const embedded = decodeCapabilityKey(bare.embeddedKey)
    if (
      embedded.suite !== supplied.suite ||
      !equalBytes(embedded.readSecret, supplied.readSecret)
    ) {
      throw new CryptoError(
        'key-conflict',
        'Embedded and separately supplied capability keys do not match',
      )
    }
  }
  return assembleLink(bare, supplied)
}
