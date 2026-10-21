import { decodeCanonicalCbor, encodeCanonicalCbor, requireArray, requireBytes, requireText, requireUnsigned } from '../../protocol/cbor'

export const TURN_CONTROL = Object.freeze({ demand: 1, grant: 2, ready: 3, retire: 4, unavailable: 5, available: 6, report: 7, recover: 8, quality: 9 } as const)
export const TURN_RECOVERY = Object.freeze({ allocationStateMismatch: 1, channelUnavailable: 2 } as const)
export const TURN_RETIRE = Object.freeze({ idle: 1, replaced: 2, expired: 3, budget: 4, revoked: 5, unhelpful: 6 } as const)
export const TURN_DEMAND_LIFETIME_MS = 120_000
export const TURN_MAX_CREDENTIAL_LIFETIME_MS = 48 * 60 * 60 * 1000
const SCHEMA = 4
const MAX_BODY_BYTES = 8192
const MAX_URLS = 8
const MAX_SECRET_BYTES = 1024
const MAX_URL_BYTES = 512
const IDENTITY_BYTES = 16

export interface TurnHeader {
  readonly sessionId: Uint8Array<ArrayBuffer>
  readonly ownerPathId: Uint8Array<ArrayBuffer>
  readonly sequence: bigint
}
export interface TurnGrant extends TurnHeader {
  readonly kind: 'grant'
  readonly pathId: Uint8Array<ArrayBuffer>
  readonly replaces: Uint8Array<ArrayBuffer>
  readonly provider: string
  readonly account: string
  readonly profile: string
  readonly credentials:
    | { readonly kind: 'fixed'; readonly username: string; readonly password: string }
    | { readonly kind: 'expiring'; readonly username: string; readonly password: string; readonly validForMs: number }
  readonly urls: readonly string[]
}
export interface TurnLane {
  readonly laneId: number
  readonly laneEpoch: number
}
export type TurnControl =
  | (TurnHeader & { readonly kind: 'demand'; readonly holdForMs: number })
  | TurnGrant
  | (TurnHeader & TurnLane & { readonly kind: 'ready'; readonly pathId: Uint8Array<ArrayBuffer> })
  | (TurnHeader & { readonly kind: 'retire'; readonly pathId: Uint8Array<ArrayBuffer>; readonly reason: number })
  | (TurnHeader & { readonly kind: 'unavailable'; readonly profile: string; readonly retryAfterMs: number; readonly reason: string })
  | (TurnHeader & { readonly kind: 'available'; readonly profiles: readonly string[] })
  | (TurnHeader & { readonly kind: 'report'; readonly pathId: Uint8Array<ArrayBuffer>; readonly usefulBytes: bigint })
  | (TurnHeader & { readonly kind: 'recover'; readonly pathId: Uint8Array<ArrayBuffer>; readonly reason: number })
  | (TurnHeader & TurnLane & { readonly kind: 'quality'; readonly pathId: Uint8Array<ArrayBuffer>;
    readonly networkGeneration: Uint8Array<ArrayBuffer>; readonly sampleSequence: bigint;
    readonly contentBytes: bigint; readonly busyMilliseconds: bigint; readonly pendingBytes: bigint })

export function encodeTurnControl(message: TurnControl): Uint8Array<ArrayBuffer> {
  const fields: unknown[] = [SCHEMA, TURN_CONTROL[message.kind], message.sessionId, message.ownerPathId, message.sequence]
  switch (message.kind) {
    case 'demand': fields.push(message.holdForMs); break
    case 'grant': fields.push(message.pathId, message.replaces, message.provider, message.account, message.profile,
      message.credentials.kind === 'fixed' ? 1 : 2, message.credentials.username, message.credentials.password,
      message.credentials.kind === 'fixed' ? 0 : message.credentials.validForMs, [...message.urls]); break
    case 'ready': fields.push(message.pathId, message.laneId, message.laneEpoch); break
    case 'retire': fields.push(message.pathId, message.reason); break
    case 'unavailable': fields.push(message.profile, message.retryAfterMs, message.reason); break
    case 'available': fields.push([...message.profiles]); break
    case 'report': fields.push(message.pathId, message.usefulBytes); break
    case 'recover': fields.push(message.pathId, message.reason); break
    case 'quality': fields.push(message.pathId, message.laneId, message.laneEpoch, message.networkGeneration,
      message.sampleSequence, message.contentBytes, message.busyMilliseconds, message.pendingBytes); break
  }
  const body = encodeCanonicalCbor(fields)
  decodeTurnControl(body)
  return body
}

export function decodeTurnControl(body: Uint8Array): TurnControl {
  const fields = requireArray(decodeCanonicalCbor(body, MAX_BODY_BYTES, 'TURN control'), 15, 'TURN control')
  if (fields.length < 6) throw new TypeError('Invalid TURN field count')
  if (requireUnsigned(fields[0], 'TURN schema') !== BigInt(SCHEMA)) throw new TypeError('Invalid TURN schema')
  const header: TurnHeader = {
    sessionId: requireBytes(fields[2], IDENTITY_BYTES, 'TURN session', true),
    ownerPathId: requireBytes(fields[3], IDENTITY_BYTES, 'TURN owner path', true),
    sequence: requireUnsigned(fields[4], 'TURN sequence'),
  }
  if (header.sequence === 0n || header.sequence > 0xffff_ffff_ffff_ffffn) throw new RangeError('Invalid TURN sequence')
  switch (Number(requireUnsigned(fields[1], 'TURN kind'))) {
    case TURN_CONTROL.demand:
      count(fields, 6)
      return { ...header, kind: 'demand', holdForMs: duration(fields[5], TURN_DEMAND_LIFETIME_MS) }
    case TURN_CONTROL.grant: return decodeGrant(fields, header)
    case TURN_CONTROL.quality: {
      count(fields, 13)
      const sampleSequence = unsigned64(fields[9])
      if (sampleSequence === 0n) throw new RangeError('Invalid TURN sample sequence')
      return { ...header, kind: 'quality', pathId: requireBytes(fields[5], IDENTITY_BYTES, 'TURN quality path', true),
        laneId: laneIdentity(fields[6]), laneEpoch: laneIdentity(fields[7]),
        networkGeneration: requireBytes(fields[8], IDENTITY_BYTES, 'TURN quality network'), sampleSequence,
        contentBytes: unsigned64(fields[10]), busyMilliseconds: unsigned64(fields[11]), pendingBytes: unsigned64(fields[12]) }
    }
    case TURN_CONTROL.ready:
      count(fields, 8)
      return { ...header, kind: 'ready', pathId: requireBytes(fields[5], IDENTITY_BYTES, 'TURN ready path', true),
        laneId: laneIdentity(fields[6]), laneEpoch: laneIdentity(fields[7]) }
    case TURN_CONTROL.report: {
      count(fields, 7)
      const usefulBytes = requireUnsigned(fields[6], 'TURN useful bytes')
      if (usefulBytes > 0xffff_ffff_ffff_ffffn) throw new RangeError('Invalid TURN useful bytes')
      return { ...header, kind: 'report', pathId: requireBytes(fields[5], IDENTITY_BYTES, 'TURN path', true), usefulBytes }
    }
    case TURN_CONTROL.recover: {
      count(fields, 7)
      const reason = Number(requireUnsigned(fields[6], 'TURN recovery'))
      if (reason < TURN_RECOVERY.allocationStateMismatch || reason > TURN_RECOVERY.channelUnavailable) throw new TypeError('Invalid TURN recovery')
      return { ...header, kind: 'recover', pathId: requireBytes(fields[5], IDENTITY_BYTES, 'TURN recovery path', true), reason }
    }
    case TURN_CONTROL.retire: {
      count(fields, 7)
      const reason = Number(requireUnsigned(fields[6], 'TURN retirement'))
      if (reason < TURN_RETIRE.idle || reason > TURN_RETIRE.unhelpful) throw new TypeError('Invalid TURN retirement')
      return { ...header, kind: 'retire', pathId: requireBytes(fields[5], IDENTITY_BYTES, 'TURN retired path', true), reason }
    }
    case TURN_CONTROL.unavailable:
      count(fields, 8)
      return { ...header, kind: 'unavailable', profile: identifier(fields[5]),
        retryAfterMs: duration(fields[6], TURN_DEMAND_LIFETIME_MS), reason: text(fields[7], MAX_SECRET_BYTES) }
    case TURN_CONTROL.available: {
      count(fields, 6)
      const profiles = requireArray(fields[5], MAX_URLS, 'TURN profiles').map(identifier)
      return { ...header, kind: 'available', profiles }
    }
    default: throw new TypeError('Invalid TURN control kind')
  }
}

function laneIdentity(value: unknown): number {
  const identity = requireUnsigned(value, 'TURN lane identity')
  if (identity === 0n || identity > 0xffff_ffffn) throw new RangeError('Invalid TURN lane identity')
  return Number(identity)
}

function unsigned64(value: unknown): bigint {
  const number = requireUnsigned(value, 'TURN measurement')
  if (number > 0xffff_ffff_ffff_ffffn) throw new RangeError('Invalid TURN measurement')
  return number
}

function decodeGrant(fields: readonly unknown[], header: TurnHeader): TurnGrant {
  count(fields, 15)
  const credentialKind = requireUnsigned(fields[10], 'TURN credential kind')
  const validForMs = duration(fields[13], TURN_MAX_CREDENTIAL_LIFETIME_MS)
  if ((credentialKind !== 1n && credentialKind !== 2n) || (credentialKind === 1n) !== (validForMs === 0)) {
    throw new TypeError('Invalid TURN credential lifetime')
  }
  const pathId = requireBytes(fields[5], IDENTITY_BYTES, 'TURN path', true)
  const replaces = requireBytes(fields[6], IDENTITY_BYTES, 'TURN replacement')
  if (pathId.every((byte, index) => byte === replaces[index])) throw new TypeError('TURN path replaces itself')
  const username = text(fields[11], MAX_SECRET_BYTES)
  const password = text(fields[12], MAX_SECRET_BYTES)
  const urls = requireArray(fields[14], MAX_URLS, 'TURN URLs').map((raw) => text(raw, MAX_URL_BYTES))
  if (urls.length === 0) throw new RangeError('No TURN URLs')
  return { ...header, kind: 'grant', pathId, replaces,
    provider: identifier(fields[7]), account: identifier(fields[8]), profile: identifier(fields[9]), urls,
    credentials: credentialKind === 1n ? { kind: 'fixed', username, password } : { kind: 'expiring', username, password, validForMs } }
}

export function turnRTCConfiguration(grant: TurnGrant): RTCConfiguration {
  for (const raw of grant.urls) {
    // The pinned native peer supports IPv4 TURN allocations. Keep browser/native
    // capability claims aligned; ordinary direct IPv6 remains independent.
    if (!/^turns?:[^:/?#@\s[\]]+(?::[1-9]\d{0,4})?(?:\?transport=(?:udp|tcp))?$/.test(raw) ||
      (raw.startsWith('turns:') && raw.endsWith('transport=udp'))) throw new TypeError('Unsupported TURN endpoint')
    const port = /:(\d+)(?:\?|$)/.exec(raw)?.[1]
    if (port !== undefined && Number(port) > 65535) throw new TypeError('Invalid TURN port')
  }
  return { iceTransportPolicy: 'relay', iceServers: [{ urls: [...grant.urls],
    username: grant.credentials.username, credential: grant.credentials.password }] }
}
function count(fields: readonly unknown[], size: number): void {
  if (fields.length !== size) throw new TypeError('Invalid TURN field count')
}
function duration(value: unknown, maximum: number): number {
  const number = requireUnsigned(value, 'TURN duration')
  if (number > BigInt(maximum)) throw new RangeError('Invalid TURN duration')
  return Number(number)
}
function text(value: unknown, maximum: number): string {
  const result = requireText(value, 'TURN text')
  if (result.length === 0 || new TextEncoder().encode(result).length > maximum) throw new TypeError('Invalid TURN text')
  return result
}
function identifier(value: unknown): string {
  const result = text(value, 64)
  if (!/^[a-zA-Z0-9_.-]+$/.test(result)) throw new TypeError('Invalid TURN identifier')
  return result
}
