import assert from 'node:assert/strict'
import test from 'node:test'
import { V2BlockBroker } from '../../../../web/src/content/v2-broker'
import { V2LaneSet } from '../../../../web/src/content/v2-lane-set'
import { BlockReceiveWindow } from '../../../../web/src/content/scheduling/receive-window'
import { FileGeometry } from '../../../../web/src/content/geometry'
import { V2SessionRuntimeError } from '../../../../web/src/session/v2-runtime-types'

const MiB = 1024 * 1024
const flush = async () => { for (let n = 0; n < 100; n++) await Promise.resolve() }
const delay = ms => new Promise(resolve => setTimeout(resolve, ms))
const leaseId = new Uint8Array(16)
function descriptor(name, blocks = 1, chunk = MiB) {
  const size = BigInt(blocks * chunk)
  return { shareInstance: new Uint8Array(16), shareInstanceId: 'share',
    fileId: new Uint8Array(16), fileIdText: name, fileRevision: new Uint8Array(16),
    fileRevisionText: 'revision', exactSize: size, geometry: new FileGeometry(size, BigInt(chunk)) }
}
const demand = file => ({ descriptor: file, leaseId, localBlockIndex: 0n })
const routes = (...allowed) => ({ active: true, allows: route => allowed.includes(route), assertActive() {}, subscribe: () => () => {} })
const direct = routes('direct'), relay = routes('application-relay'), both = routes('direct', 'application-relay')
class Lane {
  calls = []
  admitted = []
  pending = new Map()
  aborts = []
  constructor(id, now, cleanup = async () => {}) {
    this.id = id
    this.window = new BlockReceiveWindow(now)
    this.cleanup = cleanup
  }
  async fetchBlock(demand, signal, observer) {
    const key = demand.descriptor.fileIdText + ':' + demand.localBlockIndex
    this.calls.push(key)
    const range = demand.descriptor.geometry.blockPlaintext(demand.localBlockIndex)
    const bytes = Number(range.end - range.start)
    const permit = await this.window.acquire(bytes, signal)
    this.admitted.push(key)
    observer?.admitted(permit.admission)
    try {
      return await new Promise((resolve, reject) => {
        const abort = () => {
          this.aborts.push(key)
          this.pending.delete(key)
          this.cleanup().then(() => reject(signal.reason), reject)
        }
        signal.addEventListener('abort', abort, { once: true })
        this.pending.set(key, {
          success: () => {
            signal.removeEventListener('abort', abort)
            this.pending.delete(key)
            permit.receive(bytes)
            observer?.received(bytes)
            resolve({ descriptor: demand.descriptor, localBlockIndex: demand.localBlockIndex, data: new Uint8Array(bytes).fill(7) })
          },
          fail: error => {
            signal.removeEventListener('abort', abort)
            this.pending.delete(key)
            reject(error)
          },
        })
        if (signal.aborted) abort()
      })
    } finally { permit.close() }
  }
  complete(name, block = 0n) {
    const item = this.pending.get(name + ':' + block)
    assert(item, 'Missing admitted block ' + name + ':' + block)
    item.success()
  }
  fail(name, error) { this.pending.get(name + ':0').fail(error) }
}

// These opt-in characterization probes deliberately expose current behavior;
// they are evidence for the plan, not assertions to preserve after refactoring.
for (const mode of ['rescue', 'retry']) {
  test(mode + ' enters a busy lane before physical admission', { timeout: 4000 }, async () => {
    let now = 0, widen = false
    const lanes = new V2LaneSet({ now: () => now })
    const fast = new Lane(1, () => now), busy = new Lane(2, () => now)
    lanes.add(fast, 'direct'); lanes.add(busy, 'application-relay')
    const broker = new V2BlockBroker(lanes, { maximumUpstreamReads: 2 })
    const blockerAbort = new AbortController(), victimAbort = new AbortController()
    const blocker = broker.readBlock(demand(descriptor('blocker')), { routes: relay, signal: blockerAbort.signal }).catch(() => {})
    const victimRoutes = { ...direct, allows: route => route === 'direct' || widen }
    const victim = broker.readBlock(demand(descriptor('victim')), { routes: victimRoutes, signal: victimAbort.signal }).catch(() => {})
    await flush()
    assert.deepEqual(fast.admitted, ['victim:0'])
    assert.deepEqual(busy.admitted, ['blocker:0'])
    widen = true
    if (mode === 'retry') fast.fail('victim', new V2SessionRuntimeError('lane', 'injected lane failure'))
    else now = 1000
    await delay(30); await flush()
    assert.deepEqual(busy.calls, ['blocker:0', 'victim:0'])
    assert.deepEqual(busy.admitted, ['blocker:0'])
    const preview = broker.readBlock(demand(descriptor('preview')), { routes: direct, priority: 'preview' })
    await flush()
    assert(!fast.calls.includes('preview:0'))
    victimAbort.abort()
    await victim; await flush()
    // A failed primary is retired; publish its replacement to exercise wake-up.
    if (mode === 'retry') { lanes.remove(1); lanes.add(fast, 'direct', 1); await flush() }
    assert(fast.calls.includes('preview:0'))
    assert.equal(busy.aborts.length, 0)
    fast.complete('preview'); await preview
    blockerAbort.abort(); await blocker
    await broker.waitForLeaseIdle(leaseId)
    assert.equal(busy.pending.size, 0)
    broker.close(); lanes.close()
  })
}

test('a successful rescue releases logical work before the losing attempt settles', { timeout: 4000 }, async () => {
  let now = 0, releaseCleanup
  const cleanup = new Promise(resolve => { releaseCleanup = resolve })
  const lanes = new V2LaneSet({ now: () => now })
  const slow = new Lane(1, () => now, () => cleanup), winner = new Lane(2, () => now)
  lanes.add(slow, 'direct')
  const broker = new V2BlockBroker(lanes, { maximumUpstreamReads: 1 })
  const first = broker.readBlock(demand(descriptor('first')), { routes: both })
  await flush()
  lanes.add(winner, 'application-relay')
  now = 1000
  await delay(30); await flush()
  winner.complete('first')
  await first; await flush()
  assert.equal(slow.aborts.length, 1)
  let settled = false
  const idle = broker.waitForLeaseIdle(leaseId).then(() => { settled = true })
  await flush()
  assert.equal(settled, false)
  const next = broker.readBlock(demand(descriptor('next')), { routes: relay })
  await flush()
  assert(winner.admitted.includes('next:0'))
  winner.complete('next'); await next
  releaseCleanup()
  await idle
  assert.equal(settled, true)
  broker.close(); lanes.close()
})

test('supported 4 MiB blocks can exhaust a paused range and exclude preview', { timeout: 4000 }, async () => {
  const lanes = new V2LaneSet(), lane = new Lane(1, () => 0)
  lanes.add(lane, 'direct')
  const broker = new V2BlockBroker(lanes, { maximumRangeBufferBytes: 8 * MiB, maximumUpstreamReads: 1 })
  const file = descriptor('bulk', 8, 4 * MiB), image = descriptor('preview', 1, 4 * MiB)
  const controller = new AbortController()
  const bulk = broker.readRouteAuthorizedRange(file, leaseId, { start: 0n, end: file.exactSize },
    { routes: direct, signal: controller.signal, maximumParallel: 1 })
  const first = bulk.next()
  await flush(); lane.complete('bulk'); await first
  await flush(); lane.complete('bulk', 1n)
  await flush(); lane.complete('bulk', 2n)
  await flush()
  const preview = broker.readRouteAuthorizedRange(image, leaseId, { start: 0n, end: image.exactSize },
    { routes: direct, priority: 'preview' })
  const next = preview.next()
  await flush()
  assert(!lane.calls.includes('preview:0'))
  controller.abort()
  await bulk.return()
  await flush()
  assert(lane.calls.includes('preview:0'))
  lane.complete('preview')
  assert.equal((await next).value.data.byteLength, 4 * MiB)
  await preview.return()
  await broker.waitForLeaseIdle(leaseId)
  broker.close(); lanes.close()
})
