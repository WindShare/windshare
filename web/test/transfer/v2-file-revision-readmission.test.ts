import { expect, it } from 'vitest'
import { byteRange, type ByteRange } from '../../src/content/geometry'
import type { BlockRangeReader } from '../../src/content/broker'
import { RevisionReopenedDuringRecoveryError, type RevisionReader } from '../../src/content/session-services'
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
