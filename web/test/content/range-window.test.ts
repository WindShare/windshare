import { describe, expect, it } from 'vitest'
import { BlockBroker } from '../../src/content/broker'
import { LaneSet, type BlockDemand, type BlockReadObserver } from '../../src/content/lane-set'
import { FileGeometry } from '../../src/content/geometry'
import type { BlockRecord, FileRevisionDescriptor } from '../../src/content/records'
import type { BlockRouteEligibility } from '../../src/content/route-policy'

const BLOCK_BYTES = 1024
const BLOCKS = 16
const routes: BlockRouteEligibility = {
  active: true, allows: () => true, assertActive: () => undefined, subscribe: () => () => undefined,
}
const descriptor: FileRevisionDescriptor = {
  shareInstance: new Uint8Array(16), shareInstanceId: 'share', fileId: new Uint8Array(16), fileIdText: 'file',
  fileRevision: new Uint8Array(16), fileRevisionText: 'revision', exactSize: BigInt(BLOCK_BYTES * BLOCKS),
  geometry: new FileGeometry(BigInt(BLOCK_BYTES * BLOCKS), BigInt(BLOCK_BYTES)),
}
class ControlledLane {
  readonly id = 1
  readonly calls: BlockDemand[] = []
  readonly active = new Map<string, () => void>()
  fetchBlock(input: BlockDemand, signal: AbortSignal, observer?: BlockReadObserver): Promise<BlockRecord> {
    this.calls.push(input)
    observer?.received(BLOCK_BYTES)
    const key = this.key(input.descriptor.fileIdText, input.localBlockIndex)
    return new Promise((resolve, reject) => {
      const abort = () => { this.active.delete(key); reject(signal.reason) }
      signal.addEventListener('abort', abort, { once: true })
      this.active.set(key, () => {
        signal.removeEventListener('abort', abort)
        this.active.delete(key)
        resolve({ descriptor: input.descriptor, localBlockIndex: input.localBlockIndex, data: new Uint8Array(BLOCK_BYTES) })
      })
    })
  }
  complete(index: bigint, file = 'file'): void {
    const finish = this.active.get(this.key(file, index))
    if (finish === undefined) throw new Error('Expected an active block')
    finish()
  }
  key(file: string, index: bigint): string { return file + ':' + index }
}
async function microtasks(): Promise<void> {
  for (let turn = 0; turn < 16; turn += 1) await Promise.resolve()
}
function setup(bufferBlocks: number) {
  const lane = new ControlledLane()
  const lanes = new LaneSet()
  lanes.add(lane, 'direct')
  const broker = new BlockBroker(lanes, { maximumRangeBufferBytes: BLOCK_BYTES * bufferBlocks })
  return { lane, lanes, broker }
}
function range(broker: BlockBroker, signal: AbortSignal, file = descriptor) {
  return broker.readRouteAuthorizedRange(file, new Uint8Array(16), { start: 0n, end: file.exactSize },
    { routes, signal, maximumParallel: 2 })
}

describe('bounded ordered block delivery', () => {
  it('keeps later cached blocks from taking the pending output frontier capacity', async () => {
    const { lane, lanes, broker } = setup(2)
    const cached = broker.readBlock({ descriptor, leaseId: new Uint8Array(16), localBlockIndex: 1n }, { routes })
    await microtasks()
    lane.complete(1n)
    await cached
    const controller = new AbortController()
    const iterator = range(broker, controller.signal)
    const first = iterator.next()
    await microtasks()
    expect(lane.active.has(lane.key('file', 0n))).toBe(true)
    lane.complete(0n)
    expect((await first).value?.offset).toBe(0n)
    expect((await iterator.next()).value?.offset).toBe(BigInt(BLOCK_BYTES))
    expect(lane.calls.filter(call => call.localBlockIndex === 1n)).toHaveLength(1)
    controller.abort()
    await iterator.return(undefined)
    broker.close(); lanes.close()
  })

  it('refills completed network slots while the output frontier waits, then stops at the byte budget', async () => {
    const { lane, lanes, broker } = setup(8)
    const controller = new AbortController()
    const iterator = range(broker, controller.signal)
    const first = iterator.next()
    await microtasks()
    expect(lane.calls.map(value => value.localBlockIndex)).toEqual([0n, 1n])
    expect(lanes.schedulingDemand()).toMatchObject({ networkWaitingBytes: BLOCK_BYTES, outputBytes: 0 })
    lane.complete(1n)
    await microtasks()
    expect(lane.calls.map(value => value.localBlockIndex)).toEqual([0n, 1n, 2n])
    lane.complete(2n)
    await microtasks()
    lane.complete(3n)
    await microtasks()
    expect(lane.calls).toHaveLength(4)
    expect(lanes.schedulingDemand()).toMatchObject({ networkWaitingBytes: 0, outputBytes: 0 })
    expect(lanes.schedulingDemand().capacityWaitingBytes).toBeGreaterThan(0)
    lane.complete(0n)
    expect((await first).value?.offset).toBe(0n)
    controller.abort()
    await iterator.return(undefined)
    expect(lane.active.size).toBe(0)
    broker.close(); lanes.close()
  })

  it('gives a newer range a budget turn instead of starving it behind a large download', async () => {
    const { lane, lanes, broker } = setup(2)
    const firstController = new AbortController()
    const secondController = new AbortController()
    const first = range(broker, firstController.signal)
    const firstRead = first.next()
    await microtasks()
    const secondDescriptor = { ...descriptor, fileIdText: 'second', fileRevisionText: 'second-revision' }
    const second = range(broker, secondController.signal, secondDescriptor)
    const secondRead = second.next()
    await microtasks()
    expect(lane.calls.every(value => value.descriptor.fileIdText === 'file')).toBe(true)
    lane.complete(0n)
    await firstRead
    const following = first.next().catch(() => undefined)
    await microtasks()
    expect(lane.active.has(lane.key('second', 0n))).toBe(true)
    lane.complete(0n, 'second')
    expect((await secondRead).value?.offset).toBe(0n)
    firstController.abort(); secondController.abort()
    await following
    await Promise.all([first.return(undefined), second.return(undefined)])
    broker.close(); lanes.close()
  })

  it('releases reserved read-ahead after cancellation so a following range can start', async () => {
    const { lane, lanes, broker } = setup(2)
    const controller = new AbortController()
    const first = range(broker, controller.signal)
    const pending = first.next()
    const rejection = expect(pending).rejects.toMatchObject({ name: 'AbortError' })
    await microtasks()
    controller.abort()
    await rejection
    expect(lane.active.size).toBe(0)
    const nextController = new AbortController()
    const next = range(broker, nextController.signal)
    const nextRead = next.next()
    await microtasks()
    lane.complete(0n)
    expect((await nextRead).value?.offset).toBe(0n)
    nextController.abort()
    await next.return(undefined)
    broker.close(); lanes.close()
  })

  it('closes ranges waiting for buffer space even when another consumer has paused output', async () => {
    const { lane, lanes, broker } = setup(2)
    const first = range(broker, new AbortController().signal)
    const firstRead = first.next()
    await microtasks()
    lane.complete(0n)
    await firstRead
    await microtasks()
    const second = range(broker, new AbortController().signal, { ...descriptor, fileIdText: 'second' })
    const waiting = expect(second.next()).rejects.toThrow('Block broker closed')
    await microtasks()
    expect(lane.calls.every(value => value.descriptor.fileIdText === 'file')).toBe(true)
    broker.close()
    await waiting
    await first.return(undefined)
    expect(lane.active.size).toBe(0)
    lanes.close()
  })

  it('rejects a block larger than its entire buffer budget without starting upstream work', async () => {
    const { lane, lanes, broker } = setup(1)
    const larger = { ...descriptor, geometry: new FileGeometry(descriptor.exactSize, BigInt(BLOCK_BYTES * 2)) }
    const iterator = range(broker, new AbortController().signal, larger)
    await expect(iterator.next()).rejects.toThrow('Block exceeds the range buffer budget')
    expect(lane.calls).toEqual([])
    broker.close(); lanes.close()
  })
})
