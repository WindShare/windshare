import { expect, test } from '@playwright/test'

declare global {
  interface Window {
    diagnosticsArchiveProbe: { bodyReads: string[]; largeEncodes: number }
  }
}

test('ordinary startup lists summaries without reading or encoding archived log bodies', async ({ page }) => {
  await page.addInitScript(() => {
    const probe = { bodyReads: [] as string[], largeEncodes: 0 }
    window.diagnosticsArchiveProbe = probe
    const get = IDBObjectStore.prototype.get
    IDBObjectStore.prototype.get = function (query) {
      if (this.name === 'captures') probe.bodyReads.push(String(query))
      return get.call(this, query)
    }
    const getAll = IDBObjectStore.prototype.getAll
    IDBObjectStore.prototype.getAll = function (...args: Parameters<typeof getAll>) {
      if (this.name === 'captures') probe.bodyReads.push('*')
      return getAll.apply(this, args)
    }
    const openCursor = IDBObjectStore.prototype.openCursor
    IDBObjectStore.prototype.openCursor = function (...args: Parameters<typeof openCursor>) {
      if (this.name === 'captures') probe.bodyReads.push('*')
      return openCursor.apply(this, args)
    }
    const encode = TextEncoder.prototype.encode
    TextEncoder.prototype.encode = function (input) {
      if (input !== undefined && input.length >= 1_024 * 1_024) probe.largeEncodes++
      return encode.call(this, input)
    }
  })
  await page.goto('/')
  await expect.poll(() => page.evaluate(() => window.windshareDiagnostics !== undefined)).toBe(true)
  await page.evaluate(async () => {
    const modulePath = '/src/diagnostics/browser/indexeddb-archive.ts'
    const { IndexedDBDiagnosticsArchive } = await import(/* @vite-ignore */ modulePath) as typeof import('../../src/diagnostics/browser/indexeddb-archive')
    const archive = new IndexedDBDiagnosticsArchive(() => indexedDB)
    const runtimeRunId = 'AQAAAAAAAAAAAAAAAAAAAA'
    const text = JSON.stringify({ line_type: 'bundle_header', runtime_run_id: runtimeRunId, time: new Date().toISOString() }) +
      '\n' + JSON.stringify({ message: 'x'.repeat(5 * 1_024 * 1_024) }) + '\n'
    for (let index = 0; index < 3; index++) {
      await archive.save({
        id: 'capture-' + index, scope: index === 0 ? '/' : '/other-' + index, savedAt: Date.now() - index,
        file: { name: 'diagnostics.ndjson', runtimeRunId, text },
      })
    }
  })
  await page.reload()
  const bar = page.getByRole('complementary', { name: 'Diagnostics' })
  await expect(bar).toBeVisible()
  expect(await page.evaluate(() => window.windshareDiagnostics.status().enabled)).toBe(false)
  expect(await page.evaluate(() => window.diagnosticsArchiveProbe)).toEqual({ bodyReads: [], largeEncodes: 0 })

  await bar.getByRole('button', { name: 'Export diagnostics' }).click()
  const previous = page.getByRole('region', { name: 'Previous diagnostics' })
  await expect(previous.getByRole('button', { name: 'Save file' })).toBeVisible()
  expect(await page.evaluate(() => [...new Set(window.diagnosticsArchiveProbe.bodyReads)])).toEqual(['capture-0'])
  expect(await page.evaluate(() => window.diagnosticsArchiveProbe.largeEncodes)).toBe(0)
})

test('archive retention removes both summaries and bodies', async ({ page }) => {
  await page.goto('/test/browser/contract-host.html')
  const result = await page.evaluate(async () => {
    const modulePath = '/src/diagnostics/browser/indexeddb-archive.ts'
    const { IndexedDBDiagnosticsArchive } = await import(/* @vite-ignore */ modulePath) as typeof import('../../src/diagnostics/browser/indexeddb-archive')
    const policyPath = '/src/diagnostics/browser/archive.ts'
    const { DIAGNOSTICS_ARCHIVE_MAX_AGE_MS } = await import(/* @vite-ignore */ policyPath) as typeof import('../../src/diagnostics/browser/archive')
    let now = Date.now()
    const oldCapture = {
      id: 'original', scope: '/', savedAt: now,
      file: { name: 'diagnostics.ndjson', runtimeRunId: 'AQAAAAAAAAAAAAAAAAAAAA', text: '原始记录' },
    }
    const archive = new IndexedDBDiagnosticsArchive(() => indexedDB, () => now)
    const saved = await archive.save(oldCapture)
    const preserved = await archive.readFile('original')
    for (let index = 0; index < 3; index++) {
      now++
      await archive.save({ ...oldCapture, id: 'new-' + index, savedAt: now })
    }
    const bounded = await archive.list()
    const evicted = await archive.readFile('original')
    await archive.remove('new-0')
    const removed = await archive.readFile('new-0')
    now += DIAGNOSTICS_ARCHIVE_MAX_AGE_MS
    const expired = await archive.readFile('new-2')
    const pruned = await archive.list()
    return { saved, preserved, bounded: bounded.map(capture => capture.id), evicted, removed, expired, pruned }
  })
  expect(result.saved).toMatchObject([{ id: 'original', byteLength: 12 }])
  expect(result.preserved?.text).toBe('原始记录')
  expect(result.bounded).toEqual(['new-2', 'new-1', 'new-0'])
  expect(result.evicted).toBeNull()
  expect(result.removed).toBeNull()
  expect(result.expired).toBeNull()
  expect(result.pruned).toEqual([])
})
