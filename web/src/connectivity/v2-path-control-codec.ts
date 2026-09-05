import {
  decodeCanonicalCbor,
  encodeCanonicalCbor,
  requireArray,
  requireBytes,
  requireUnsigned,
  requireText,
} from '../protocol/cbor'

export const V2_PEER_PATH_CONTROL_KIND = Object.freeze({
  demand: 1,
  revoke: 2,
  mappingReady: 3,
  networkChanged: 4,
} as const)
export const V2_MAXIMUM_PATH_CONTROL_LIFETIME_MS = 120_000
const SCHEMA_VERSION = 2
const IDENTITY_BYTES = 16
const FIELD_COUNT = 8
const MAXIMUM_PROVIDER_PROFILE_LENGTH = 64
const MAXIMUM_BODY_BYTES = 256
const MAXIMUM_SEQUENCE = 0xffff_ffff_ffff_ffffn

export interface V2PeerPathControl {
  readonly peerPathId: Uint8Array<ArrayBuffer>
  readonly networkGenerationId: Uint8Array<ArrayBuffer>
  readonly controlSequence: bigint
  readonly kind: number
  readonly validForMilliseconds: number
  readonly holdForMilliseconds: number
  readonly providerProfile?: string
}

export function encodeV2PeerPathControl(value: V2PeerPathControl): Uint8Array<ArrayBuffer> {
  validate(value)
  return encodeCanonicalCbor([
    SCHEMA_VERSION, value.peerPathId, value.networkGenerationId, value.controlSequence,
    value.kind, value.validForMilliseconds, value.holdForMilliseconds, value.providerProfile ?? '',
  ])
}

export function decodeV2PeerPathControl(body: Uint8Array): V2PeerPathControl {
  const fields = requireArray(
    decodeCanonicalCbor(body, MAXIMUM_BODY_BYTES, 'peer path control'),
    FIELD_COUNT,
    'peer path control',
  )
  if (fields.length !== FIELD_COUNT || requireUnsigned(fields[0], 'path control schema') !== BigInt(SCHEMA_VERSION)) {
    throw new Error('invalid path control schema')
  }
  const value = Object.freeze({
    peerPathId: requireBytes(fields[1], IDENTITY_BYTES, 'peer path ID', true),
    networkGenerationId: requireBytes(fields[2], IDENTITY_BYTES, 'network generation ID', true),
    controlSequence: requireUnsigned(fields[3], 'control sequence'),
    kind: Number(requireUnsigned(fields[4], 'path control kind')),
    validForMilliseconds: Number(requireUnsigned(fields[5], 'path control lifetime')),
    holdForMilliseconds: Number(requireUnsigned(fields[6], 'path control hold')),
    providerProfile: requireText(fields[7], 'provider profile'),
  })
  validate(value)
  return value
}

function validate(value: V2PeerPathControl): void {
  const profile = value.providerProfile ?? ''
  if (typeof profile !== 'string' || profile.length > MAXIMUM_PROVIDER_PROFILE_LENGTH || /[^a-z0-9.-]/u.test(profile)) {
    throw new Error('invalid provider profile')
  }
  requireBytes(value.peerPathId, IDENTITY_BYTES, 'peer path ID', true)
  requireBytes(value.networkGenerationId, IDENTITY_BYTES, 'network generation ID', true)
  if (
    typeof value.controlSequence !== 'bigint' ||
    value.controlSequence <= 0n || value.controlSequence > MAXIMUM_SEQUENCE ||
    !Number.isInteger(value.kind) ||
    value.kind < V2_PEER_PATH_CONTROL_KIND.demand || value.kind > V2_PEER_PATH_CONTROL_KIND.networkChanged
  ) {
    throw new Error('invalid path control identity or kind')
  }
  // Receipt owns these durations; accepting wall-clock timestamps would make
  // remote clock skew capable of extending a local reachability lease.
  if (
    !Number.isInteger(value.validForMilliseconds) || value.validForMilliseconds < 0 ||
    value.validForMilliseconds > V2_MAXIMUM_PATH_CONTROL_LIFETIME_MS ||
    !Number.isInteger(value.holdForMilliseconds) || value.holdForMilliseconds < 0 ||
    value.holdForMilliseconds > value.validForMilliseconds ||
    (value.kind === V2_PEER_PATH_CONTROL_KIND.revoke) !== (value.validForMilliseconds === 0) ||
    (value.kind !== V2_PEER_PATH_CONTROL_KIND.demand && value.holdForMilliseconds !== 0)
  ) {
    throw new Error('invalid path control lifetime or hold')
  }
}
