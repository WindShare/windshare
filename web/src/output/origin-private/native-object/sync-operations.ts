import { bigintToSafeNumber } from '../../../content/geometry'
import type { NativeSyncHandle } from './contracts'

export function writeNativeBytes(handle: Pick<NativeSyncHandle, 'write'>, offset: bigint, bytes: Uint8Array): void {
  bigintToSafeNumber(offset + BigInt(bytes.byteLength), 'native output end')
  const start = bigintToSafeNumber(offset, 'native output offset')
  let written = 0
  while (written < bytes.byteLength) {
    const count = handle.write(bytes.subarray(written), { at: start + written })
    if (!Number.isSafeInteger(count) || count <= 0 || count > bytes.byteLength - written) {
      throw new DOMException('Native output write made invalid progress', 'OperationError')
    }
    written += count
  }
}

export function truncateNativeObject(handle: NativeSyncHandle, length: bigint): void {
  handle.truncate(bigintToSafeNumber(length, 'native output length'))
}
