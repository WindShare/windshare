import { describe, expect, it } from 'vitest'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import { byteRange } from '../../src/content/geometry'
import { TransferPauseRequestedError } from '../../src/transfer/output-session'
import type { TransferProgress } from '../../src/transfer/v2-job'
import { V2TransferProgressLedger } from '../../src/transfer/progress/v2-ledger'
import {
  catalogFixture, fileEntry, identity, planAuthorityFixture, readerFixture,
  receiveIntentFixture, testOutput, transferJobFixture,
} from './v2-job-fixture'

async function runCoverage(initialEnd: bigint, failCommit = false, fileCount = 1) {
  const files = Array.from({ length: fileCount }, (_, index) => fileEntry(identity(index + 11), `file-${index}.bin`, 4n))
  const selection = new V2SelectionPolicy(true)
  const intent = await receiveIntentFixture({ planKind: 'direct-tree', artifactKind: 'directory-tree', selection })
  const catalog = catalogFixture([{ id: identity(2), entries: files }])
  const readers = readerFixture(files)
  const progress: TransferProgress[] = []
  const output = testOutput([], { durability: 'ProcessRestart', initialRanges: [byteRange(0n, initialEnd)], failCommit })
  const finalizationPhases: Array<TransferProgress['phase'] | undefined> = []
  const plans = planAuthorityFixture({
    output,
    beforeDirectoryFinalize: () => { finalizationPhases.push(progress.at(-1)?.phase) },
    beforeDirectTreeSettlement: () => { finalizationPhases.push(progress.at(-1)?.phase) },
  })
  const result = await transferJobFixture({
    catalog: catalog.catalog, selection, intent, plans,
    revisions: readers.revisions,
    broker: {
      readRange: async function* (descriptor, leaseId, range, request) {
        request?.onReceive?.(Number(range.end - range.start))
        yield* readers.broker.readRange(descriptor, leaseId, range, request)
      },
    },
    onProgress: value => { progress.push(value) },
  }).run()
  return { progress, result, output, finalizationPhases }
}

async function runPausedCoverage(options: {
  initialBytes?: bigint; durability?: 'ProcessRestart' | 'None'; failPause?: boolean; failRelease?: boolean
}) {
  const file = fileEntry(identity(11), 'partial.bin', 12n)
  const selection = new V2SelectionPolicy(true)
  const intent = await receiveIntentFixture({ planKind: 'direct-tree', artifactKind: 'directory-tree', selection })
  const catalog = catalogFixture([{ id: identity(2), entries: [file] }])
  const readers = readerFixture([file])
  const controller = new AbortController()
  const progress: TransferProgress[] = []
  const output = testOutput([], {
    durability: options.durability ?? 'ProcessRestart',
    initialRanges: [byteRange(0n, options.initialBytes ?? 0n)],
    beforePause: async () => { if (options.failPause) throw new Error('pause failed') },
  })
  const result = await transferJobFixture({
    catalog: catalog.catalog, selection, intent, plans: planAuthorityFixture({ output }),
    revisions: {
      open: async (...args) => {
        const revision = await readers.revisions.open(...args)
        return { ...revision, release: async () => {
          await revision.release()
          if (options.failRelease) throw new Error('lease release failed')
        } }
      },
    },
    broker: {
      readRange: async function* (_descriptor, _lease, range, request) {
        for (let offset = range.start; offset < range.end; offset += 2n) {
          request?.signal?.throwIfAborted()
          yield { offset, data: new Uint8Array(2).fill(7) }
        }
      },
    },
    onProgress: value => {
      progress.push(value)
      if (value.writtenBytes > 0n && !controller.signal.aborted) {
        controller.abort(new TransferPauseRequestedError())
      }
    },
  }).run(controller.signal)
  return { progress, result, output }
}

describe('native materialization progress', () => {
  it.each([0n, 2n])('retains a paused file including its %s previously verified bytes', async initialBytes => {
    const { progress, result, output } = await runPausedCoverage({ initialBytes })
    expect(result.worker.status).toBe('Paused')
    expect(output.pauseEvidence[0]?.ranges).toEqual([byteRange(0n, initialBytes + 2n)])
    expect(progress.at(-1)).toMatchObject({
      materializedBytes: initialBytes + 2n, writtenBytes: 2n,
      recoverableBytes: 2n, completedBytes: 0n, completedFiles: 0,
    })
  })

  it('retracts accepted writes when a transient pause retains no data', async () => {
    const { progress } = await runPausedCoverage({ durability: 'None' })
    expect(progress.some(sample => sample.materializedBytes === 2n)).toBe(true)
    expect(progress.at(-1)).toMatchObject({ materializedBytes: 0n, writtenBytes: 2n, completedBytes: 0n })
  })

  it('does not claim retained coverage when pause settlement fails', async () => {
    const { progress, output } = await runPausedCoverage({ failPause: true })
    expect(output.pauseEvidence).toEqual([])
    expect(progress.at(-1)).toMatchObject({ materializedBytes: 0n, writtenBytes: 2n, completedBytes: 0n })
  })

  it('keeps confirmed storage progress when remote lease cleanup fails', async () => {
    const { progress } = await runPausedCoverage({ initialBytes: 2n, failRelease: true })
    expect(progress.at(-1)).toMatchObject({ materializedBytes: 4n, writtenBytes: 2n, completedBytes: 0n })
  })

  it('includes retained ranges before reading and never adds completion twice', async () => {
    const { progress, result, finalizationPhases } = await runCoverage(2n)
    expect(result.worker.status).toBe('Succeeded')
    expect(progress).toContainEqual(expect.objectContaining({ receivedObjectBytes: 2n, writtenBytes: 0n, materializedBytes: 2n }))
    expect(finalizationPhases.length).toBeGreaterThan(0)
    expect(finalizationPhases.every(phase => phase === 'finishing')).toBe(true)
    expect(progress).toEqual(expect.arrayContaining([
      expect.objectContaining({ writtenBytes: 0n, materializedBytes: 2n, completedBytes: 0n }),
      expect.objectContaining({ writtenBytes: 2n, materializedBytes: 4n, completedBytes: 0n }),
    ]))
    expect(progress.at(-1)).toMatchObject({ writtenBytes: 2n, materializedBytes: 4n, completedBytes: 4n, phase: 'finishing' })
    expect(progress.every(value => value.materializedBytes <= 4n)).toBe(true)
  })

  it('counts completely retained files while network receipt remains zero', async () => {
    const { progress, output } = await runCoverage(4n, false, 3)
    expect(output.writes).toEqual([])
    expect(progress.at(-1)).toMatchObject({ writtenBytes: 0n, materializedBytes: 12n, completedBytes: 12n, completedFiles: 3 })
  })

  it('retracts failed transaction coverage while preserving receipt as a separate fact', async () => {
    const { progress, result } = await runCoverage(2n, true)
    expect(result.worker.status).toBe('Paused')
    expect(progress.some(value => value.materializedBytes === 4n)).toBe(true)
    expect(progress.at(-1)).toMatchObject({ writtenBytes: 2n, materializedBytes: 0n, completedBytes: 0n })
  })

  it('replaces retried coverage without overlapping completed siblings or prior attempts', () => {
    const ledger = new V2TransferProgressLedger()
    ledger.completeFile(90n)
    ledger.observeMaterializedFile('partial', 6n)
    ledger.acknowledgeWrite(2n)
    ledger.observeMaterializedFile('partial', 8n)
    ledger.settleFile('partial', { kind: 'paused', retainedBytes: 7n })
    ledger.observeMaterializedFile('partial', 7n)
    const measure = { discovery: 'complete', discoveredFiles: 2, discoveredBytes: 100n, sizeClass: 'small' } as const
    expect(ledger.snapshot(measure)).toMatchObject({ materializedBytes: 97n, writtenBytes: 2n, completedBytes: 90n })
    ledger.settleFile('partial', { kind: 'completed', exactSize: 10n })
    expect(ledger.snapshot(measure)).toMatchObject({ materializedBytes: 100n, writtenBytes: 2n, completedBytes: 100n })
  })
})
