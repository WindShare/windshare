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
  readonly receive_operation_id?: string
  readonly transfer_job_id?: string
  readonly output_session_id?: string
  readonly protocol_generation?: number
}

export interface LocalOutputCorrelationInputV1 {
  readonly receiveOperationId: string
  readonly transferJobId: string
  readonly outputSessionId: string
  readonly protocolSessionIdentity?: FailureIdentity<'protocol_session'>
  readonly protocolGeneration?: number
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

/**
 * Local output correlation deliberately excludes artifact paths. Protocol identity
 * is shared with the ordinary incident vocabulary, while receive/output identities
 * remain diagnostic-only and never enter protocol trace payloads.
 */
export function projectLocalOutputCorrelationV1(
  input: LocalOutputCorrelationInputV1,
): CorrelationV1 {
  const receiveOperationId = correlationText(
    input.receiveOperationId,
    'receive operation identity',
  )
  const transferJobId = correlationText(input.transferJobId, 'transfer job identity')
  const outputSessionId = correlationText(input.outputSessionId, 'output session identity')
  const protocolSessionId = projectIdentity(input.protocolSessionIdentity, 'protocol_session')
  if ((protocolSessionId === undefined) !== (input.protocolGeneration === undefined)) {
    throw new TypeError('protocol generation and session identity must be captured together')
  }
  if (input.protocolGeneration !== undefined && (
    !Number.isSafeInteger(input.protocolGeneration) ||
    input.protocolGeneration <= 0 ||
    input.protocolGeneration > UINT32_MAX
  )) {
    throw new RangeError('protocol generation must be a positive uint32')
  }
  return Object.freeze({
    receive_operation_id: receiveOperationId,
    transfer_job_id: transferJobId,
    output_session_id: outputSessionId,
    ...(protocolSessionId === undefined
      ? {}
      : {
          protocol_session_id: protocolSessionId,
          protocol_generation: input.protocolGeneration,
        }),
  })
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

function correlationText(value: string, field: string): string {
  if (typeof value !== 'string' || value.length === 0) {
    throw new TypeError(`${field} must be non-empty text`)
  }
  return value
}
