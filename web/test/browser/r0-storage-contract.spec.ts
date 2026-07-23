import { mkdtemp } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { expect, test } from '@playwright/test'

import {
  removePersistentBrowserProfile,
  requireOriginPrivateStorage,
} from './browser-storage-support'

interface OriginStorage extends StorageManager {
  getDirectory(): Promise<FileSystemDirectoryHandle>
}

const CHECKPOINT_BYTES = Uint8Array.of(0x57, 0x53, 0x32, 0x00, 0xff)

test('OPFS checkpoint survives reload and publishes only after reopen verification', async ({
  browserName,
  page,
}) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
  const sandboxName = await page.evaluate(async (checkpointBytes) => {
    const root = await (navigator.storage as OriginStorage).getDirectory()
    const name = `r0-${crypto.randomUUID()}`
    const sandbox = await root.getDirectoryHandle(name, { create: true })
    const dataHandle = await sandbox.getFileHandle('output.pending', { create: true })
    const dataWriter = await dataHandle.createWritable()
    await dataWriter.write(Uint8Array.from(checkpointBytes))
    await dataWriter.close()
    const journalHandle = await sandbox.getFileHandle('journal.pending', { create: true })
    const journalWriter = await journalHandle.createWritable()
    await journalWriter.write('checkpoint-1')
    await journalWriter.close()
    return name
  }, [...CHECKPOINT_BYTES])

  await page.reload()
  const reopened = await page.evaluate(async ({ name, checkpointBytes }) => {
    const root = await (navigator.storage as OriginStorage).getDirectory()
    const sandbox = await root.getDirectoryHandle(name)
    const data = await (await sandbox.getFileHandle('output.pending')).getFile()
    const journal = await (await sandbox.getFileHandle('journal.pending')).getFile()
    let publishedBeforeVerification = true
    try {
      await sandbox.getFileHandle('published.marker')
    } catch (error) {
      publishedBeforeVerification = (error as DOMException).name !== 'NotFoundError'
    }
    const actual = [...new Uint8Array(await data.arrayBuffer())]
    const verified =
      actual.length === checkpointBytes.length &&
      actual.every((value, index) => value === checkpointBytes[index]) &&
      (await journal.text()) === 'checkpoint-1'
    if (verified) {
      // Publication follows a fresh handle lookup and full byte verification;
      // an in-memory success from before reload is intentionally insufficient.
      const marker = await sandbox.getFileHandle('published.marker', { create: true })
      const markerWriter = await marker.createWritable()
      await markerWriter.write('verified')
      await markerWriter.close()
    }
    return { actual, publishedBeforeVerification, verified }
  }, { name: sandboxName, checkpointBytes: [...CHECKPOINT_BYTES] })

  expect(reopened).toEqual({
    actual: [...CHECKPOINT_BYTES],
    publishedBeforeVerification: false,
    verified: true,
  })

  await page.reload()
  const durableMarker = await page.evaluate(async (name) => {
    const root = await (navigator.storage as OriginStorage).getDirectory()
    const sandbox = await root.getDirectoryHandle(name)
    const marker = await (await sandbox.getFileHandle('published.marker')).getFile()
    const value = await marker.text()
    await root.removeEntry(name, { recursive: true })
    return value
  }, sandboxName)
  expect(durableMarker).toBe('verified')
})

test('a direct stream cannot roll back bytes emitted before failure', async ({ page }) => {
  await page.goto('/')
  const result = await page.evaluate(async () => {
    const emitted: number[] = []
    const output = new WritableStream<Uint8Array>({
      write(chunk) {
        if (chunk[0] === 2) {
          throw new Error('injected later-file failure')
        }
        emitted.push(...chunk)
      },
    })
    const writer = output.getWriter()
    await writer.write(Uint8Array.of(1))
    let failed = false
    try {
      await writer.write(Uint8Array.of(2))
    } catch {
      failed = true
    }
    return {
      emitted,
      failed,
      durability: 'none',
      fileFailureIsolation: false,
      requiredOutcome: 'aborted',
    }
  })

  expect(result).toEqual({
    emitted: [1],
    failed: true,
    durability: 'none',
    fileFailureIsolation: false,
    requiredOutcome: 'aborted',
  })
})

test('OPFS data remains visible after a fresh browser process opens the same profile', async (
  { browser, browserName, page },
  testInfo,
) => {
  if (!browser.isConnected()) {
    throw new Error('Playwright worker browser is unavailable')
  }
  const storageOrigin = testInfo.project.use.baseURL
  if (typeof storageOrigin !== 'string') {
    throw new Error('R0 storage contract requires a configured baseURL')
  }
  await page.goto(storageOrigin)
  await requireOriginPrivateStorage(page, browserName)

  const profile = await mkdtemp(join(tmpdir(), 'windshare-r0-opfs-'))
  const browserType = browser.browserType()
  expect(browserType.name()).toBe(browserName)
  try {
    const firstContext = await browserType.launchPersistentContext(profile, { headless: true })
    const firstPage = firstContext.pages()[0] ?? (await firstContext.newPage())
    await firstPage.goto(storageOrigin)
    await requireOriginPrivateStorage(firstPage, browserName)
    await firstPage.evaluate(async (checkpointBytes) => {
      const root = await (navigator.storage as OriginStorage).getDirectory()
      const handle = await root.getFileHandle('process-restart.bin', { create: true })
      const writer = await handle.createWritable()
      await writer.write(Uint8Array.from(checkpointBytes))
      await writer.close()
    }, [...CHECKPOINT_BYTES])
    await firstContext.close()

    const secondContext = await browserType.launchPersistentContext(profile, { headless: true })
    const secondPage = secondContext.pages()[0] ?? (await secondContext.newPage())
    await secondPage.goto(storageOrigin)
    const reopened = await secondPage.evaluate(async () => {
      const root = await (navigator.storage as OriginStorage).getDirectory()
      const file = await (await root.getFileHandle('process-restart.bin')).getFile()
      const value = [...new Uint8Array(await file.arrayBuffer())]
      await root.removeEntry('process-restart.bin')
      return value
    })
    await secondContext.close()
    expect(reopened).toEqual([...CHECKPOINT_BYTES])
  } finally {
    await removePersistentBrowserProfile(profile)
  }
})
