import { expect, test } from '@playwright/test'
import { Uint8ArrayReader, Uint8ArrayWriter, ZipReader } from '@zip.js/zip.js'

for (const mode of ['complete', 'pause-resume', 'delete', 'delete-retry', 'unpromoted-resume',
  'unpromoted-delete', 'unpromoted-continue', 'unpromoted-settle', 'bootstrap-recovery',
  'completion-journal-recovery', 'completion-acknowledgement-recovery', 'completion-continue'] as const) {
  test('production Direct ZIP composition: ' + mode, async ({ page }) => {
    await page.goto('/')
    const result = await page.evaluate(async input => {
      const path = '/test/browser/direct-zip/production-probe.ts'
      const probe = await import(path) as typeof import('./production-probe')
      return probe.probeBrowserDirectZipProduction(input.databaseName, input.mode)
    }, { databaseName: 'direct-zip-production-' + crypto.randomUUID(), mode })
    expect(result.directSupport).toBe('runtime-supported')
    if (mode === 'delete' || mode === 'delete-retry' || mode === 'unpromoted-delete') {
      expect(result.contents).toEqual([])
    } else {
      expect(result.lifecycle).toBe('published')
      expect(result.signature).toEqual([0x50, 0x4b, 0x05, 0x06])
      expect(result.resumeOffset).toBe(mode === 'complete' || mode === 'bootstrap-recovery' ||
        mode.startsWith('completion-') ? '0' : '3')
      expect(result.fileBytes).toBeGreaterThan(6)
    }
    if (mode === 'complete' || mode.startsWith('completion-')) {
      const finalization = result.finalization!
      expect(finalization.before.opens).toBe(2)
      expect(finalization.after.opens).toBe(finalization.before.opens)
      expect(finalization.after.lastWritable).toBe(finalization.before.lastWritable)
      expect(finalization.after.prefixBytes).toBe(finalization.before.prefixBytes)
      expect(finalization.after.writtenBytes).toBeGreaterThan(finalization.before.writtenBytes)
      expect(finalization.after.closes).toBe(finalization.before.closes + 1)
      const archive = new ZipReader(new Uint8ArrayReader(Uint8Array.from(result.archive!)), {
        checkSignature: true, useWebWorkers: false,
      })
      try {
        const files = (await archive.getEntries()).filter(entry => !entry.directory)
        expect(files.map(entry => entry.filename)).toEqual(['shared/a.txt'])
        expect(await files[0]!.getData!(new Uint8ArrayWriter())).toEqual(Uint8Array.of(1, 2, 3, 4, 5, 6))
      } finally { await archive.close() }
    }
    if (mode === 'completion-journal-recovery' || mode === 'completion-continue') {
      expect(result.recovery).toMatchObject({
        candidateKind: 'closing', safePayloadBefore: '0', candidatePayload: '6', lifecycleBeforeResume: 'receiving',
        continuationBeforeResume: 'verify-direct-zip-completion',
      })
    }
    if (mode === 'completion-acknowledgement-recovery') {
      expect(result.recovery).toMatchObject({ safePayloadBefore: '6', lifecycleBeforeResume: 'published',
        continuationBeforeResume: 'history-only' })
      expect(result.recovery!.candidateKind).toBeUndefined()
    }
    if (mode.startsWith('completion-')) {
      expect(result.recovery!.resumeTransfer).toBeUndefined()
      expect(result.recovery!.after).toEqual(result.recovery!.before)
      expect(result.archive).toEqual(result.recovery!.completedArchive)
    }
  })
}
