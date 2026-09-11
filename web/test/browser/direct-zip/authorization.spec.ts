import { expect, test } from '@playwright/test'
import { Uint8ArrayReader, Uint8ArrayWriter, ZipReader } from '@zip.js/zip.js'
import type { prepareAuthorizationProbe } from './authorization-probe'

declare global {
  interface Window {
    zipAuthorizationProbe: Awaited<ReturnType<typeof prepareAuthorizationProbe>>
  }
}

for (const scenario of ['resume', 'foreign-target'] as const) {
  test('Direct ZIP authorizes a live operation before verification: ' + scenario, async ({ page }) => {
    await page.goto('/')
    const databaseName = 'direct-zip-authorization-' + crypto.randomUUID()
    await page.evaluate(async name => {
      const path = '/test/browser/direct-zip/authorization-probe.ts'
      const probe = await import(path) as typeof import('./authorization-probe')
      window.zipAuthorizationProbe = await probe.prepareAuthorizationProbe(name)
    }, databaseName)
    try {
      const initial = await page.evaluate(() => window.zipAuthorizationProbe.initial)
      expect(initial.lifecycle).toBe('authorization-required')
      expect(await page.evaluate(() => window.zipAuthorizationProbe.requests)).toEqual([])
      if (scenario === 'resume') {
        for (const response of ['denied', 'prompt', 'cancelled'] as const) {
          await page.evaluate(value => window.zipAuthorizationProbe.configure(value), response)
          await page.getByRole('button', { name: 'Authorize and continue', exact: true }).click()
          const result = await page.evaluate(() => window.zipAuthorizationProbe.result())
          expect(result).toEqual({ lifecycle: 'authorization-required', resumeTransfer: undefined,
            error: response === 'cancelled' ? 'AbortError' : 'NotAllowedError' })
          expect(await page.evaluate(() => window.zipAuthorizationProbe.snapshot())).toEqual(initial)
        }
      } else {
        await page.evaluate(() => window.zipAuthorizationProbe.replaceTargetContents())
      }
      await page.evaluate(value => window.zipAuthorizationProbe.configure(value),
        scenario === 'resume' ? 'pending' as const : 'granted' as const)
      const beforeGrant = await page.evaluate(() => window.zipAuthorizationProbe.snapshot())
      await page.getByRole('button', { name: 'Authorize and continue', exact: true }).click()
      if (scenario === 'resume') {
        expect(await page.evaluate(() => window.zipAuthorizationProbe.snapshot())).toEqual(initial)
        await page.evaluate(() => window.zipAuthorizationProbe.grantPending())
      }
      const granted = await page.evaluate(() => window.zipAuthorizationProbe.result())
      const requests = await page.evaluate(() => window.zipAuthorizationProbe.requests)
      expect(requests).toHaveLength(scenario === 'resume' ? 4 : 1)
      for (const request of requests) {
        expect(request).toEqual({ mode: 'readwrite', directory: databaseName, insideClick: true, userActivation: true })
      }
      if (scenario === 'foreign-target') {
        expect(granted?.resumeTransfer).toBeUndefined()
        expect(granted?.error).toBe('DirectZipWriterGateError')
        const after = await page.evaluate(() => window.zipAuthorizationProbe.snapshot())
        expect(after.bytes).toEqual(beforeGrant.bytes)
        expect(after.fileSystem).toEqual(beforeGrant.fileSystem)
        expect(after.checkpoint).toBe(initial.checkpoint)
        return
      }
      expect(granted).toEqual({ lifecycle: 'receiving', resumeTransfer: true, error: undefined })
      const continued = await page.evaluate(() => window.zipAuthorizationProbe.snapshot())
      expect(continued.lookups).toBeGreaterThan(beforeGrant.lookups)
      expect(continued.checkpoint).toBe(initial.checkpoint)
      expect(continued.bytes).toEqual(initial.bytes)
      const completed = await page.evaluate(() => window.zipAuthorizationProbe.finish())
      expect(completed.lifecycle).toBe('published')
      expect(completed.initialDurable.filter(member => member.phase === 'resumed')).toEqual([
        { phase: 'resumed', name: 'active.txt', offset: '3' },
        { phase: 'resumed', name: 'last.txt', offset: '0' },
      ])
      const archive = new ZipReader(new Uint8ArrayReader(Uint8Array.from(completed.archive)), {
        checkSignature: true, useWebWorkers: false,
      })
      try {
        const files = (await archive.getEntries()).filter(file => !file.directory)
        expect(files.map(file => file.filename)).toEqual(completed.expected.map(file => file.name))
        for (const [index, file] of files.entries()) {
          expect(await file.getData!(new Uint8ArrayWriter())).toEqual(Uint8Array.from(completed.expected[index]!.bytes))
        }
      } finally { await archive.close() }
    } finally { await page.evaluate(() => window.zipAuthorizationProbe.close()) }
  })
}
