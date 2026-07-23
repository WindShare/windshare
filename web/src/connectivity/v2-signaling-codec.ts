import { equalBytes } from '../crypto/bytes'
import { isWellFormedUnicode } from '../protocol/text'
import {
  decodeCanonicalCbor,
  encodeCanonicalCbor,
  requireArray,
  requireBytes,
  requireText,
  requireUnsigned,
} from '../protocol/cbor'

export const V2_SIGNALING_SCHEMA_VERSION = 1
export const V2_SIGNALING_IDENTITY_BYTES = 16
export const V2_SIGNALING_MAXIMUM_SDP_BYTES = 60 * 1024
export const V2_SIGNALING_MAXIMUM_CANDIDATE_BYTES = 4 * 1024
export const V2_SIGNALING_MAXIMUM_MID_BYTES = 256
export const V2_SIGNALING_MAXIMUM_USERNAME_BYTES = 256

const MAXIMUM_SIGNALING_BODY_BYTES = 65_536 - 44
const MAXIMUM_UINT16 = 0xffff
const TEXT_ENCODER = new TextEncoder()

export interface V2PeerBinding {
  readonly peerPathId: Uint8Array<ArrayBuffer>
  readonly attemptId: Uint8Array<ArrayBuffer>
}

export interface V2PeerDescription extends V2PeerBinding {
  readonly sdp: string
}

export interface V2PeerCandidate extends V2PeerBinding {
  readonly candidate: string
  readonly sdpMid: string | null
  readonly sdpMLineIndex: number | null
  readonly usernameFragment: string | null
}

export class V2SignalingCodecError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'V2SignalingCodecError'
  }
}

export function encodeV2PeerOffer(value: V2PeerDescription): Uint8Array<ArrayBuffer> {
  return encodeDescription(value)
}

export function decodeV2PeerOffer(encoded: Uint8Array): V2PeerDescription {
  return decodeDescription(encoded, 'peer offer')
}

export function encodeV2PeerAnswer(value: V2PeerDescription): Uint8Array<ArrayBuffer> {
  return encodeDescription(value)
}

export function decodeV2PeerAnswer(encoded: Uint8Array): V2PeerDescription {
  return decodeDescription(encoded, 'peer answer')
}

export function encodeV2PeerCandidate(value: V2PeerCandidate): Uint8Array<ArrayBuffer> {
  const binding = snapshotBinding(value)
  return encodeCanonicalCbor([
    V2_SIGNALING_SCHEMA_VERSION,
    binding.peerPathId,
    binding.attemptId,
    requireBoundedText(
      value.candidate,
      V2_SIGNALING_MAXIMUM_CANDIDATE_BYTES,
      false,
      'ICE candidate',
    ),
    requireOptionalText(value.sdpMid, V2_SIGNALING_MAXIMUM_MID_BYTES, 'SDP mid'),
    requireOptionalLineIndex(value.sdpMLineIndex),
    requireOptionalText(
      value.usernameFragment,
      V2_SIGNALING_MAXIMUM_USERNAME_BYTES,
      'ICE username fragment',
    ),
  ])
}

export function decodeV2PeerCandidate(encoded: Uint8Array): V2PeerCandidate {
  const fields = exactArray(encoded, 7, 'peer candidate')
  const binding = decodePrefix(fields, 'peer candidate')
  return Object.freeze({
    ...binding,
    candidate: requireBoundedText(
      requireText(fields[3], 'ICE candidate'),
      V2_SIGNALING_MAXIMUM_CANDIDATE_BYTES,
      false,
      'ICE candidate',
    ),
    sdpMid: decodeOptionalText(fields[4], V2_SIGNALING_MAXIMUM_MID_BYTES, 'SDP mid'),
    sdpMLineIndex: decodeOptionalLineIndex(fields[5]),
    usernameFragment: decodeOptionalText(
      fields[6],
      V2_SIGNALING_MAXIMUM_USERNAME_BYTES,
      'ICE username fragment',
    ),
  })
}

export function sameV2PeerBinding(left: V2PeerBinding, right: V2PeerBinding): boolean {
  return equalBytes(left.peerPathId, right.peerPathId) && equalBytes(left.attemptId, right.attemptId)
}

function encodeDescription(value: V2PeerDescription): Uint8Array<ArrayBuffer> {
  const binding = snapshotBinding(value)
  return encodeCanonicalCbor([
    V2_SIGNALING_SCHEMA_VERSION,
    binding.peerPathId,
    binding.attemptId,
    requireBoundedText(value.sdp, V2_SIGNALING_MAXIMUM_SDP_BYTES, false, 'SDP'),
  ])
}

function decodeDescription(encoded: Uint8Array, label: string): V2PeerDescription {
  const fields = exactArray(encoded, 4, label)
  const binding = decodePrefix(fields, label)
  return Object.freeze({
    ...binding,
    sdp: requireBoundedText(
      requireText(fields[3], `${label} SDP`),
      V2_SIGNALING_MAXIMUM_SDP_BYTES,
      false,
      `${label} SDP`,
    ),
  })
}

function exactArray(encoded: Uint8Array, expected: number, label: string): readonly unknown[] {
  const value = requireArray(
    decodeCanonicalCbor(encoded, MAXIMUM_SIGNALING_BODY_BYTES, label),
    expected,
    label,
  )
  if (value.length !== expected) throw new V2SignalingCodecError(`${label} has the wrong field count`)
  return value
}

function decodePrefix(fields: readonly unknown[], label: string): V2PeerBinding {
  if (requireUnsigned(fields[0], `${label} schema version`) !== BigInt(V2_SIGNALING_SCHEMA_VERSION)) {
    throw new V2SignalingCodecError(`${label} uses an unsupported schema version`)
  }
  return Object.freeze({
    peerPathId: requireBytes(fields[1], V2_SIGNALING_IDENTITY_BYTES, 'peer path ID', true),
    attemptId: requireBytes(fields[2], V2_SIGNALING_IDENTITY_BYTES, 'peer attempt ID', true),
  })
}

function snapshotBinding(binding: V2PeerBinding): V2PeerBinding {
  return Object.freeze({
    peerPathId: requireBytes(binding.peerPathId, V2_SIGNALING_IDENTITY_BYTES, 'peer path ID', true),
    attemptId: requireBytes(binding.attemptId, V2_SIGNALING_IDENTITY_BYTES, 'peer attempt ID', true),
  })
}

function requireBoundedText(
  value: string,
  maximumBytes: number,
  allowEmpty: boolean,
  label: string,
): string {
  if (
    typeof value !== 'string' ||
    !isWellFormedUnicode(value) ||
    value.normalize('NFC') !== value ||
    (!allowEmpty && value.length === 0) ||
    TEXT_ENCODER.encode(value).byteLength > maximumBytes
  ) {
    throw new V2SignalingCodecError(`${label} is empty, non-NFC, or exceeds its UTF-8 limit`)
  }
  return value
}

function requireOptionalText(value: string | null, maximumBytes: number, label: string): string | null {
  return value === null ? null : requireBoundedText(value, maximumBytes, true, label)
}

function decodeOptionalText(value: unknown, maximumBytes: number, label: string): string | null {
  return value === null
    ? null
    : requireBoundedText(requireText(value, label), maximumBytes, true, label)
}

function requireOptionalLineIndex(value: number | null): number | null {
  if (value === null) return null
  if (!Number.isInteger(value) || value < 0 || value > MAXIMUM_UINT16) {
    throw new V2SignalingCodecError('SDP m-line index is outside uint16')
  }
  return value
}

function decodeOptionalLineIndex(value: unknown): number | null {
  if (value === null) return null
  const decoded = requireUnsigned(value, 'SDP m-line index')
  if (decoded > BigInt(MAXIMUM_UINT16)) {
    throw new V2SignalingCodecError('SDP m-line index is outside uint16')
  }
  return Number(decoded)
}
