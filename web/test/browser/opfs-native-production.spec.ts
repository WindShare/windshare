import { writeFile } from 'node:fs/promises'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test } from '@playwright/test'
import { build, preview } from 'vite'

const WEB_ROOT = fileURLToPath(new URL('../../', import.meta.url))
const SUPPORT_ENTRY = join(WEB_ROOT, 'src/output/origin-private/native-object/support.ts')
const BUNDLE_NAME = 'native-support.js'
const PAGE_NAME = 'native-support.html'
const CONTENT_SECURITY_POLICY = "default-src 'self'; img-src 'self' data:; worker-src 'self' blob:"

test('production native capability probe runs under a same-origin Worker policy', async ({ page }, testInfo) => {
  const outDir = testInfo.outputPath('native-support-build')
  // Development serving transpiles raw .ts URLs, hiding Worker entry detection failures.
  // Build only this boundary so ordinary contracts cover deployment without another full app build.
  await build({
    root: WEB_ROOT,
    logLevel: 'error',
    build: {
      outDir,
      copyPublicDir: false,
      lib: { entry: SUPPORT_ENTRY, formats: ['es'], fileName: () => BUNDLE_NAME },
    },
  })
  await writeFile(join(outDir, PAGE_NAME), '<!doctype html><html><head><title>Native support</title><link rel="icon" href="data:,"></head><body></body></html>')
  const server = await preview({
    root: WEB_ROOT,
    logLevel: 'error',
    build: { outDir },
    preview: {
      host: '127.0.0.1', port: 0, strictPort: true,
      headers: { 'Content-Security-Policy': CONTENT_SECURITY_POLICY },
    },
  })
  try {
    const address = server.httpServer.address()
    if (address === null || typeof address === 'string') throw new Error('Production probe server has no TCP address')
    const origin = `http://127.0.0.1:${address.port}`
    const workers: string[] = []
    const errors: string[] = []
    page.on('worker', worker => workers.push(worker.url()))
    page.on('pageerror', error => errors.push(error.message))
    page.on('console', message => {
      if (message.type() === 'error') errors.push(message.text())
    })
    const response = await page.goto(`${origin}/${PAGE_NAME}`)
    expect(response?.headers()['content-security-policy']).toBe(CONTENT_SECURITY_POLICY)
    const supported = await page.evaluate(async path => {
      const probe = await import(path) as typeof import('../../src/output/origin-private/native-object/support')
      return probe.probeNativeObjectSupport(window)
    }, `/${BUNDLE_NAME}`)
    // Chromium is required here: a failed capability probe must fail, never skip this regression.
    expect(supported).toBe(true)
    expect(workers).toHaveLength(1)
    expect(new URL(workers[0]!).origin).toBe(origin)
    expect(new URL(workers[0]!).pathname).toMatch(/\.js$/u)
    expect(errors).toEqual([])
  } finally {
    await page.close()
    await server.close()
  }
})
