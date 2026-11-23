import { equalBytes } from '../crypto/bytes'
import { isWellFormedUnicode } from '../protocol/text'
import { requirePeerSlot, type PeerAttemptOrdinal } from '../session/peer-slots/slots'
import {
  decodeCanonicalCbor,
  encodeCanonicalCbor,
  requireArray,
  requireBytes,
  requireText,
  requireUnsigned,
} from '../protocol/cbor'

export const SIGNALING_SCHEMA_VERSION = 5
export const SIGNALING_IDENTITY_BYTES = 16
export const SIGNALING_MAXIMUM_SDP_BYTES = 60 * 1024
export const SIGNALING_MAXIMUM_CANDIDATE_BYTES = 4 * 1024
export const SIGNALING_MAXIMUM_MID_BYTES = 256
export const SIGNALING_MAXIMUM_USERNAME_BYTES = 256

const MAXIMUM_SIGNALING_BODY_BYTES = 65_536 - 44
const MAXIMUM_UINT16 = 0xffff
const TEXT_ENCODER = new TextEncoder()

export type ConnectionPurpose = 'initial' | 'recovery' | 'throughput' | 'turn-relay'
const PURPOSES: readonly ConnectionPurpose[] = ['initial', 'recovery', 'throughput', 'turn-relay']
export interface PeerBinding extends PeerAttemptOrdinal {
  readonly purpose: ConnectionPurpose
  readonly peerPathId: Uint8Array<ArrayBuffer>
  readonly attemptId: Uint8Array<ArrayBuffer>
}

export interface PeerDescription extends PeerBinding {
  readonly sdp: string
}

export interface PeerCandidate extends PeerBinding {
  readonly candidate: string
  readonly sdpMid: string | null
  readonly sdpMLineIndex: number | null
  readonly usernameFragment: string | null
}

export class SignalingCodecError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'SignalingCodecError'
  }
}

export interface PeerOffer extends PeerDescription {
  readonly turnGrantId?: Uint8Array<ArrayBuffer>
}

export function encodePeerOffer(value: PeerOffer): Uint8Array<ArrayBuffer> {
  if (value.purpose === 'turn-relay') {
    return encodeCanonicalCbor([...descriptionFields(value), requireBytes(value.turnGrantId, SIGNALING_IDENTITY_BYTES, 'TURN grant ID', true)])
  }
  if (value.turnGrantId !== undefined) throw new SignalingCodecError('Unexpected TURN grant ID')
  return encodeCanonicalCbor([...descriptionFields(value), null])
}

export function decodePeerOffer(encoded: Uint8Array): PeerOffer {
  const fields = exactArray(encoded, 8, 'peer offer')
  const description = decodeDescription(fields, 'peer offer')
  if (description.purpose === 'turn-relay') {
    return { ...description, turnGrantId: requireBytes(fields[7], SIGNALING_IDENTITY_BYTES, 'TURN grant ID', true) }
  }
  if (fields[7] !== null) throw new SignalingCodecError('Unexpected TURN grant ID')
  return description
}

export function encodePeerAnswer(value: PeerDescription): Uint8Array<ArrayBuffer> {
  return encodeCanonicalCbor(descriptionFields(value))
}

export function decodePeerAnswer(encoded: Uint8Array): PeerDescription {
  return decodeDescription(exactArray(encoded, 7, 'peer answer'), 'peer answer')
}

export function encodePeerCandidate(value: PeerCandidate): Uint8Array<ArrayBuffer> {
  const binding = snapshotBinding(value)
  return encodeCanonicalCbor([
    SIGNALING_SCHEMA_VERSION,
    binding.peerPathId,
    binding.attemptId,
    binding.attemptSequence,
    binding.peerSlot,
    encodePurpose(binding.purpose),
    requireBoundedText(
      value.candidate,
      SIGNALING_MAXIMUM_CANDIDATE_BYTES,
      false,
      'ICE candidate',
    ),
    requireOptionalText(value.sdpMid, SIGNALING_MAXIMUM_MID_BYTES, 'SDP mid'),
    requireOptionalLineIndex(value.sdpMLineIndex),
    requireOptionalText(
      value.usernameFragment,
      SIGNALING_MAXIMUM_USERNAME_BYTES,
      'ICE username fragment',
    ),
  ])
}

export function decodePeerCandidate(encoded: Uint8Array): PeerCandidate {
  const fields = exactArray(encoded, 10, 'peer candidate')
  const binding = decodePrefix(fields, 'peer candidate')
  return Object.freeze({
    ...binding,
    candidate: requireBoundedText(
      requireText(fields[6], 'ICE candidate'),
      SIGNALING_MAXIMUM_CANDIDATE_BYTES,
      false,
      'ICE candidate',
    ),
    sdpMid: decodeOptionalText(fields[7], SIGNALING_MAXIMUM_MID_BYTES, 'SDP mid'),
    sdpMLineIndex: decodeOptionalLineIndex(fields[8]),
    usernameFragment: decodeOptionalText(
      fields[9],
      SIGNALING_MAXIMUM_USERNAME_BYTES,
      'ICE username fragment',
    ),
  })
}

export function samePeerBinding(left: PeerBinding, right: PeerBinding): boolean {
  return left.purpose === right.purpose && left.peerSlot === right.peerSlot && left.attemptSequence === right.attemptSequence && equalBytes(left.peerPathId, right.peerPathId) && equalBytes(left.attemptId, right.attemptId)
}

function descriptionFields(value: PeerDescription): unknown[] {
  const binding = snapshotBinding(value)
  return [
    SIGNALING_SCHEMA_VERSION,
    binding.peerPathId,
    binding.attemptId,
    binding.attemptSequence,
    binding.peerSlot,
    encodePurpose(binding.purpose),
    requireBoundedText(value.sdp, SIGNALING_MAXIMUM_SDP_BYTES, false, 'SDP'),
  ]
}

function decodeDescription(fields: readonly unknown[], label: string): PeerDescription {
  const binding = decodePrefix(fields, label)
  return Object.freeze({
    ...binding,
    sdp: requireBoundedText(
      requireText(fields[6], `${label} SDP`),
      SIGNALING_MAXIMUM_SDP_BYTES,
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
  if (value.length !== expected) throw new SignalingCodecError(`${label} has the wrong field count`)
  return value
}

function decodePrefix(fields: readonly unknown[], label: string): PeerBinding {
  if (requireUnsigned(fields[0], `${label} schema version`) !== BigInt(SIGNALING_SCHEMA_VERSION)) {
    throw new SignalingCodecError(`${label} uses an unsupported schema version`)
  }
  return Object.freeze({
    peerPathId: requireBytes(fields[1], SIGNALING_IDENTITY_BYTES, 'peer path ID', true),
    attemptId: requireBytes(fields[2], SIGNALING_IDENTITY_BYTES, 'peer attempt ID', true),
    attemptSequence: requireAttemptSequence(fields[3]),
    peerSlot: requirePeerSlot(requireUnsigned(fields[4], 'peer slot')),
    purpose: decodePurpose(fields[5]),
  })
}

function snapshotBinding(binding: PeerBinding): PeerBinding {
  return Object.freeze({
    peerPathId: requireBytes(binding.peerPathId, SIGNALING_IDENTITY_BYTES, 'peer path ID', true),
    attemptId: requireBytes(binding.attemptId, SIGNALING_IDENTITY_BYTES, 'peer attempt ID', true),
    attemptSequence: requireAttemptSequence(binding.attemptSequence),
    peerSlot: requirePeerSlot(requireUnsigned(binding.peerSlot, 'peer slot')),
    purpose: decodePurpose(encodePurpose(binding.purpose)),
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
    throw new SignalingCodecError(`${label} is empty, non-NFC, or exceeds its UTF-8 limit`)
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
    throw new SignalingCodecError('SDP m-line index is outside uint16')
  }
  return value
}

function decodeOptionalLineIndex(value: unknown): number | null {
  if (value === null) return null
  const decoded = requireUnsigned(value, 'SDP m-line index')
  if (decoded > BigInt(MAXIMUM_UINT16)) {
    throw new SignalingCodecError('SDP m-line index is outside uint16')
  }
  return Number(decoded)
}

function requireAttemptSequence(value: unknown): bigint {
  const sequence = requireUnsigned(value, 'attempt sequence')
  if (sequence === 0n || sequence > 0xffffffffffffffffn) {
    throw new SignalingCodecError('attempt sequence is outside positive uint64')
  }
  return sequence
}

function encodePurpose(value: ConnectionPurpose): number {
 const index = PURPOSES.indexOf(value)
 if (index < 0) throw new SignalingCodecError('Invalid connection purpose')
 return index
}
function decodePurpose(value: unknown): ConnectionPurpose {
 const index = requireUnsigned(value, 'connection purpose')
 const purpose = PURPOSES[Number(index)]
 if (index > 3n || purpose === undefined) throw new SignalingCodecError('Invalid connection purpose')
 return purpose
}
