import { expect, test } from '@playwright/test'
import { Uint8ArrayReader, Uint8ArrayWriter, ZipReader } from '@zip.js/zip.js'
import { BROWSER_CONTRACT_HOST_PATH } from '../contract-host'

for (const mode of ['before-truncate', 'cancel-before-truncate', 'after-truncate'] as const) {
  test('production Direct ZIP replays durable member rollback intent: ' + mode, async ({ page }) => {
    await page.goto(BROWSER_CONTRACT_HOST_PATH)
    const result = await page.evaluate(async input => {
      const path = '/test/browser/direct-zip/member-rollback-faults.ts'
      const probe = await import(path) as typeof import('./member-rollback-faults')
      return probe.probeProductionMemberRollbackFault(input.databaseName, input.mode)
    }, { databaseName: 'direct-zip-member-rollback-fault-' + crypto.randomUUID(), mode })
    expect(result.interrupted).toMatchObject({
      checkpointPhase: 'inside-member', safePayload: '6', candidateKind: 'rollback',
      proposalPhase: 'between-members', resumedRanges: [],
    })
    expect(result.interrupted.bytes).toEqual(mode === 'after-truncate' ? result.prefix : result.interrupted.originalBytes)
    expect(result.recovered).toEqual({
      phase: 'between-members', safePayload: '3', ordinal: '2', candidatePresent: false,
      fileBytes: result.rollbackOffset, archiveOffset: result.rollbackOffset, prefix: result.prefix,
    })
    expect(result.completed.lifecycle).toBe('published')
    expect(result.completed.completed.safePayload).toBe('12')
    expect(result.completed.prefixAfter).toEqual(result.prefix)
    expect(result.completed.ranges.filter(range => range.phase === 'resumed')).toEqual([
      { phase: 'resumed', name: 'active.txt', start: '0', end: '3' },
      { phase: 'resumed', name: 'active.txt', start: '3', end: '6' },
      { phase: 'resumed', name: 'last.txt', start: '0', end: '3' },
    ])
    const archive = new ZipReader(new Uint8ArrayReader(Uint8Array.from(result.completed.archive)), {
      checkSignature: true, useWebWorkers: false,
    })
    try {
      const files = (await archive.getEntries()).filter(entry => !entry.directory)
      expect(files.map(file => file.filename)).toEqual(result.completed.expected.map(file => file.name))
      for (const [index, file] of files.entries()) {
        expect(await file.getData!(new Uint8ArrayWriter()))
          .toEqual(Uint8Array.from(result.completed.expected[index]!.bytes))
      }
    } finally { await archive.close() }
  })
}

for (const mode of ['ownership-marker', 'completed-prefix'] as const) {
  test('production Direct ZIP refuses rollback after retained authority changes: ' + mode, async ({ page }) => {
    await page.goto(BROWSER_CONTRACT_HOST_PATH)
    const result = await page.evaluate(async input => {
      const path = '/test/browser/direct-zip/member-rollback-faults.ts'
      const probe = await import(path) as typeof import('./member-rollback-faults')
      return probe.probeProductionMemberRollbackTamper(input.databaseName, input.mode)
    }, { databaseName: 'direct-zip-member-rollback-tamper-' + crypto.randomUUID(), mode })
    expect(result.rejected).toBe(true)
    expect(result.after).toEqual(result.before)
    expect(result.checkpointDigest).toBe(result.expectedCheckpointDigest)
    expect(result.candidateDigest).toBe(result.interrupted.candidateDigest)
    expect(result.resumedRanges).toEqual([])
  })
}

for (const mode of ['earlier-completed-boundary', 'target-binding', 'target-observation'] as const) {
  test('production Direct ZIP rejects canonical rollback authority substitution: ' + mode, async ({ page }) => {
    await page.goto(BROWSER_CONTRACT_HOST_PATH)
    const result = await page.evaluate(async input => {
      const path = '/test/browser/direct-zip/member-rollback-candidate-tamper.ts'
      const probe = await import(path) as typeof import('./member-rollback-candidate-tamper')
      return probe.probeProductionMemberRollbackCandidateTamper(input.databaseName, input.mode)
    }, { databaseName: 'direct-zip-rollback-candidate-tamper-' + crypto.randomUUID(), mode })
    expect(result.canonicalCandidateDigest).toBe(result.forgedCandidateDigest)
    expect(result.rejected).toBe(true)
    expect(result.openedExecution).toBe(false)
    expect(result.mutationCounts).toMatchObject({ opens: 0, writes: 0, writtenBytes: 0, closes: 0 })
    expect(result.after).toEqual(result.expectedBytes)
    expect(result.checkpointDigest).toBe(result.expectedCheckpointDigest)
    expect(result.retainedCandidateDigest).toBe(result.forgedCandidateDigest)
    expect(result.resumedRanges).toEqual([])
    if (mode === 'earlier-completed-boundary') {
      expect(result.forgedOrdinal).toBe('1')
      expect(result.forgedOffset).toBeLessThan(result.activeMemberOffset)
    }
  })
}
