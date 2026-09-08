import { describe, expect, it, vi } from 'vitest'
import {
  writeObjectBatch, type ObjectWriteBatchCapacity,
} from '../../src/output/origin-private/native-object/write-batch'

const object = { operationId: 'task', objectId: 'archive' }
const writes = [
  { offset: 0n, bytes: new Uint8Array([1, 2]) },
  { offset: 8n, bytes: new Uint8Array([3, 4]) },
  { offset: 1000n, bytes: new Uint8Array() },
]

function fixture() {
  const events: string[] = []
  const io = {
    size: vi.fn(async () => 4n),
    writeAt: vi.fn(async () => { events.push('write') }),
  }
  const reservation = {
    settle: vi.fn(async () => { events.push('settle') }),
    release: vi.fn(async () => { events.push('release') }),
  }
  const capacity: ObjectWriteBatchCapacity = {
    reserveGrowth: vi.fn(async () => { events.push('reserve'); return reservation }),
  }
  return { io, capacity, reservation, events }
}

describe('native object write batch capacity', () => {
  it('holds one durable reservation across sparse growth and existing regions, ignoring empty writes', async () => {
    const f = fixture()
    expect(await writeObjectBatch(f.io, { object, capacity: f.capacity, writes })).toBe(10n)
    expect(f.capacity.reserveGrowth).toHaveBeenCalledExactlyOnceWith({
      ...object, currentLength: 4n, targetLength: 10n, metadataHeadroom: 0n,
    })
    expect(f.io.size).toHaveBeenCalledTimes(1)
    expect(f.io.writeAt).toHaveBeenCalledTimes(2)
    expect(f.reservation.settle).toHaveBeenCalledExactlyOnceWith(10n)
    expect(f.events).toEqual(['reserve', 'write', 'write', 'settle', 'release'])
  })

  it('does not shrink occupancy when every write stays inside the object', async () => {
    const f = fixture()
    expect(await writeObjectBatch(f.io, { object, capacity: f.capacity, writes: writes.slice(0, 1) })).toBe(4n)
    expect(f.reservation.settle).toHaveBeenCalledWith(4n)
  })

  it('keeps a conservative charge after a native write fails partway through the batch', async () => {
    const f = fixture()
    const failure = new DOMException('partial native extension', 'UnknownError')
    f.io.writeAt.mockRejectedValueOnce(failure)
    await expect(writeObjectBatch(f.io, { object, capacity: f.capacity, writes })).rejects.toBe(failure)
    expect(f.io.writeAt).toHaveBeenCalledTimes(1)
    expect(f.reservation.settle).toHaveBeenCalledExactlyOnceWith(10n)
    expect(f.events).toEqual(['reserve', 'settle', 'release'])
  })

  it('leaves the reservation outstanding if durable accounting cannot settle the native result', async () => {
    const f = fixture()
    const failure = new Error('database unavailable')
    f.reservation.settle.mockRejectedValue(failure)
    await expect(writeObjectBatch(f.io, { object, capacity: f.capacity, writes })).rejects.toBe(failure)
    expect(f.io.writeAt).toHaveBeenCalledTimes(2)
    expect(f.reservation.release).not.toHaveBeenCalled()
  })

  it('does not touch the object when growth admission is declined', async () => {
    const f = fixture()
    const failure = new DOMException('quota pressure', 'QuotaExceededError')
    f.capacity.reserveGrowth = vi.fn(async () => { throw failure })
    await expect(writeObjectBatch(f.io, { object, capacity: f.capacity, writes })).rejects.toBe(failure)
    expect(f.io.writeAt).not.toHaveBeenCalled()
    expect(f.reservation.settle).not.toHaveBeenCalled()
  })

  it.each([-1n, 0xffff_ffff_ffff_ffffn])('validates offset %s before acquiring capacity', async offset => {
    const f = fixture()
    await expect(writeObjectBatch(f.io, {
      object, capacity: f.capacity, writes: [{ offset, bytes: new Uint8Array(2) }],
    })).rejects.toThrow(RangeError)
    expect(f.capacity.reserveGrowth).not.toHaveBeenCalled()
    expect(f.io.writeAt).not.toHaveBeenCalled()
  })

  it('does not acquire capacity or extend the object for an empty batch', async () => {
    const f = fixture()
    expect(await writeObjectBatch(f.io, {
      object, capacity: f.capacity, writes: [writes[2]!],
    })).toBe(4n)
    expect(f.capacity.reserveGrowth).not.toHaveBeenCalled()
  })
})
