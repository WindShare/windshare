import { canonicalICEHost } from '../network/stun-endpoint'
import {
  CborError,
  decodeCanonicalCbor,
  encodeCanonicalCbor,
  requireArray,
  requireBoolean,
  requireBytes,
  requireText,
  requireUnsigned,
} from './cbor'

export const MAX_TURN_PATHS = 4
const MAX_BODY = 16 * 1024
const MAX_ID_BYTES = 128
const MAX_URL_BYTES = 512
const MAX_CREDENTIAL_BYTES = 1024
const textEncoder = new TextEncoder()

const modes = ['parallel', 'all'] as const
const statuses = ['ok', 'unconfigured', 'retry_later', 'partial_retryable'] as const
export type TURNMode = (typeof modes)[number]
export type TURNStatus = (typeof statuses)[number]
export interface TURNDemand {
  readonly requestId: bigint
  readonly mode: TURNMode
  readonly serviceIds: readonly string[]
}
export interface TURNGrant {
  readonly serviceId: string
  readonly grantId: Uint8Array<ArrayBuffer>
  readonly urls: readonly string[]
  readonly username: string
  readonly password: string
  readonly validForMs: bigint
}
export interface TURNGrants {
  readonly requestId: bigint
  readonly status: TURNStatus
  readonly hasFallback: boolean
  readonly grants: readonly TURNGrant[]
}

function text(value: unknown, max: number): string {
  const result = requireText(value, 'TURN field')
  if (!result || textEncoder.encode(result).length > max) throw new CborError('Invalid TURN text length')
  return result
}
function id(value: unknown): string {
  const result = text(value, MAX_ID_BYTES)
  // Go's strings.TrimSpace follows Unicode White_Space, which differs from JS trim at U+0085 and U+FEFF.
  if (/^\p{White_Space}|\p{White_Space}$/u.test(result)) throw new CborError('Invalid TURN service ID')
  return result
}
function unique<T>(items: readonly T[]): void {
  if (new Set(items).size !== items.length) throw new CborError('Duplicate TURN identifier')
}
function header(body: Uint8Array, kind: number, count: number): readonly unknown[] {
  const fields = requireArray(decodeCanonicalCbor(body, MAX_BODY, 'TURN control'), count, 'TURN control')
  if (
    fields.length !== count ||
    fields[0] !== 1 ||
    fields[1] !== kind ||
    requireUnsigned(fields[2], 'request ID') === 0n
  )
    throw new CborError('Invalid TURN control header')
  return fields
}
export function decodeTURNDemand(body: Uint8Array): TURNDemand {
  const fields = header(body, 1, 5)
  if (fields[3] !== 1 && fields[3] !== 2) throw new CborError('Invalid TURN demand mode')
  const serviceIds = requireArray(fields[4], MAX_TURN_PATHS, 'TURN targets').map(id)
  unique(serviceIds)
  return { requestId: requireUnsigned(fields[2], 'request ID'), mode: fields[3] === 1 ? 'parallel' : 'all', serviceIds }
}
export function encodeTURNDemand(demand: TURNDemand): Uint8Array<ArrayBuffer> {
  const body = encodeCanonicalCbor([1, 1, demand.requestId, modes.indexOf(demand.mode) + 1, demand.serviceIds])
  decodeTURNDemand(body)
  return body
}
function turnURL(value: unknown): string {
  const url = text(value, MAX_URL_BYTES)
  const match = /^turns?:([a-zA-Z0-9._-]+|\[[a-fA-F0-9:.]+\])(?::(\d+))?(?:\?transport=(?:udp|tcp))?$/.exec(url)
  if (!match || match[0] !== url || (match[2] !== undefined && (Number(match[2]) < 1 || Number(match[2]) > 65535)))
    throw new CborError('Invalid TURN URL')
  try {
    canonicalICEHost(match[1]!)
  } catch (cause) {
    throw new CborError('Invalid TURN host', { cause })
  }
  return url
}
function grant(value: unknown): TURNGrant {
  const fields = requireArray(value, 6, 'TURN grant')
  if (fields.length !== 6) throw new CborError('Invalid TURN grant')
  const urls = requireArray(fields[2], 8, 'TURN URLs').map(turnURL)
  if (urls.length === 0) throw new CborError('Missing TURN URLs')
  return {
    serviceId: id(fields[0]),
    grantId: requireBytes(fields[1], 16, 'TURN grant ID', true),
    urls,
    username: text(fields[3], MAX_CREDENTIAL_BYTES),
    password: text(fields[4], MAX_CREDENTIAL_BYTES),
    validForMs: requireUnsigned(fields[5], 'TURN lifetime'),
  }
}
export function decodeTURNGrants(body: Uint8Array): TURNGrants {
  const fields = header(body, 2, 6)
  const status = statuses[Number(requireUnsigned(fields[3], 'TURN status')) - 1]
  const hasFallback = requireBoolean(fields[4], 'TURN fallback role')
  const grants = requireArray(fields[5], MAX_TURN_PATHS, 'TURN grants').map(grant)
  if (!status || (status === 'partial_retryable' && grants.length === 0) ||
    ((status === 'retry_later' || status === 'unconfigured') && grants.length !== 0) ||
    (status === 'unconfigured' && hasFallback)) {
    throw new CborError('Invalid TURN status')
  }
  unique(grants.map((item) => item.serviceId))
  unique(grants.map((item) => item.grantId.toString()))
  return { requestId: requireUnsigned(fields[2], 'request ID'), status, hasFallback, grants }
}
export function encodeTURNGrants(value: TURNGrants): Uint8Array<ArrayBuffer> {
  const body = encodeCanonicalCbor([
    1,
    2,
    value.requestId,
    statuses.indexOf(value.status) + 1,
    value.hasFallback,
    value.grants.map((g) => [g.serviceId, g.grantId, g.urls, g.username, g.password, g.validForMs]),
  ])
  decodeTURNGrants(body)
  return body
}
