/** Browser quota is advisory; a successful reservation never promises a successful disk write. */
export interface ObjectGrowthRequest {
  readonly operationId: string
  readonly objectId: string
  readonly currentLength: bigint
  readonly targetLength: bigint
  readonly metadataHeadroom: bigint
}

export interface ObjectGrowthReservation {
  readonly reservationId: string
  settle(actualLength: bigint): Promise<void>
  release(): Promise<void>
}

export interface ObjectCapacityTraceEvent {
  readonly name: 'receive.capacity.reserved' | 'receive.capacity.rejected' |
    'receive.capacity.settled' | 'receive.capacity.released'
  readonly operation_id: string
  readonly object_id: string
  readonly reservation_id: string
  readonly lease_id: string
  readonly current_length?: bigint
  readonly target_length?: bigint
  readonly actual_length?: bigint
  readonly metadata_headroom?: bigint
  readonly failure_name?: string
}

export type ObjectCapacityTrace = (event: ObjectCapacityTraceEvent) => void

export interface ObjectCapacity {
  reserveGrowth(input: ObjectGrowthRequest): Promise<ObjectGrowthReservation>
}

export interface ObjectCapacityFence {
  readonly operationId: string
  readonly token: string
  readonly nowMilliseconds: number
}

export interface ObjectCapacityReservationRecord {
  readonly reservationId: string
  readonly targetLength: bigint
  readonly metadataHeadroom: bigint
}

export interface ObjectCapacityRecord {
  readonly id: string
  readonly operationId: string
  readonly objectId: string
  readonly token: string
  readonly occupiedBytes: bigint
  readonly reservations: readonly ObjectCapacityReservationRecord[]
}

export interface ObjectCapacityTotals {
  readonly occupiedBytes: bigint
  readonly outstandingGrowthBytes: bigint
  readonly metadataHeadroomBytes: bigint
}

export function objectCapacityTotals(record: ObjectCapacityRecord): ObjectCapacityTotals {
  let highWater = record.occupiedBytes
  let metadataHeadroomBytes = 0n
  for (const reservation of record.reservations) {
    if (reservation.targetLength > highWater) highWater = reservation.targetLength
    metadataHeadroomBytes += reservation.metadataHeadroom
  }
  return {
    occupiedBytes: record.occupiedBytes,
    outstandingGrowthBytes: highWater - record.occupiedBytes,
    metadataHeadroomBytes,
  }
}

export function requireCapacityLength(value: bigint): bigint {
  if (typeof value !== 'bigint' || value < 0n || value > 0xffff_ffff_ffff_ffffn) {
    throw new RangeError('Object capacity length is not a u64')
  }
  return value
}
