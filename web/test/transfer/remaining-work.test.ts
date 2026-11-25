import { describe, expect, it } from 'vitest'
import { SelectionPolicy } from '../../src/catalog/selection'
import { ConnectivityRouteAuthority } from '../../src/connectivity/receiver-policy'
import { TURNDeliveryPressure } from '../../src/connectivity/turn-demand'
import {
  BlockBroker,
  LaneSet,
  type BlockLane,
  type BlockRangeReader,
  type BlockRangeReaderOptions,
} from '../../src/content/broker'
import { FileGeometry } from '../../src/content/geometry'
import type { FileRevisionDescriptor } from '../../src/content/records'
import type { RevisionReader } from '../../src/content/session-services'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { TransferJob } from '../../src/transfer/job'
import { gate, id, makeShareFixture, outputFile } from './share-fixture'

const selection = new SelectionPolicy(true).snapshot()
const MiB = 1024 * 1024
const ONE_AND_HALF_MIB = 1_536 * 1024
const BLOCK_SIZE_BYTES = 2n * 1024n * 1024n

describe('transfer job remaining-work observation', () => {
  it('shares stable per-job callbacks across concurrent files while summing distinct jobs', async () => {
    const firstShare = makeShareFixture([
      { seed: 10, name: 'a.bin', bytes: new Uint8Array(16) },
      { seed: 11, name: 'b.bin', bytes: new Uint8Array(16) },
    ])
    const secondShare = makeShareFixture([
      { seed: 12, name: 'c.bin', bytes: new Uint8Array(24) },
    ])

    const lanes = new LaneSet()
    const firstCalls: BlockRangeReaderOptions[] = []
    const secondCalls: BlockRangeReaderOptions[] = []
    const allActive = gate()
    const release = gate()
    const firstDiscoveryDone = gate()
    const secondDiscoveryDone = gate()

    const observeBroker = (
      calls: BlockRangeReaderOptions[],
      expectedTotal: number,
    ): BlockRangeReader => ({
      readRange: async function* (_descriptor, _leaseId, span, options = {}) {
        calls.push(options)
        const stop = options.remainingWork === undefined
          ? undefined
          : lanes.observeRemainingWork(options.remainingWork)
        try {
          if (firstCalls.length + secondCalls.length === expectedTotal) allActive.resolve()
          await release.promise
          yield { offset: span.start, data: new Uint8Array(Number(span.end - span.start)) }
        } finally {
          stop?.()
        }
      },
    })

    const firstJob = new TransferJob({
      descriptor: firstShare.descriptor,
      catalog: firstShare.catalog,
      selection,
      revisions: firstShare.revisions,
      broker: observeBroker(firstCalls, 3),
      output: {
        sessionId: 'job-1',
        capabilities: { maxConcurrentFiles: 2, resumable: true },
        openFile: async () => outputFile({}),
      },
      onProgress: progress => {
        if (progress.discovery === 'complete') firstDiscoveryDone.resolve()
      },
    })
    const secondJob = new TransferJob({
      descriptor: secondShare.descriptor,
      catalog: secondShare.catalog,
      selection,
      revisions: secondShare.revisions,
      broker: observeBroker(secondCalls, 3),
      output: {
        sessionId: 'job-2',
        capabilities: { maxConcurrentFiles: 2, resumable: true },
        openFile: async () => outputFile({}),
      },
      onProgress: progress => {
        if (progress.discovery === 'complete') secondDiscoveryDone.resolve()
      },
    })

    const runs = Promise.all([firstJob.run(), secondJob.run()])
    await Promise.all([allActive.promise, firstDiscoveryDone.promise, secondDiscoveryDone.promise])

    expect(firstCalls).toHaveLength(2)
    expect(firstCalls[0]?.remainingWork).toBeDefined()
    expect(firstCalls[0]?.remainingWork).toBe(firstCalls[1]?.remainingWork)
    expect(firstCalls[0]?.onReceive).toBeDefined()
    expect(firstCalls[0]?.onReceive).toBe(firstCalls[1]?.onReceive)
    expect(secondCalls).toHaveLength(1)
    expect(secondCalls[0]?.remainingWork).not.toBe(firstCalls[0]?.remainingWork)
    expect(lanes.schedulingDemand()).toMatchObject({
      remainingBytes: 32 + 24,
      remainingKnown: true,
    })

    release.resolve()
    const [firstResult, secondResult] = await runs
    expect(firstResult.status).toBe('completed')
    expect(secondResult.status).toBe('completed')
    expect(lanes.schedulingDemand()).toMatchObject({ remainingBytes: 0, remainingKnown: false })
    lanes.close()
  })

  it('reports 3 MiB without triggering parallel TURN when two 1.5 MiB files are both in flight', async () => {
    const files = [
      { seed: 20, name: 'first.bin', bytes: new Uint8Array(ONE_AND_HALF_MIB) },
      { seed: 21, name: 'second.bin', bytes: new Uint8Array(ONE_AND_HALF_MIB) },
    ] as const
    const share = makeShareFixture(files)
    const byId = new Map(files.map(file => [encodeBase64Url(id(file.seed)), file]))

    const revisions: RevisionReader = {
      open: async fileId => {
        const key = encodeBase64Url(fileId)
        const file = byId.get(key)!
        const rev = id(file.seed + 50)
        const descriptor: FileRevisionDescriptor = Object.freeze({
          shareInstance: share.descriptor.shareInstance,
          shareInstanceId: share.descriptor.shareInstanceId,
          fileId: id(file.seed),
          fileIdText: key,
          fileRevision: rev,
          fileRevisionText: encodeBase64Url(rev),
          exactSize: BigInt(file.bytes.byteLength),
          geometry: new FileGeometry(BigInt(file.bytes.byteLength), BLOCK_SIZE_BYTES),
        })
        return Object.freeze({ descriptor, leaseId: id(file.seed + 80), release: async () => undefined })
      },
    }

    const lanes = new LaneSet()
    const broker = new BlockBroker(lanes)
    const routes = new ConnectivityRouteAuthority('auto')
    const bothInFlight = gate()
    const deliver = gate()
    const discoveryDone = gate()
    let startedFetches = 0

    const makeLane = (laneId: number): BlockLane => ({
      id: laneId,
      fetchBlock: async (demand, signal) => {
        startedFetches += 1
        if (startedFetches === 2) bothInFlight.resolve()
        await deliver.promise
        signal.throwIfAborted()
        const file = byId.get(demand.descriptor.fileIdText)!
        return Object.freeze({
          descriptor: demand.descriptor,
          localBlockIndex: demand.localBlockIndex,
          data: file.bytes.slice(),
        })
      },
    })

    // Two cold lanes admit both 1.5 MiB single-block files concurrently with an empty queue.
    lanes.add(makeLane(1), 'application-relay', 1)
    lanes.add(makeLane(2), 'direct', 1)

    try {
      const job = new TransferJob({
        descriptor: share.descriptor,
        catalog: share.catalog,
        selection,
        revisions,
        broker: {
          readRange: (descriptor, leaseId, range, options) =>
            broker.readRouteAuthorizedRange(descriptor, leaseId, range, { ...options, routes }),
        },
        output: {
          sessionId: 'two-files-in-flight',
          capabilities: { maxConcurrentFiles: 2, resumable: true },
          openFile: async () => outputFile({}),
        },
        onProgress: progress => {
          if (progress.discovery === 'complete') discoveryDone.resolve()
        },
      })

      const run = job.run()
      await Promise.all([bothInFlight.promise, discoveryDone.promise])

      const demand = lanes.schedulingDemand()
      expect(demand).toMatchObject({
        queuedBytes: 0,
        networkWaitingBytes: 0,
        capacityWaitingBytes: 0,
        inFlightBytes: 3 * MiB,
        unfinishedBytes: 3 * MiB,
        remainingBytes: 3 * MiB,
        remainingKnown: true,
      })

      const pressure = new TURNDeliveryPressure()
      expect(pressure.observe(1_000, demand)).toBeUndefined()
      expect(pressure.observe(4_000, demand)).toBeUndefined()

      deliver.resolve()
      const result = await run
      expect(result.status).toBe('completed')
      expect(result.completedBytes).toBe(BigInt(3 * MiB))
      expect(lanes.schedulingDemand()).toMatchObject({ remainingBytes: 0, remainingKnown: false })
    } finally {
      routes.close()
      broker.close()
      lanes.close()
    }
  })
})
