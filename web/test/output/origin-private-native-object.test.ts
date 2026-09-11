import { describe, expect, it, vi } from 'vitest'
import { NativeObjectWorkerClient, NATIVE_QUEUE_MAX_BYTES, NATIVE_WRITE_CHUNK_BYTES, type NativeWorkerPort } from '../../src/output/origin-private/native-object/client'
import { ObjectCheckpointCoordinator } from '../../src/output/origin-private/native-object/coordinator'
import type { NativeObjectIO, NativeReply, NativeRequest } from '../../src/output/origin-private/native-object/contracts'
import { writeNativeBytes } from '../../src/output/origin-private/native-object/sync-operations'

describe('native OPFS worker writes', () => {
  it('advances both offset and source view across short writes', () => {
    const observed: Array<{ at: number; bytes: number[] }> = []
    const write = vi.fn((bytes: Uint8Array, options: { at: number }) => {
      observed.push({ at: options.at, bytes: [...bytes] })
      return Math.min(2, bytes.length)
    })
    writeNativeBytes({ write }, 7n, Uint8Array.of(10, 11, 12, 13, 14))
    expect(observed).toEqual([
      { at: 7, bytes: [10, 11, 12, 13, 14] },
      { at: 9, bytes: [12, 13, 14] },
      { at: 11, bytes: [14] },
    ])
  })

  it.each([0, -1, 4, Number.NaN, 0.5])('rejects invalid progress %s without spinning', count => {
    const write = vi.fn(() => count)
    expect(() => writeNativeBytes({ write }, 0n, Uint8Array.of(1, 2, 3))).toThrow('invalid progress')
    expect(write).toHaveBeenCalledOnce()
  })

  it('checks safe offsets at the API boundary before any mutation', () => {
    const write = vi.fn()
    expect(() => writeNativeBytes({ write }, BigInt(Number.MAX_SAFE_INTEGER), Uint8Array.of(1))).toThrow('safely')
    expect(write).not.toHaveBeenCalled()
  })

  it('applies byte backpressure before copying and dispatching the next worker message', async () => {
    const worker = new ControlledWorker()
    const client = new NativeObjectWorkerClient(worker)
    const capacity = NATIVE_QUEUE_MAX_BYTES / NATIVE_WRITE_CHUNK_BYTES
    const writes = Array.from({ length: capacity + 1 }, (_, index) =>
      client.writeAt(BigInt(index * NATIVE_WRITE_CHUNK_BYTES), new Uint8Array(NATIVE_WRITE_CHUNK_BYTES)))
    await Promise.resolve()
    expect(worker.requests).toHaveLength(capacity)
    worker.reply(worker.requests[0]!.id)
    await Promise.resolve()
    expect(worker.requests).toHaveLength(capacity + 1)
    for (const request of worker.requests.slice(1)) worker.reply(request.id)
    await Promise.all(writes)
  })

  it('rejects every pending and waiting request after a worker failure', async () => {
    const worker = new ControlledWorker()
    const client = new NativeObjectWorkerClient(worker)
    const operations = Array.from({ length: 34 }, () => client.writeAt(0n, new Uint8Array(NATIVE_WRITE_CHUNK_BYTES)))
    const results = Promise.allSettled(operations)
    await Promise.resolve()
    worker.onerror?.({ message: 'worker crashed' } as ErrorEvent)
    expect((await results).every(result => result.status === 'rejected')).toBe(true)
    await expect(client.flush()).rejects.toThrow('worker crashed')
    expect(worker.terminate).toHaveBeenCalledOnce()
  })

  it('does not accept a block whose later chunk fails', async () => {
    const worker = new ControlledWorker()
    const client = new NativeObjectWorkerClient(worker)
    const write = client.writeAt(0n, new Uint8Array(NATIVE_WRITE_CHUNK_BYTES + 1))
    const failure = expect(write).rejects.toThrow('disk full')
    await Promise.resolve()
    worker.reply(worker.requests[0]!.id)
    await vi.waitFor(() => expect(worker.requests).toHaveLength(2))
    const last = worker.requests.at(-1)!
    worker.onmessage?.({ data: { id: last.id, ok: false, name: 'QuotaExceededError', message: 'disk full' } } as MessageEvent<NativeReply>)
    await failure
  })
})

describe('one native object checkpoint boundary', () => {
  it('coalesces shared-member cuts after queued writes while retaining one open native handle', async () => {
    const events: string[] = []
    const coordinator = new ObjectCheckpointCoordinator({
      io: fakeIO(events), operationId: 'task', objectId: 'shared-archive',
    })
    let dirty = false
    let generation = 0
    const checkpoint = () => coordinator.checkpointIfChanged('members-due', () => dirty,
      () => generation, async () => {
        events.push('metadata')
        dirty = false
        return ++generation
      })
    const write = coordinator.mutate(async writer => {
      await writer.writeAt(0n, Uint8Array.of(1))
      dirty = true
    })
    const first = checkpoint()
    const second = checkpoint()
    await write
    expect(await Promise.all([first, second])).toEqual([1, 1])
    expect(events).toEqual(['write:0', 'flush', 'metadata'])
    await coordinator.mutate(async writer => {
      await writer.writeAt(1n, Uint8Array.of(2))
      dirty = true
    })
    expect(await checkpoint()).toBe(2)
    expect(events).toEqual(['write:0', 'flush', 'metadata', 'write:1', 'flush', 'metadata'])
    await coordinator.close()
    expect(events.at(-1)).toBe('close')
  })

  it('holds later writes until the covered writes, flush and atomic commit complete', async () => {
    const events: string[] = []
    let release!: () => void
    const gate = new Promise<void>(resolve => { release = resolve })
    const io = fakeIO(events)
    const coordinator = new ObjectCheckpointCoordinator({ io, operationId: 'task', objectId: 'zip' })
    const first = coordinator.mutate(async writer => {
      await writer.writeAt(0n, Uint8Array.of(1))
      events.push('range-accepted')
    })
    const cut = coordinator.checkpoint('automatic', async () => {
      events.push('commit-start')
      await gate
      events.push('commit-end')
      return 1
    })
    const second = coordinator.writeAt(1n, Uint8Array.of(2))
    await first
    await vi.waitFor(() => expect(events).toContain('commit-start'))
    expect(events).toEqual(['write:0', 'range-accepted', 'flush', 'commit-start'])
    release()
    expect(await cut).toBe(1)
    await second
    expect(events.at(-1)).toBe('write:1')
  })

  it.each(['write', 'flush', 'commit'])('latches %s failure and retains the prior committed authority', async failureStage => {
    const events: string[] = []
    const io = fakeIO(events)
    const coordinator = new ObjectCheckpointCoordinator({ io, operationId: 'task', objectId: 'zip' })
    let committed = 1
    if (failureStage === 'write') io.writeAt = async () => { throw new Error('write failed') }
    if (failureStage === 'flush') io.flush = async () => { throw new Error('flush failed') }
    const operation = failureStage === 'write'
      ? coordinator.writeAt(0n, Uint8Array.of(1))
      : coordinator.checkpoint('automatic', async () => {
        if (failureStage === 'commit') throw new Error('commit failed')
        committed = 2
      })
    const later = coordinator.writeAt(1n, Uint8Array.of(2))
    const results = await Promise.allSettled([operation, later])
    expect(results.every(result => result.status === 'rejected')).toBe(true)
    expect(committed).toBe(1)
    expect(events).toContain('close')
    expect(events).not.toContain('write:1')
  })
})

class ControlledWorker implements NativeWorkerPort {
  readonly requests: NativeRequest[] = []
  readonly terminate = vi.fn()
  onmessage: NativeWorkerPort['onmessage'] = null
  onerror: NativeWorkerPort['onerror'] = null
  onmessageerror: NativeWorkerPort['onmessageerror'] = null
  postMessage(request: NativeRequest): void { this.requests.push(request) }
  reply(id: number): void { this.onmessage?.({ data: { id, ok: true } } as MessageEvent<NativeReply>) }
}

function fakeIO(events: string[]): NativeObjectIO {
  return {
    writeAt: async offset => { events.push(`write:${offset}`) },
    truncate: async () => undefined,
    size: async () => 0n,
    flush: async () => { events.push('flush') },
    close: async () => { events.push('close') },
  }
}
