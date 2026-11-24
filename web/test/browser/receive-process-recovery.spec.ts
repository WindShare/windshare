import { mkdtemp } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { expect, test, type BrowserContext, type Download } from '@playwright/test'
import { Uint8ArrayReader, Uint8ArrayWriter, ZipReader, type FileEntry } from '@zip.js/zip.js'
import { removePersistentBrowserProfile, requireOriginPrivateStorage } from './browser-storage-support'
import { BROWSER_CONTRACT_HOST_PATH } from './contract-host'

const HARNESS_PATH = '/test/browser/receive-process-recovery-harness.ts'

async function readDownloadBytes(download: Download): Promise<Uint8Array> {
  const stream = await download.createReadStream()
  if (stream === null) throw new Error('Playwright download stream is unavailable')
  const chunks: Buffer[] = []
  for await (const chunk of stream) chunks.push(Buffer.from(chunk))
  return new Uint8Array(Buffer.concat(chunks))
}

async function readZip(bytes: Uint8Array): Promise<Record<string, string>> {
  const reader = new ZipReader(new Uint8ArrayReader(bytes), {
    checkSignature: true,
    useWebWorkers: false,
  })
  try {
    const entries: Record<string, string> = {}
    for (const entry of await reader.getEntries()) {
      if (!entry.directory && 'getData' in entry) {
        const data = await (entry as FileEntry).getData(new Uint8ArrayWriter())
        entries[entry.filename] = new TextDecoder().decode(data)
      }
    }
    return entries
  } finally {
    await reader.close()
  }
}

test('OPFS progressive ZIP and FSA directory tasks recover across a real Chromium process restart', async ({
  browser,
  browserName,
}, testInfo) => {
  const origin = testInfo.project.use.baseURL
  if (typeof origin !== 'string') throw new Error('Process recovery test requires baseURL')
  const contractHostURL = new URL(BROWSER_CONTRACT_HOST_PATH, origin).href
  const profile = await mkdtemp(join(tmpdir(), 'windshare-process-recovery-'))
  const browserType = browser.browserType()
  const ids = {
    databaseName: `windshare-proc-${crypto.randomUUID()}`,
    opfsTaskId: `opfs-proc-${crypto.randomUUID()}`,
    fsaTaskId: `fsa-proc-${crypto.randomUUID()}`,
    fsaSandbox: `fsa-proc-sandbox-${crypto.randomUUID()}`,
  }

  let activeContext: BrowserContext | undefined
  try {
    const firstContext = await browserType.launchPersistentContext(profile, { headless: true })
    activeContext = firstContext
    const firstPage = firstContext.pages()[0] ?? await firstContext.newPage()
    await firstPage.goto(contractHostURL)
    await requireOriginPrivateStorage(firstPage, browserName)
    const crashCut = await firstPage.evaluate(async ({ path, crashIds }) => {
      const harness = await import(path) as typeof import('./receive-process-recovery-harness')
      return harness.startActiveProcessCrashCut(crashIds)
    }, { path: HARNESS_PATH, crashIds: ids })
    expect(crashCut).toEqual({ opfsLifecycle: 'downloading', fsaLifecycle: 'downloading' })

    // Closing a persistent BrowserContext terminates its dedicated Chromium process without
    // running JS session settlement, releasing OS handles and Web Locks only via process exit.
    await firstContext.close()
    activeContext = undefined

    const secondContext = await browserType.launchPersistentContext(profile, { headless: true })
    activeContext = secondContext
    const secondPage = secondContext.pages()[0] ?? await secondContext.newPage()
    await secondPage.goto(contractHostURL)
    await requireOriginPrivateStorage(secondPage, browserName)

    const downloadPromise = secondPage.waitForEvent('download')
    const recovered = await secondPage.evaluate(async ({ path, crashIds }) => {
      const harness = await import(path) as typeof import('./receive-process-recovery-harness')
      return harness.recoverAfterBrowserProcessRestart(crashIds)
    }, { path: HARNESS_PATH, crashIds: ids })
    const download = await downloadPromise

    expect(recovered).toEqual({
      normalizedOpfs: {
        activeElsewhere: false,
        kind: 'paused',
        reason: 'interrupted',
        completedFiles: 1,
        retainedBytes: 34,
      },
      normalizedFsa: {
        activeElsewhere: false,
        kind: 'paused',
        reason: 'interrupted',
        completedFiles: 1,
        retainedBytes: 8,
      },
      opfs: {
        f1CompletedSize: 24,
        f2Ranges: ['0:10'],
        f3Ranges: [],
        settledLifecycle: 'download-started',
      },
      fsa: {
        f1CompletedSize: 4,
        f2Ranges: ['0:4'],
        f3Ranges: [],
        settledLifecycle: 'saved',
        doneBytes: [10, 20, 30, 40],
        partBytes: [1, 2, 3, 4, 5, 6, 7, 8],
        draftBytes: [11, 12, 13, 14, 15, 16],
      },
    })
    expect(download.suggestedFilename()).toBe('bundle.zip')
    expect(await readZip(await readDownloadBytes(download))).toEqual({
      'docs/done.txt': 'done-before-process-exit',
      'docs/part.txt': 'first-part|second-part',
      'docs/uncommitted.txt': 'fresh-after-process-exit',
    })

    await secondContext.close()
    activeContext = undefined
  } finally {
    await activeContext?.close().catch(() => undefined)
    await removePersistentBrowserProfile(profile)
  }
})
