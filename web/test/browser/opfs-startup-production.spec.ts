import { writeFile } from 'node:fs/promises'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { expect, test, type Page } from '@playwright/test'
import { build, preview } from 'vite'

const WEB_ROOT = fileURLToPath(new URL('../../', import.meta.url))
const BUNDLE_NAME = 'startup-recovery.js'
const PAGE_NAME = 'startup-recovery.html'
const WORKER_ASSET = /\/assets\/worker-[^/]+\.js$/u
type Harness = typeof import('./opfs/startup-recovery-harness')

test('production worker loading failure retains one retryable ZIP task across attempts and reload', async ({ page }, testInfo) => {
  const outDir = testInfo.outputPath('startup-recovery-build')
  // A real emitted Worker and real durable storage exercise the failed boundary and ownership on retry.
  await build({ root: WEB_ROOT, logLevel: 'error', build: {
    outDir, copyPublicDir: false,
    lib: { entry: join(WEB_ROOT, 'test/browser/opfs/startup-recovery-harness.ts'), formats: ['es'], fileName: () => BUNDLE_NAME },
  } })
  await writeFile(join(outDir, PAGE_NAME), '<!doctype html><title>Startup recovery</title><link rel="icon" href="data:,">')
  const server = await preview({ root: WEB_ROOT, logLevel: 'error', build: { outDir },
    preview: { host: '127.0.0.1', port: 0, strictPort: true } })
  try {
    const address = server.httpServer.address()
    if (address === null || typeof address === 'string') throw new Error('Startup server has no TCP address')
    await page.goto(`http://127.0.0.1:${address.port}/${PAGE_NAME}`)
    let failedRequests = 0
    await page.route(WORKER_ASSET, async route => { failedRequests += 1; await route.abort('internetdisconnected') })
    const operationId = await call(page, 'create')
    const failed = { operationId, state: 'resumable-start', admissions: 0,
      failureStage: 'output_initialization', recovery: 'retryable' }
    expect(await call(page, 'attempt')).toEqual(failed)
    expect(await call(page, 'retry')).toEqual(failed)
    expect(failedRequests).toBe(2)
    await call(page, 'detach')
    await page.reload()
    expect(await call(page, 'inventory')).toMatchObject([{ operationId, continuation: 'resume-start' }])
    await page.evaluate(async id => {
      const modulePath = '/startup-recovery.js'
      const harness = await import(modulePath) as Harness
      await harness.reopen(id)
    }, operationId)
    expect(await call(page, 'attempt')).toEqual(failed)
    await page.unroute(WORKER_ASSET)
    expect(await call(page, 'retry')).toMatchObject({ operationId, state: 'receiving', admissions: 1 })
    expect(await call(page, 'pause')).toMatchObject({ kind: 'resumable-receive', payloadKind: 'opfs-zip' })
    await call(page, 'detach')
    expect(await discard(page, operationId)).toBe('discarded')

    // A failed, still-empty archive must also remain explicitly discardable after a fresh page.
    await page.route(WORKER_ASSET, route => route.abort('internetdisconnected'))
    const activeFailure = await call(page, 'create')
    expect(await call(page, 'attempt')).toMatchObject({ state: 'resumable-start' })
    expect(await discard(page, activeFailure)).toBe('discarded')
    const disposable = await call(page, 'create')
    expect(await call(page, 'attempt')).toMatchObject({ state: 'resumable-start' })
    await call(page, 'detach')
    await page.reload()
    expect(await discard(page, disposable)).toBe('discarded')
    expect((await call(page, 'inventory')).some(value => value.operationId === disposable)).toBe(false)

    const paused = await call(page, 'create')
    expect(await call(page, 'pause')).toMatchObject({ kind: 'resumable-start', reason: 'paused' })
    expect(await discard(page, paused)).toBe('discarded')
  } finally {
    await page.close()
    await server.close()
  }
})

function call<K extends Exclude<keyof Harness, 'reopen' | 'discard'>>(page: Page, name: K): Promise<Awaited<ReturnType<Harness[K]>>> {
  return page.evaluate(async method => {
    const modulePath = '/startup-recovery.js'
    const harness = await import(modulePath) as Harness
    return harness[method]()
  }, name) as Promise<Awaited<ReturnType<Harness[K]>>>
}

function discard(page: Page, operationId: string): Promise<string> {
  return page.evaluate(async id => {
    const modulePath = '/startup-recovery.js'
    const harness = await import(modulePath) as Harness
    return harness.discard(id)
  }, operationId)
}
