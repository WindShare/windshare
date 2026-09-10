import { expect, test } from '@playwright/test'

for (const mode of ['complete', 'pause-resume', 'delete', 'delete-retry', 'unpromoted-resume',
  'unpromoted-delete', 'unpromoted-continue', 'unpromoted-settle', 'bootstrap-recovery'] as const) {
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
      expect(result.resumeOffset).toBe(mode === 'complete' || mode === 'bootstrap-recovery' ? '0' : '3')
      expect(result.fileBytes).toBeGreaterThan(6)
    }
  })
}
