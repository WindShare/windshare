import { decodeCanonicalCbor, requireArray, requireBytes, requireUnsigned } from '../protocol/cbor'
import { MESSAGE_KIND, type MessageKind } from './message'
import { OPERATION_CANCEL_REASON, type OperationCancelReason } from './runtime-types'

const LEASE_ID_BYTES = 16
const HEX_RADIX = 16
const HEX_BYTE_WIDTH = 2

export interface OperationRequestTrace {
  readonly leaseId: string
  readonly blocks?: Readonly<{ firstIndex: bigint; count: number }>
}

export type CancellationTraceReason = 'user' | 'superseded' | 'output_abort' | 'timeout' | 'lane_race'

export function cancellationTraceReason(reason: OperationCancelReason): CancellationTraceReason {
  switch (reason) {
    case OPERATION_CANCEL_REASON.user: return 'user'
    case OPERATION_CANCEL_REASON.superseded: return 'superseded'
    case OPERATION_CANCEL_REASON.outputAbort: return 'output_abort'
    case OPERATION_CANCEL_REASON.timeout: return 'timeout'
    case OPERATION_CANCEL_REASON.laneRace: return 'lane_race'
  }
}

/** Keep compact join keys, never a request body or a consumer's error object.
 * A missing diagnostic snapshot has no authority to reject protocol work.
 */
export function snapshotOperationRequest(kind: MessageKind, body: Uint8Array): OperationRequestTrace | undefined {
  if (kind !== MESSAGE_KIND.requestBlocks && kind !== MESSAGE_KIND.releaseLease &&
    kind !== MESSAGE_KIND.renewLease) return undefined
  try {
    const fields = requireArray(decodeCanonicalCbor(body, body.byteLength, 'trace request'),
      kind === MESSAGE_KIND.requestBlocks ? 2 : 1, 'trace request')
    const lease = requireBytes(fields[0], LEASE_ID_BYTES, 'trace lease', true)
    const leaseId = Array.from(lease, byte => byte.toString(HEX_RADIX).padStart(HEX_BYTE_WIDTH, '0')).join('')
    if (kind !== MESSAGE_KIND.requestBlocks) return Object.freeze({ leaseId })
    const indices = fields[1]
    if (!Array.isArray(indices) || indices.length === 0) return undefined
    return Object.freeze({ leaseId, blocks: Object.freeze({
      firstIndex: requireUnsigned(indices[0], 'trace first block'), count: indices.length,
    }) })
  } catch {
    return undefined
  }
}
