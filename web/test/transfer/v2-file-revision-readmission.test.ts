import { expect, it } from 'vitest'
import { SelectionPolicy } from '../../src/catalog/selection'
import { byteRange, type ByteRange } from '../../src/content/geometry'
import type { BlockRangeReader } from '../../src/content/broker'
import { RevisionReopenedDuringRecoveryError, type RevisionReader } from '../../src/content/session-services'
import { BoundaryFaultError, FaultScope, SourceFaultCode, sourceFault } from '../../src/transfer/failure/fault'
import type { TransferProgress, TransferTraceEvent } from '../../src/transfer/job/contract'
import type { OutputSession } from '../../src/transfer/output/session'
import { deferred } from '../session/v2-send-fixture'
import {
  catalogFixture, digestIdentity, fileEntry, identity, identityText, planAuthorityFixture,
  readerFixture, receiveIntentFixture, selectOnlyFile, testOutput, transferJobFixture,
} from './v2-job-fixture'

it.each(['agreement', 'unavailable', 'changed-on-retry'] as const)(
  'settles the old file before deciding readmission (%s)',
  async (scenario) => {
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: identity(2), entries: [file] }])
    const initial = await readerFixture([file]).revisions.open(file.id)
    const previous = { ...initial.descriptor, sourceMetadata: digestIdentity(71) }
    const reopened = { ...initial.descriptor, fileRevision: identity(72), fileRevisionText: identityText(72),
      ...(scenario === 'unavailable' ? {} : { sourceMetadata: previous.sourceMetadata }),
    }
    const current = scenario === 'changed-on-retry'
      ? { ...reopened, fileRevision: identity(73), fileRevisionText: identityText(73), sourceMetadata: digestIdentity(74) }
      : reopened
    const events: string[] = []
    const pauseStarted = deferred<void>()
    const allowPause = deferred<void>()
    const firstOutput = testOutput(events, {
      durability: 'ProcessRestart',
      beforePause: async () => {
        pauseStarted.resolve()
        await allowPause.promise
      },
    })
    let secondOutput: ReturnType<typeof testOutput> | undefined
    const output = {
      ...firstOutput,
      readmitFile: async (file, paused, signal) => {
        expect(paused).toBe(firstOutput.pauseEvidence[0])
        expect(paused.ranges).toEqual([byteRange(0n, 2n)])
        secondOutput = testOutput(events, { durability: 'ProcessRestart', initialRanges: paused.ranges })
        return secondOutput.beginFile(file, signal)
      },
    } satisfies OutputSession
    let opens = 0
    const revisions: RevisionReader = {
      open: async (_file, signal) => {
        signal?.throwIfAborted()
        const attempt = ++opens
        events.push(`open:${attempt}`)
        return {
          descriptor: attempt === 1 ? previous : current,
          leaseId: identity(80 + attempt),
          release: async () => { events.push(`release:${attempt}`) },
        }
      },
    }
    const reads: ByteRange[] = []
    const broker: BlockRangeReader = {
      readRange: async function* (descriptor, lease, range) {
        reads.push(range)
        if (descriptor.fileRevisionText === previous.fileRevisionText) {
          expect(lease).toEqual(identity(81))
          try {
            yield { offset: 0n, data: Uint8Array.of(1, 2) }
            throw new RevisionReopenedDuringRecoveryError(previous, reopened)
          } finally {
            events.push('old-reader-drained')
          }
        }
        expect(lease).toEqual(identity(82))
        yield { offset: 2n, data: Uint8Array.of(3, 4) }
      },
    }
    const intent = await receiveIntentFixture({ planKind: 'direct-atomic', artifactKind: 'original-file', selection, file })
    const trace: TransferTraceEvent[] = []
    const progress: TransferProgress[] = []
    const running = transferJobFixture({
      catalog: catalog.catalog, selection, intent, revisions, broker, plans: planAuthorityFixture({ output }),
      trace: { current: value => trace.push(value) },
      onProgress: value => progress.push(value),
    }).run()
    await pauseStarted.promise
    expect(events).toContain('old-reader-drained')
    expect(opens).toBe(1)
    expect(events).not.toContain('release:1')
    allowPause.resolve()
    const result = await running

    expect(firstOutput.commits).toEqual([])
    expect(firstOutput.writes).toEqual([{ offset: 0n, bytes: 2 }])
    expect(events.indexOf('old-reader-drained')).toBeLessThan(events.indexOf('pause-started'))
    expect(events.indexOf('pause-completed')).toBeLessThan(events.indexOf('release:1'))
    const decisions = trace.filter(event => 'transition' in event && event.transition === 'file_revision_readmission')
    if (scenario === 'agreement') {
      expect(result.worker.status).toBe('Succeeded')
      expect(events.indexOf('release:1')).toBeLessThan(events.indexOf('open:2'))
      expect(reads).toEqual([byteRange(0n, 4n), byteRange(2n, 4n)])
      expect(secondOutput?.writes).toEqual([{ offset: 2n, bytes: 2 }])
      expect(secondOutput?.commits).toEqual([file.idText])
      expect(decisions.at(-1)).toMatchObject({
        decision: 'readmitted', previousRevision: previous.fileRevisionText,
        currentRevision: current.fileRevisionText, retainedBytes: 2n, reusedBytes: 2n, fileId: file.idText,
      })
      expect(progress.at(-1)).toMatchObject({ completedFiles: 1, completedBytes: 4n, writtenBytes: 4n })
    } else {
      expect(result.worker.status).toBe('CompletedWithErrors')
      expect(reads).toEqual([byteRange(0n, 4n)])
      expect(secondOutput?.commits ?? []).toEqual([])
      expect(firstOutput.pauseEvidence[0]?.ranges).toEqual([byteRange(0n, 2n)])
      expect(decisions.at(-1)).toMatchObject({
        decision: scenario === 'unavailable' ? 'evidence-unavailable' : 'admission-failed',
        comparisonReason: scenario === 'unavailable' ? 'metadata-unavailable' : 'metadata-changed',
        retainedBytes: 2n, reusedBytes: 0n,
      })
      expect(opens).toBe(scenario === 'unavailable' ? 1 : 2)
    }
  },
)

it('continues readmitting across multiple reopens while durable progress advances', async () => {
  const payload = fileEntry(identity(11), 'payload.bin', 6n)
  const selection = selectOnlyFile(payload)
  const catalog = catalogFixture([{ id: identity(2), entries: [payload] }])
  const readers = readerFixture([payload])
  const versions = [71, 72, 73].map(seed => ({
    fileRevision: identity(seed), fileRevisionText: identityText(seed), sourceMetadata: digestIdentity(74),
  }))
  let payloadOpens = 0
  const outputs: ReturnType<typeof testOutput>[] = []
  const firstOutput = testOutput([], { durability: 'ProcessRestart' })
  outputs.push(firstOutput)
  const output = {
    ...firstOutput,
    readmitFile: (file, paused, signal) => {
      const next = testOutput([], { durability: 'ProcessRestart', initialRanges: paused.ranges })
      outputs.push(next)
      return next.beginFile(file, signal)
    },
  } satisfies OutputSession
  const intent = await receiveIntentFixture({ planKind: 'direct-atomic', artifactKind: 'original-file', selection, file: payload })
  const result = await transferJobFixture({
    catalog: catalog.catalog, selection, intent, plans: planAuthorityFixture({ output }),
    revisions: { open: async (file, signal) => {
      const opened = await readers.revisions.open(file, signal)
      const attempt = payloadOpens++
      return {
        ...opened,
        descriptor: { ...opened.descriptor, ...versions[attempt]! },
        leaseId: identity(81 + attempt),
      }
    } },
    broker: { readRange: async function* (descriptor, _lease, range) {
      yield { offset: range.start, data: Uint8Array.of(1, 2) }
      if (range.start + 2n < range.end) {
        throw new RevisionReopenedDuringRecoveryError(descriptor, { ...descriptor, ...versions[payloadOpens]! })
      }
    } },
  }).run()

  expect(result.worker.status).toBe('Succeeded')
  expect(payloadOpens).toBe(3)
  expect(outputs.at(-1)?.commits).toEqual([payload.idText])
})

it.each([FaultScope.FileLocal, FaultScope.OutputPause])(
  'honors a %s release fault after checkpointing an exhausted readmission',
  async scope => {
    const payload = fileEntry(identity(11), 'payload.bin', 6n)
    const sibling = fileEntry(identity(12), 'sibling.bin', 2n)
    const selection = new SelectionPolicy(true)
    const catalog = catalogFixture([{ id: identity(2), entries: [payload, sibling] }])
    const readers = readerFixture([payload, sibling])
    const versions = [71, 72, 73, 74].map(seed => ({
      fileRevision: identity(seed), fileRevisionText: identityText(seed), sourceMetadata: digestIdentity(75),
    }))
    let payloadOpens = 0
    const firstOutput = testOutput([], { durability: 'ProcessRestart' })
    const output = {
      ...firstOutput,
      readmitFile: (file, paused, signal) =>
        testOutput([], { durability: 'ProcessRestart', initialRanges: paused.ranges }).beginFile(file, signal),
    } satisfies OutputSession
    const plans = planAuthorityFixture({ output })
    const intent = await receiveIntentFixture({ planKind: 'direct-tree', artifactKind: 'directory-tree', selection })
    const result = await transferJobFixture({
      catalog: catalog.catalog, selection, intent, plans, maximumConcurrentFiles: 1,
      revisions: { open: async (file, signal) => {
        const opened = await readers.revisions.open(file, signal)
        if (opened.descriptor.fileIdText !== payload.idText) return opened
        const attempt = payloadOpens++
        return {
          ...opened,
          descriptor: { ...opened.descriptor, ...versions[attempt]! },
          leaseId: identity(81 + attempt),
          release: async () => {
            await opened.release()
            if (attempt === 2) throw new BoundaryFaultError(sourceFault(scope, SourceFaultCode.Unavailable))
          },
        }
      } },
      broker: { readRange: async function* (descriptor, lease, range, request) {
        if (descriptor.fileIdText !== payload.idText) {
          yield* readers.broker.readRange(descriptor, lease, range, request)
          return
        }
        if (range.start === 0n) {
          yield { offset: range.start, data: Uint8Array.of(1, 2) }
        }
        throw new RevisionReopenedDuringRecoveryError(descriptor, { ...descriptor, ...versions[payloadOpens]! })
      } },
    }).run()

    expect(payloadOpens).toBe(3)
    expect(result.worker.status).toBe('Paused')
    expect(result.failureTrigger?.fault.scope).toBe(scope)
    expect(plans.pauseRequests).toHaveLength(1)
    expect(firstOutput.commits).toEqual(scope === FaultScope.FileLocal ? [sibling.idText] : [])
  },
)
