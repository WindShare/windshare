import { expect, test } from '@playwright/test'
import { Uint8ArrayReader, Uint8ArrayWriter, ZipReader } from '@zip.js/zip.js'
import { BROWSER_CONTRACT_HOST_PATH } from '../contract-host'

for (const mode of ['complete', 'pause-resume', 'delete', 'delete-retry', 'unpromoted-resume',
  'unpromoted-delete', 'unpromoted-continue', 'unpromoted-settle', 'bootstrap-recovery',
  'completion-journal-recovery', 'completion-acknowledgement-recovery', 'completion-continue',
  'aborted-write-continue', 'automatic-checkpoint-spacing', 'activation-recovery'] as const) {
  test('production Direct ZIP composition: ' + mode, async ({ page }) => {
    await page.goto(BROWSER_CONTRACT_HOST_PATH)
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
        mode === 'aborted-write-continue' || mode === 'automatic-checkpoint-spacing' ||
        mode === 'activation-recovery' || mode.startsWith('completion-') ? '0' : '3')
      expect(result.fileBytes).toBeGreaterThan(6)
      const progress = result.progress!
      const samples = progress.samples
      expect(samples.initial).toMatchObject({ received: '0', written: '0', safe: '0',
        lifecycle: 'receiving', category: 'active', actions: ['pause'] })
      expect(samples.published).toMatchObject({ lifecycle: 'published', category: 'terminal', actions: [] })
      if (samples.paused?.lifecycle === 'resumable-receive') {
        expect(samples.paused).toMatchObject({ category: 'retained', actions: ['continue', 'delete'] })
      }
      expect(samples.metadata).toMatchObject({ received: '0', written: '0', safe: '0' })
      const firstBytes = mode === 'automatic-checkpoint-spacing' ? '1024' : '3'
      const totalBytes = mode === 'automatic-checkpoint-spacing' ? '1536' : '6'
      expect(samples['first-write']).toMatchObject({ received: firstBytes, written: firstBytes, safe: '0' })
      expect(samples['before-finalization']).toMatchObject({ received: totalBytes, written: totalBytes, percentage: '99' })
      expect(samples.published).toMatchObject({ received: totalBytes, written: totalBytes, safe: totalBytes, percentage: '100' })
      if (samples.continued !== undefined) {
        const retainedBytes = mode === 'aborted-write-continue' ? '0' : '3'
        expect(samples.continued).toMatchObject({ received: retainedBytes, written: retainedBytes, safe: retainedBytes })
        expect(samples['resumed-member']).toMatchObject({ received: retainedBytes, written: retainedBytes, safe: retainedBytes })
      }
      assertProgressNotifications(progress, firstBytes)
    }
    if (mode === 'automatic-checkpoint-spacing') {
      const samples = result.progress!.samples
      expect(samples['automatic-checkpoint']).toMatchObject({ received: '1024', written: '1024', safe: '1024', percentage: '66' })
      expect(samples['spaced-write-1024']).toMatchObject({ received: '1280', written: '1280', safe: '1024', percentage: '83' })
      expect(samples['spaced-write-1280']).toMatchObject({ received: '1536', written: '1536', safe: '1024', percentage: '99' })
      expect(samples['spaced-write-1024']!.safeResume).toBe(samples['automatic-checkpoint']!.safeResume)
      expect(samples['spaced-write-1280']!.safeResume).toBe(samples['automatic-checkpoint']!.safeResume)
      // Bootstrap plus the one automatic cut; later checkpoint observations must
      // leave the native writable open while visible payload progress advances.
      expect(result.finalization!.before.closes).toBe(2)
      expect(result.finalization!.before.opens).toBe(3)
      expect(result.finalization!.after.closes).toBe(3)
      expect(result.finalization!.after.opens).toBe(3)
    }
    if (mode === 'aborted-write-continue') {
      expect(result.progress!.samples.paused).toMatchObject({ received: '0', written: '0', safe: '0' })
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

function assertProgressNotifications(
  progress: ReturnType<ReturnType<
    typeof import('./production-progress-observation').observeProductionDirectZipProgress>['result']>,
  firstBytes: string,
) {
  for (const events of progress.notifications) {
    expect(events.length).toBeGreaterThan(0)
    for (const [index, event] of events.entries()) {
      expect(event.operationId).toBe(progress.samples.initial!.operationId)
      expect(BigInt(event.safe)).toBeLessThanOrEqual(BigInt(event.written))
      expect(BigInt(event.written)).toBeLessThanOrEqual(BigInt(event.received))
      expect(BigInt(event.generation)).toBeGreaterThan(index === 0 ? 0n : BigInt(events[index - 1]!.generation))
    }
  }
  expect(progress.notifications.flat()).toEqual(expect.arrayContaining([
    expect.objectContaining({ received: firstBytes, written: '0', safe: '0' }),
    expect.objectContaining({ received: firstBytes, written: firstBytes, safe: '0' }),
  ]))
}
