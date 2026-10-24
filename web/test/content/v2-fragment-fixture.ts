import { createHash } from 'node:crypto'

import {
  FRAGMENT_HEADER_BYTES,
  FRAGMENT_PAYLOAD_BYTES,
} from '../../src/content/flow'

export function fragmentRecord(
  operationId: Uint8Array,
  object: Uint8Array,
): Uint8Array<ArrayBuffer>[] {
  const count = Math.ceil(object.byteLength / FRAGMENT_PAYLOAD_BYTES)
  const recordId = createHash('sha256').update(object).digest().subarray(0, 16)
  return Array.from({ length: count }, (_, index) => {
    const start = index * FRAGMENT_PAYLOAD_BYTES
    const end = Math.min(start + FRAGMENT_PAYLOAD_BYTES, object.byteLength)
    const payload = object.subarray(start, end)
    const encoded = new Uint8Array(FRAGMENT_HEADER_BYTES + payload.byteLength)
    encoded[0] = 1
    encoded[1] = 8
    encoded[2] = index === count - 1 ? 1 : 0
    encoded.set(operationId, 4)
    encoded.set(recordId, 20)
    const view = new DataView(encoded.buffer)
    view.setUint32(36, index, false)
    view.setUint32(40, count, false)
    view.setUint32(44, object.byteLength, false)
    view.setUint32(48, payload.byteLength, false)
    encoded.set(payload, FRAGMENT_HEADER_BYTES)
    return encoded
  })
}
