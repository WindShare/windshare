import { describe, expect, it } from 'vitest'

import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import { byteRange } from '../../src/content/geometry'
import {
  V2BlockBroker,
  V2LaneSet,
  type V2BlockDemand,
  type V2BlockLane,
  type V2BlockRangeReader,
  type V2BlockRouteEligibility,
} from '../../src/content/v2-broker'
import type { V2BlockRecord } from '../../src/content/v2-records'
import {
  V2RevisionCapacityBusyError,
  type V2RevisionReader,
} from '../../src/content/v2-session-services'
import { V2_REVISION_CODE_QUOTA } from '../../src/content/v2-flow'
import { outputExecutionProfile } from '../../src/transfer/output-session'
import {
  createFailureIdentity,
  createProtocolFailure,
} from '../../src/diagnostics/incident'
import { CheckpointLineageDecisionError } from '../../src/output/persistent-tree/errors'
import type { TransferProgress, TransferTraceEvent } from '../../src/transfer/v2-job'
import type {
  V2ProtocolSessionReplacementWaiter,
  V2RevisionCapacityClock,
} from '../../src/transfer/revision-capacity/public'
import type { TestOutput } from './v2-job-fixture'
import {
  catalogFixture,
  fileEntry,
  identity,
  planAuthorityFixture,
  readerFixture,
  receiveIntentFixture,
  selectOnlyFile,
  testOutput,
  transferJobFixture,
} from './v2-job-fixture'

const ALL_CONTENT_ROUTES: V2BlockRouteEligibility = Object.freeze({
  active: true,
  allows: () => true,
  assertActive: () => undefined,
  subscribe: () => () => undefined,
})

class DeferredTransferLane implements V2BlockLane {
  readonly id: number
  readonly calls: Array<Readonly<{
    demand: V2BlockDemand
    resolve: (record: V2BlockRecord) => void
  }>> = []

  constructor(id: number) {
    this.id = id
  }

  fetchBlock(demand: V2BlockDemand): Promise<V2BlockRecord> {
    return new Promise((resolve) => this.calls.push({ demand, resolve }))
  }

  completeAll(): void {
    for (const call of this.calls) {
      const block = call.demand.descriptor.geometry.blockPlaintext(call.demand.localBlockIndex)
      call.resolve(Object.freeze({
        descriptor: call.demand.descriptor,
        localBlockIndex: call.demand.localBlockIndex,
        data: new Uint8Array(Number(block.end - block.start)).fill(this.id),
      }))
    }
  }
}

describe('v2 authenticated file transfer', () => {
  it('opens the authenticated revision through the adapter before output ownership exists', async () => {
    const events: string[] = []
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file], events)
    const output = testOutput(events)
    const plans = planAuthorityFixture({ output })
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic',
      artifactKind: 'original-file',
      selection,
      file,
    })

    const result = await transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
    }).run()

    expect(result.worker.status).toBe('Succeeded')
    expect(result.lifecycle.kind).toBe('published')
    expect(events.slice(0, 4)).toEqual([
      'begin-request',
      `revision:${file.idText}`,
      'revision-opened',
      'transaction-created',
    ])
    expect(events.indexOf(`block:${file.idText}`)).toBeGreaterThan(events.indexOf('transaction-created'))
    expect(output.requests).toHaveLength(1)
    expect(output.requests[0]).toMatchObject({
      source: { fileId: file.idText },
      sourceAuthenticationPath: ['payload.bin'],
      logicalArtifactPath: ['payload.bin'],
      materializationRelativePath: ['payload.bin'],
      expectedSize: 4n,
    })
    expect(readers.revisionRequests).toEqual([file.idText])
    expect(readers.releases).toEqual([file.idText])
    expect(plans.settlements).toEqual(['direct-atomic:Succeeded'])
  })

  registerRevisionCapacityJobTests()
  registerCheckpointSchedulingTests()

  it('isolates a checkpoint decision while an independent sibling completes', async () => {
    const root = identity(2)
    const blocked = fileEntry(identity(11), 'blocked.bin', 2n)
    const sibling = fileEntry(identity(12), 'sibling.bin', 2n)
    const selection = new V2SelectionPolicy(true)
    const catalog = catalogFixture([{ id: root, entries: [blocked, sibling] }])
    const readers = readerFixture([blocked, sibling])
    const output = testOutput([], {
      failBeginFor: blocked.idText,
      beginFailure: new CheckpointLineageDecisionError('revision-conflict'),
    })
    const plans = planAuthorityFixture({ output })
    const intent = await receiveIntentFixture({
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      selection,
    })

    const result = await transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
    }).run()

    expect(result.worker.status).toBe('CompletedWithErrors')
    expect(result.worker.fileOutcomes).toEqual(expect.objectContaining({
      revisionConflictFiles: 1,
      sourceDriftFiles: 0,
      failedFiles: 0,
    }))
    expect(output.commits).toEqual([sibling.idText])
    expect(readers.blockRequests).toEqual([sibling.idText])
    expect(result.lifecycle.kind).toBe('partial-directory')
  })

  it('requests only missing authenticated ranges and still commits the whole file', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file])
    const output = testOutput([], {
      durability: 'ProcessRestart',
      initialRanges: [byteRange(0n, 2n)],
    })
    const plans = planAuthorityFixture({ output })
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic',
      artifactKind: 'original-file',
      selection,
      file,
    })

    const result = await transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
    }).run()

    expect(result.worker.status).toBe('Succeeded')
    expect(readers.blockRequests).toEqual([file.idText])
    expect(output.writes).toEqual([{ offset: 2n, bytes: 2 }])
    expect(output.commits).toEqual([file.idText])
    expect(readers.releases).toEqual([file.idText])
  })

  it('fills disjoint unaligned gaps without rewriting the durable middle range', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file])
    const output = testOutput([], {
      durability: 'ProcessRestart',
      initialRanges: [byteRange(1n, 3n)],
    })
    const plans = planAuthorityFixture({ output })
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic',
      artifactKind: 'original-file',
      selection,
      file,
    })

    const result = await transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
    }).run()

    expect(result.worker.status).toBe('Succeeded')
    expect(output.writes).toEqual([
      { offset: 0n, bytes: 1 },
      { offset: 3n, bytes: 1 },
    ])
    expect(readers.blockRequests).toEqual([file.idText, file.idText])
    expect(output.commits).toEqual([file.idText])
    expect(output.finalProofs).toHaveLength(1)
  })

  it('pipelines one large file across relay and peer lanes', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'parallel.bin', 8n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file])
    const lanes = new V2LaneSet()
    const relay = new DeferredTransferLane(1)
    const peer = new DeferredTransferLane(2)
    lanes.add(relay, 'relay')
    lanes.add(peer, 'peer')
    const blockBroker = new V2BlockBroker(lanes)
    const broker: V2BlockRangeReader = {
      readRange: (descriptor, leaseId, range, options = {}) =>
        blockBroker.readRouteAuthorizedRange(descriptor, leaseId, range, {
          ...options,
          routes: ALL_CONTENT_ROUTES,
        }),
    }
    const output = testOutput()
    const plans = planAuthorityFixture({ output })
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic',
      artifactKind: 'original-file',
      selection,
      file,
    })

    const running = transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker,
    }).run()
    await expect.poll(() => relay.calls.length + peer.calls.length).toBe(4)
    expect(relay.calls).toHaveLength(2)
    expect(peer.calls).toHaveLength(2)
    expect([...relay.calls, ...peer.calls]
      .map((call) => call.demand.localBlockIndex)
      .sort((left, right) => Number(left - right))).toEqual([0n, 1n, 2n, 3n])

    relay.completeAll()
    peer.completeAll()
    const result = await running
    blockBroker.close()
    lanes.close()

    expect(result.worker.status).toBe('Succeeded')
    expect(output.writes).toEqual([
      { offset: 0n, bytes: 2 },
      { offset: 2n, bytes: 2 },
      { offset: 4n, bytes: 2 },
      { offset: 6n, bytes: 2 },
    ])
  })

  it('commits an authenticated empty file without requesting content blocks', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'empty.bin', 0n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file])
    const output = testOutput()
    const plans = planAuthorityFixture({ output })
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic',
      artifactKind: 'original-file',
      selection,
      file,
    })

    const result = await transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
    }).run()

    expect(result.worker.status).toBe('Succeeded')
    expect(readers.revisionRequests).toEqual([file.idText])
    expect(readers.blockRequests).toEqual([])
    expect(output.writes).toEqual([])
    expect(output.commits).toEqual([file.idText])
    expect(readers.releases).toEqual([file.idText])
  })

  it.each([
    'direct-atomic',
    'workspace-then-publish',
    'portable-handoff',
  ] as const)('never seals or publishes %s after a selected revision failure', async (planKind) => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file], [], { failRevisionFor: file.idText })
    const plans = planAuthorityFixture()
    const intent = await receiveIntentFixture({
      planKind,
      artifactKind: 'original-file',
      selection,
      file,
    })

    const result = await transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
    }).run()

    expect(result.worker.status).toBe('CompletedWithErrors')
    expect(result.lifecycle.kind).toBe(planKind === 'workspace-then-publish'
      ? 'resumable-receive'
      : 'restart-required')
    expect(plans.settlements).toEqual([])
    expect(plans.pauses).toEqual([planKind])
    expect(readers.revisionRequests).toEqual([file.idText])
    expect(readers.blockRequests).toEqual([])
    expect(plans.output.commits).toEqual([])
  })

  it('rejects an adapter that invokes the revision callback twice and releases the first lease', async () => {
    const events: string[] = []
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 2n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file], events)
    const base = testOutput(events)
    const output: TestOutput = {
      ...base,
      beginFile: async (request, signal) => {
        await request.openRevision(signal)
        return base.beginFile(request, signal)
      },
    }
    const plans = planAuthorityFixture({ output })
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic',
      artifactKind: 'original-file',
      selection,
      file,
    })

    const result = await transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
    }).run()

    expect(result.worker.status).toBe('Paused')
    expect(result.lifecycle.kind).toBe('restart-required')
    expect(readers.revisionRequests).toEqual([file.idText])
    expect(readers.blockRequests).toEqual([])
    expect(readers.releases).toEqual([file.idText])
    expect(events).not.toContain('transaction-created')
    expect(plans.settlements).toEqual([])
    expect(plans.pauses).toEqual(['direct-atomic'])
  })
})

function registerCheckpointSchedulingTests(): void {
  it.each([
    { label: 'no trigger fires', pendingBytes: 8n },
    { label: 'only the final block reaches the trigger', pendingBytes: 4n },
  ])('commits a normal multi-block file without an automatic checkpoint when $label', async ({
    pendingBytes,
  }) => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file])
    const output = testOutput([], {
      durability: 'ProcessRestart',
      executionProfile: boundedCheckpointProfile(pendingBytes),
    })
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic', artifactKind: 'original-file', selection, file,
    })

    const result = await transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans: planAuthorityFixture({ output }),
      revisions: readers.revisions,
      broker: readers.broker,
    }).run()

    expect(result.worker.status).toBe('Succeeded')
    expect(output.writes).toHaveLength(2)
    expect(output.automaticCheckpointAttempts).toEqual([])
    expect(output.finalProofs).toHaveLength(1)
  })

  it('acknowledges written progress before waiting for a delayed automatic checkpoint', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file])
    const checkpoint = deferred<void>()
    const output = testOutput([], {
      durability: 'ProcessRestart',
      executionProfile: boundedCheckpointProfile(2n),
      beforeAutomaticCheckpoint: () => checkpoint.promise,
    })
    const progress: TransferProgress[] = []
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic', artifactKind: 'original-file', selection, file,
    })
    const running = transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans: planAuthorityFixture({ output }),
      revisions: readers.revisions,
      broker: readers.broker,
      onProgress: value => progress.push(value),
    }).run()

    await expect.poll(() => output.automaticCheckpointAttempts.length).toBe(1)
    expect(progress.at(-1)).toMatchObject({
      writtenBytes: 2n,
      recoverableBytes: 0n,
      completedFiles: 0,
    })
    expect(output.checkpointAdvances).toEqual([])
    checkpoint.resolve()
    await expect(running).resolves.toMatchObject({ worker: { status: 'Succeeded' } })
    expect(output.checkpointAdvances).toEqual([file.idText])
    expect(progress.at(-1)).toMatchObject({ recoverableBytes: 4n, completedFiles: 1 })
  })

  it('suppresses later automatic checkpoints after the first decline', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 6n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file])
    const output = testOutput([], {
      durability: 'ProcessRestart',
      executionProfile: boundedCheckpointProfile(2n),
      automaticCheckpointDecisions: ['declined', 'advanced'],
    })
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic', artifactKind: 'original-file', selection, file,
    })

    const result = await transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans: planAuthorityFixture({ output }),
      revisions: readers.revisions,
      broker: readers.broker,
    }).run()

    expect(result.worker.status).toBe('Succeeded')
    expect(output.automaticCheckpointAttempts).toEqual([file.idText])
    expect(output.checkpointDeclines).toEqual([file.idText])
    expect(output.checkpointAdvances).toEqual([])
    expect(output.finalProofs).toHaveLength(1)
  })
}

function registerRevisionCapacityJobTests(): void {
  it('retries authenticated capacity behind one output transaction and records no file error', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file])
    const output = testOutput()
    const plans = planAuthorityFixture({ output })
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic', artifactKind: 'original-file', selection, file,
    })
    let opens = 0
    const revisions: V2RevisionReader = {
      open: async (fileId, signal) => {
        opens += 1
        if (opens === 1) throw capacityFailure(25)
        return readers.revisions.open(fileId, signal)
      },
    }
    const traces: TransferTraceEvent[] = []
    const progress: TransferProgress[] = []
    const result = await transferJobFixture({
      catalog: catalog.catalog, selection, intent, plans, revisions, broker: readers.broker,
      revisionCapacity: immediateCapacityPolicy(1_000),
      onProgress: value => progress.push(value),
      trace: { current: event => traces.push(event) },
    }).run()

    expect(result.worker.status).toBe('Succeeded')
    expect(opens).toBe(2)
    expect(output.requests).toHaveLength(1)
    expect(output.commits).toEqual([file.idText])
    expect(readers.revisionRequests).toEqual([file.idText])
    expect(progress.at(-1)).toMatchObject({
      fileErrors: 0,
      capacityWaitingFiles: 0,
      capacityAccumulatedWaitMilliseconds: 25,
      capacityWaitAttempts: 1,
      capacityWaitVisible: false,
    })
    expect(traces.filter(event => event.name === 'receive_transition').map(event => event.transition))
      .toEqual(expect.arrayContaining(['capacity_retry_scheduled', 'capacity_retry_succeeded']))
  })

  it('turns capacity wait budget exhaustion into a resumable pause with zero file errors', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file])
    const output = testOutput()
    const plans = planAuthorityFixture({ output })
    const intent = await receiveIntentFixture({
      planKind: 'workspace-then-publish', artifactKind: 'original-file', selection, file,
    })
    let opens = 0
    const progress: TransferProgress[] = []
    const traces: TransferTraceEvent[] = []
    const result = await transferJobFixture({
      catalog: catalog.catalog, selection, intent, plans,
      revisions: { open: async () => { opens += 1; throw capacityFailure(100) } },
      broker: readers.broker,
      revisionCapacity: immediateCapacityPolicy(50),
      onProgress: value => progress.push(value),
      trace: { current: event => traces.push(event) },
    }).run()

    expect(opens).toBe(1)
    expect(result.worker).toMatchObject({ status: 'Paused', failureCount: 0 })
    expect(result.lifecycle.kind).toBe('resumable-receive')
    expect(result.failureTrigger).toMatchObject({
      fault: { scope: 'output-pause' },
      fact: { kind: 'protocol_failure', recoveryDisposition: 'resumable_receive' },
    })
    expect(output.requests).toHaveLength(1)
    expect(output.commits).toEqual([])
    expect(progress.at(-1)).toMatchObject({
      fileErrors: 0,
      capacityWaitingFiles: 0,
      capacityAccumulatedWaitMilliseconds: 50,
      capacityWaitAttempts: 1,
      capacityWaitVisible: false,
    })
    expect(traces).toContainEqual(expect.objectContaining({
      name: 'receive_transition', transition: 'capacity_wait_budget_paused',
    }))
  })
}

function capacityFailure(retryAfterMilliseconds: number): V2RevisionCapacityBusyError {
  const failure = Object.freeze({
    code: V2_REVISION_CODE_QUOTA,
    retryable: true as const,
    retryAfterMilliseconds,
  })
  return new V2RevisionCapacityBusyError(failure, createProtocolFailure({
    requestKind: 'open_revisions',
    wireScope: 'revision',
    wireCode: failure.code,
    retryable: true,
    retryAfterMilliseconds,
    settlement: Object.freeze({ kind: 'received_authenticated' }),
    correlation: {
      protocolSessionId: createFailureIdentity('protocol_session', identity(91)),
      protocolOperationId: createFailureIdentity('protocol_operation', identity(92)),
    },
  }))
}

function immediateCapacityPolicy(waitBudgetMilliseconds: number) {
  let currentTime = 0
  const clock: V2RevisionCapacityClock = {
    now: () => currentTime,
    sleep: async (milliseconds, signal) => {
      signal.throwIfAborted()
      currentTime += milliseconds
    },
  }
  const generation: V2ProtocolSessionReplacementWaiter = {
    waitForProtocolSessionReplacement: (_identity, signal) => new Promise((_resolve, reject) => {
      const abort = () => reject(signal.reason)
      signal.addEventListener('abort', abort, { once: true })
      if (signal.aborted) abort()
    }),
  }
  return Object.freeze({
    generation,
    clock,
    waitBudgetMilliseconds,
    additiveJitterLimitMilliseconds: 0,
    visibilityThresholdMilliseconds: 0,
    random: () => 0,
    randomBytes: (length: number) => identity(93).slice(0, length),
  })
}

function boundedCheckpointProfile(pendingBytes: bigint) {
  return outputExecutionProfile({
    maximumConcurrentFilePipelines: 1,
    maximumOutstandingWriteBytes: 8n,
    maximumBufferedBytes: 8n,
    automaticCheckpoint: {
      kind: 'bounded',
      trigger: { pendingBytes, pendingMilliseconds: 60_000 },
      costBudget: {
        maximumPrefixCopyBytes: 8n,
        maximumCumulativeWriteAmplificationBytes: 8n,
        maximumPeakTemporaryBytes: 8n,
      },
    },
  })
}

function deferred<T>(): {
  readonly promise: Promise<T>
  readonly resolve: (value: T) => void
} {
  let resolve!: (value: T) => void
  const promise = new Promise<T>(next => { resolve = next })
  return { promise, resolve }
}
