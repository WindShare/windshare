import { expect, test, type Page } from '@playwright/test'
import { readFileSync } from 'node:fs'
import type { NetworkRoot } from '../../src/network/node-directory'
import { BROWSER_CONTRACT_HOST_PATH } from './contract-host'

const vectors = JSON.parse(readFileSync(new URL('../../../testdata/network/directory.json', import.meta.url), 'utf8')) as {
  network: string; roots: NetworkRoot[]; entries: { name: string; envelope: unknown }[]
}

async function commit(page: Page, name: string): Promise<boolean> {
  return page.evaluate(async ({ network, roots, envelope }) => {
    const cachePath = '/src/network/directory-cache.ts'
    const trustPath = '/src/network/node-directory.ts'
    const { BrowserDirectoryCache } = await import(/* @vite-ignore */ cachePath) as typeof import('../../src/network/directory-cache')
    const { verifyDirectory } = await import(/* @vite-ignore */ trustPath) as typeof import('../../src/network/node-directory')
    const verified = await verifyDirectory(envelope, network, roots)
    try { await new BrowserDirectoryCache().commit(verified); return true }
    catch { return false }
  }, { network: vectors.network, roots: vectors.roots, envelope: JSON.stringify(vectors.entries.find(entry => entry.name === name)!.envelope) })
}

test('directory trust survives competing tabs, reopen and alias removal', async ({ page, context }) => {
  const other = await context.newPage()
  await Promise.all([page.goto(BROWSER_CONTRACT_HOST_PATH), other.goto(BROWSER_CONTRACT_HOST_PATH)])
  await Promise.all([commit(page, 'valid'), commit(other, 'new_generation')])
  expect(await commit(page, 'older_serial')).toBe(false)
  expect(await commit(other, 'removed')).toBe(true)
  expect(await commit(page, 'reassigned')).toBe(false)
  await other.close()
  await page.reload()
  const record = await page.evaluate(async () => {
    const cachePath = '/src/network/directory-cache.ts'
    const { BrowserDirectoryCache } = await import(/* @vite-ignore */ cachePath) as typeof import('../../src/network/directory-cache')
    return new BrowserDirectoryCache().load()
  })
  expect(record?.cursor).toMatchObject({ generation: 2, serial: 3 })
  expect(record?.aliases.jp1).toBe('wss://jp1.example/v2/ws')
})
