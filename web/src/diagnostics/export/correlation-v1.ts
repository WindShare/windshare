import { encodeBase64Url } from '../../crypto/bytes'
import type {
  FailureCorrelation,
  FailureIdentity,
  FailureIdentityKind,
  FailureLaneCorrelation,
} from '../incident/fact'

const IDENTITY_BYTE_LENGTH = 16
const UINT32_MAX = 0xffff_ffff

export interface CorrelationV1 {
  readonly protocol_session_id?: string
  readonly protocol_operation_id?: string
  readonly peer_path_id?: string
  readonly peer_attempt_id?: string
  readonly lane_id?: number
  readonly lane_epoch?: number
}

// Projection is the only boundary allowed to turn typed identity bytes into
// strings, keeping unrelated identities distinguishable throughout business flow.
export function projectCorrelationV1(
  correlation: FailureCorrelation | undefined,
): CorrelationV1 | undefined {
  if (correlation === undefined) return undefined

  const protocolSessionId = projectIdentity(
    correlation.protocolSessionId,
    'protocol_session',
  )
  const protocolOperationId = projectIdentity(
    correlation.protocolOperationId,
    'protocol_operation',
  )
  const peerPathId = projectIdentity(correlation.peerPathId, 'peer_path')
  const peerAttemptId = projectIdentity(correlation.peerAttemptId, 'peer_attempt')
  const lane = validateLane(correlation.lane)

  if (protocolOperationId !== undefined && protocolSessionId === undefined) {
    throw new TypeError('Protocol operation correlation requires a protocol session')
  }
  if (peerAttemptId !== undefined && peerPathId === undefined) {
    throw new TypeError('Peer attempt correlation requires a peer path')
  }
  if (
    protocolSessionId === undefined &&
    protocolOperationId === undefined &&
    peerPathId === undefined &&
    peerAttemptId === undefined &&
    lane === undefined
  ) {
    return undefined
  }

  const projected: {
    protocol_session_id?: string
    protocol_operation_id?: string
    peer_path_id?: string
    peer_attempt_id?: string
    lane_id?: number
    lane_epoch?: number
  } = {}
  if (protocolSessionId !== undefined) projected.protocol_session_id = protocolSessionId
  if (protocolOperationId !== undefined) projected.protocol_operation_id = protocolOperationId
  if (peerPathId !== undefined) projected.peer_path_id = peerPathId
  if (peerAttemptId !== undefined) projected.peer_attempt_id = peerAttemptId
  if (lane !== undefined) {
    projected.lane_id = lane.id
    // Epoch zero names the authenticated transcript lane and must not be
    // mistaken for omission by truthiness-based projection.
    projected.lane_epoch = lane.epoch
  }
  return Object.freeze(projected)
}

function projectIdentity<Kind extends FailureIdentityKind>(
  identity: FailureIdentity<Kind> | undefined,
  expectedKind: Kind,
): string | undefined {
  if (identity === undefined) return undefined
  if (
    identity.kind !== expectedKind ||
    identity.byteLength !== IDENTITY_BYTE_LENGTH ||
    typeof identity.copyBytes !== 'function'
  ) {
    throw new TypeError('Diagnostic identity type is invalid')
  }

  const bytes = identity.copyBytes()
  if (!(bytes instanceof Uint8Array) || bytes.byteLength !== IDENTITY_BYTE_LENGTH) {
    throw new TypeError('Diagnostic identities must contain exactly 16 bytes')
  }
  if (bytes.every((value) => value === 0)) return undefined
  return encodeBase64Url(bytes)
}

function validateLane(
  lane: FailureLaneCorrelation | undefined,
): FailureLaneCorrelation | undefined {
  if (lane === undefined) return undefined
  if (
    !Number.isInteger(lane.id) ||
    lane.id < 0 ||
    lane.id > UINT32_MAX ||
    !Number.isInteger(lane.epoch) ||
    lane.epoch < 0 ||
    lane.epoch > UINT32_MAX
  ) {
    throw new TypeError('Lane correlation must contain a uint32 ID and epoch')
  }
  return lane
}
