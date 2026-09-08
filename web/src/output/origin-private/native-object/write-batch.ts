import { requireCapacityLength, type ObjectGrowthRequest, type ObjectGrowthReservation } from '../object-capacity'
import type { NativeObjectIO } from './contracts'

export interface NativeObjectWrite {
  readonly offset: bigint
  readonly bytes: Uint8Array
}

export interface ObjectWriteBatchCapacity {
  reserveGrowth(input: ObjectGrowthRequest): Promise<Pick<ObjectGrowthReservation, 'settle' | 'release'>>
}

/**
 * The caller holds the object's mutation queue for the entire batch. One reservation
 * covers both existing regions and growth, without weakening the durable capacity fence.
 */
export async function writeObjectBatch(
  io: Pick<NativeObjectIO, 'size' | 'writeAt'>,
  input: {
    readonly object: Readonly<{ operationId: string; objectId: string }>
    readonly capacity: ObjectWriteBatchCapacity
    readonly writes: readonly NativeObjectWrite[]
  },
): Promise<bigint> {
  const currentLength = requireCapacityLength(await io.size())
  let occupiedBound = currentLength
  for (const write of input.writes) {
    requireCapacityLength(write.offset)
    const end = requireCapacityLength(write.offset + BigInt(write.bytes.byteLength))
    if (write.bytes.byteLength > 0 && end > occupiedBound) occupiedBound = end
  }
  if (!input.writes.some(write => write.bytes.byteLength > 0)) return currentLength
  const reservation = await input.capacity.reserveGrowth({
    operationId: input.object.operationId, objectId: input.object.objectId,
    currentLength, targetLength: occupiedBound, metadataHeadroom: 0n,
  })
  let settled = false
  try {
    for (const write of input.writes) {
      if (write.bytes.byteLength > 0) await io.writeAt(write.offset, write.bytes)
    }
    await reservation.settle(occupiedBound)
    settled = true
    return occupiedBound
  } catch (error) {
    // A failed write may have extended the object. Recovery measures it before reclaiming
    // this conservative charge; a failed settlement keeps the reservation outstanding.
    try {
      await reservation.settle(occupiedBound)
      settled = true
    } catch { /* Retain the capacity fence until physical recovery. */ }
    throw error
  } finally {
    if (settled) await reservation.release()
  }
}
