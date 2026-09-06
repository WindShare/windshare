import { describe, expect, it } from 'vitest'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import { byteRange } from '../../src/content/geometry'
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
    revisions: readers.revisions, broker: readers.broker,
    onProgress: value => { progress.push(value) },
  }).run()
  return { progress, result, output, finalizationPhases }
}

describe('native materialization progress', () => {
  it('includes retained ranges before reading and never adds completion twice', async () => {
    const { progress, result, finalizationPhases } = await runCoverage(2n)
    expect(result.worker.status).toBe('Succeeded')
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
    ledger.observeMaterializedFile('partial', 0n)
    ledger.observeMaterializedFile('partial', 7n)
    const measure = { discovery: 'complete', discoveredFiles: 2, discoveredBytes: 100n, sizeClass: 'small' } as const
    expect(ledger.snapshot(measure)).toMatchObject({ materializedBytes: 97n, writtenBytes: 2n, completedBytes: 90n })
    ledger.completeFile(10n, 'partial')
    expect(ledger.snapshot(measure)).toMatchObject({ materializedBytes: 100n, writtenBytes: 2n, completedBytes: 100n })
  })
})
