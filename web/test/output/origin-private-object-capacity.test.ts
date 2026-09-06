import { describe, expect, it } from 'vitest'
import {
  objectCapacityTotals,
  requireCapacityLength,
  type ObjectCapacityRecord,
} from '../../src/output/origin-private/object-capacity'

function object(occupiedBytes: bigint, targets: readonly bigint[]): ObjectCapacityRecord {
  return {
    id: 'task:archive', operationId: 'task', objectId: 'archive', token: 'owner',
    occupiedBytes,
    reservations: targets.map((targetLength, index) => ({
      reservationId: String(index), targetLength, metadataHeadroom: 16n,
    })),
  }
}

describe('OPFS high-water growth accounting', () => {
  it('pays the entire gap to a late entry rather than its payload size', () => {
    expect(objectCapacityTotals(object(100n, [1_000_000n]))).toEqual({
      occupiedBytes: 100n, outstandingGrowthBytes: 999_900n, metadataHeadroomBytes: 16n,
    })
  })

  it('shares overlapping growth reservations while keeping independent checkpoint headroom', () => {
    expect(objectCapacityTotals(object(100n, [600n, 500n, 50n]))).toEqual({
      occupiedBytes: 100n, outstandingGrowthBytes: 500n, metadataHeadroomBytes: 48n,
    })
    expect(objectCapacityTotals(object(600n, [500n, 50n]))).toEqual({
      occupiedBytes: 600n, outstandingGrowthBytes: 0n, metadataHeadroomBytes: 32n,
    })
  })

  it('retains exact bigint length arithmetic beyond both former workspace caps', () => {
    const length = 1n << 53n
    expect(objectCapacityTotals(object(length, [length + 31n])).outstandingGrowthBytes).toBe(31n)
    expect(() => requireCapacityLength(-1n)).toThrow()
    expect(() => requireCapacityLength(1n << 64n)).toThrow()
  })
})
