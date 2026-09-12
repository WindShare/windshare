import { decodeCanonicalCbor, requireArray, requireBytes, requireUnsigned } from '../protocol/cbor'
import { V2_MESSAGE_KIND, type V2MessageKind } from './v2-message'
import { V2_OPERATION_CANCEL_REASON, type V2OperationCancelReason } from './v2-runtime-types'

const LEASE_ID_BYTES = 16
const HEX_RADIX = 16
const HEX_BYTE_WIDTH = 2

export interface V2OperationRequestTrace {
  readonly leaseId: string
  readonly blocks?: Readonly<{ firstIndex: bigint; count: number }>
}

export type V2CancellationTraceReason = 'user' | 'superseded' | 'output_abort' | 'timeout' | 'lane_race'

export function cancellationTraceReason(reason: V2OperationCancelReason): V2CancellationTraceReason {
  switch (reason) {
    case V2_OPERATION_CANCEL_REASON.user: return 'user'
    case V2_OPERATION_CANCEL_REASON.superseded: return 'superseded'
    case V2_OPERATION_CANCEL_REASON.outputAbort: return 'output_abort'
    case V2_OPERATION_CANCEL_REASON.timeout: return 'timeout'
    case V2_OPERATION_CANCEL_REASON.laneRace: return 'lane_race'
  }
}

/** Keep compact join keys, never a request body or a consumer's error object.
 * A missing diagnostic snapshot has no authority to reject protocol work.
 */
export function snapshotOperationRequest(kind: V2MessageKind, body: Uint8Array): V2OperationRequestTrace | undefined {
  if (kind !== V2_MESSAGE_KIND.requestBlocks && kind !== V2_MESSAGE_KIND.releaseLease &&
    kind !== V2_MESSAGE_KIND.renewLease) return undefined
  try {
    const fields = requireArray(decodeCanonicalCbor(body, body.byteLength, 'trace request'),
      kind === V2_MESSAGE_KIND.requestBlocks ? 2 : 1, 'trace request')
    const lease = requireBytes(fields[0], LEASE_ID_BYTES, 'trace lease', true)
    const leaseId = Array.from(lease, byte => byte.toString(HEX_RADIX).padStart(HEX_BYTE_WIDTH, '0')).join('')
    if (kind !== V2_MESSAGE_KIND.requestBlocks) return Object.freeze({ leaseId })
    const indices = fields[1]
    if (!Array.isArray(indices) || indices.length === 0) return undefined
    return Object.freeze({ leaseId, blocks: Object.freeze({
      firstIndex: requireUnsigned(indices[0], 'trace first block'), count: indices.length,
    }) })
  } catch {
    return undefined
  }
}
