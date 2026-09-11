import { expect, test } from '@playwright/test'
import { Uint8ArrayReader, Uint8ArrayWriter, ZipReader } from '@zip.js/zip.js'
import { BROWSER_CONTRACT_HOST_PATH } from '../browser-storage-support'

for (const mode of ['unchanged-revision', 'identical-content', 'changed-content'] as const) {
  test('production Direct ZIP preserves completed members across source resume: ' + mode, async ({ page }) => {
    await page.goto(BROWSER_CONTRACT_HOST_PATH)
    const result = await page.evaluate(async input => {
      const path = '/test/browser/direct-zip/member-rollback-probe.ts'
      const probe = await import(path) as typeof import('./member-rollback-probe')
      return probe.probeProductionMemberRollback(input.databaseName, input.mode)
    }, { databaseName: 'direct-zip-member-rollback-' + crypto.randomUUID(), mode })
    expect(result.paused).toEqual({
      phase: 'inside-member', ordinal: '2', safePayload: '6', memberOffset: '3', completedPayload: '3',
    })
    expect(result.lifecycle).toBe('published')
    expect(result.completed.safePayload).toBe('12')
    expect(result.prefixBefore.length).toBeGreaterThan(0)
    expect(result.prefixAfter).toEqual(result.prefixBefore)
    expect(result.opens.filter(open => open.phase === 'resumed').map(open => open.name))
      .toEqual(['active.txt', 'last.txt'])
    const activeRevisions = result.opens.filter(open => open.name === 'active.txt')
    expect(activeRevisions[0]!.revision === activeRevisions[1]!.revision).toBe(mode === 'unchanged-revision')
    expect(result.initialDurable.filter(member => member.phase === 'resumed')).toEqual([
      { phase: 'resumed', name: 'active.txt', offset: mode === 'unchanged-revision' ? '3' : '0' },
      { phase: 'resumed', name: 'last.txt', offset: '0' },
    ])
    expect(result.ranges.filter(range => range.phase === 'resumed')).toEqual([
      ...(mode === 'unchanged-revision' ? [] : [{ phase: 'resumed', name: 'active.txt', start: '0', end: '3' }]),
      { phase: 'resumed', name: 'active.txt', start: '3', end: '6' },
      { phase: 'resumed', name: 'last.txt', start: '0', end: '3' },
    ])
    const archive = new ZipReader(new Uint8ArrayReader(Uint8Array.from(result.archive)), {
      checkSignature: true, useWebWorkers: false,
    })
    try {
      const files = (await archive.getEntries()).filter(entry => !entry.directory)
      expect(files.map(file => file.filename)).toEqual(result.expected.map(file => file.name))
      for (const [index, file] of files.entries()) {
        expect(await file.getData!(new Uint8ArrayWriter())).toEqual(Uint8Array.from(result.expected[index]!.bytes))
      }
    } finally { await archive.close() }
  })
}
