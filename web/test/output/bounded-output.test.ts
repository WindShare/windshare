import { describe, expect, it } from 'vitest'

import { EMPTY_TRANSFER_FAILURE_SUMMARY, jobOutcome } from '../../src/transfer/outcome'
import type { OutputFile } from '../../src/transfer/output-session'
import { PersistentTreeOutputSession } from '../../src/output/persistent-tree/session'
import { OUTPUT_JOURNAL_PAGE_RECORD_LIMIT } from '../../src/output/persistence/journal'
import { MemoryOutputJournal, MemoryOutputTree } from './fakes'

const ACTIVE_SIGNAL = new AbortController().signal

describe('bounded output structure', () => {
  it('recovers and enumerates staged files through bounded journal pages', async () => {
    const journal = new MemoryOutputJournal()
    const tree = new MemoryOutputTree()
    const options = {
      identity: { backend: 'memory-tree', outputSessionId: 'paged-recovery' },
      tree,
      journal,
    }
    const first = await PersistentTreeOutputSession.open(options)
    const fileCount = OUTPUT_JOURNAL_PAGE_RECORD_LIMIT * 3
    for (let index = 0; index < fileCount; index += 1) {
      const begun = await first.beginFile(zeroByteFile(index))
      await begun.transaction.commit()
    }
    await first.finishJob(
      jobOutcome('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY),
      ACTIVE_SIGNAL,
    )

    const reopened = await PersistentTreeOutputSession.open(options)
    expect(await reopened.stagedOutputTotals()).toEqual({
      logicalBytes: 0n,
      additionalBytes: 0n,
    })
    let enumerated = 0
    for await (const file of reopened.stagedCatalog().files()) {
      expect(file.record.committed).toBe(true)
      enumerated += 1
    }
    expect(enumerated).toBe(fileCount)
    expect(journal.maximumScanPageRecords).toBe(OUTPUT_JOURNAL_PAGE_RECORD_LIMIT)
  })
})

function zeroByteFile(index: number): OutputFile {
  const identity = index.toString(36)
  return {
    source: {
      shareInstance: 'million-share',
      fileId: `file-${identity}`,
      fileRevision: `revision-${identity}`,
    },
    path: [`file-${identity}`],
    exactSize: 0n,
  }
}
