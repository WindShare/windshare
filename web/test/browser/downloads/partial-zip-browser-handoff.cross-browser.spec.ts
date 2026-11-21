import { expect, test, type Download } from '@playwright/test'
import { Uint8ArrayReader, Uint8ArrayWriter, ZipReader } from '@zip.js/zip.js'
import { BROWSER_CONTRACT_HOST_PATH } from '../contract-host'
import { requireOriginPrivateStorage } from '../browser-storage-support'

const HARNESS_PATH = '/test/browser/downloads/partial-zip-browser-handoff-harness.ts'
const NATIVE_HARNESS_PATH = '/test/browser/opfs/opfs-native-harness.ts'

test('exports completed files via ordinary browser download without showSaveFilePicker while preserving continuation', async ({
  page, browserName,
}) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  await requireOriginPrivateStorage(page, browserName)
  const nativeSupported = await page.evaluate(async path => {
    const harness = await import(path) as typeof import('../opfs/opfs-native-harness')
    return harness.probeNativeObjectSupport(window)
  }, NATIVE_HARNESS_PATH)
  // Progressive ZIP recovery requires the Dedicated Worker OPFS sync-access route.
  test.skip(!nativeSupported, `Current ${browserName} test environment lacks Dedicated Worker OPFS`)

  const paused = await page.evaluate(async path => {
    const harness = await import(path) as typeof import('./partial-zip-browser-handoff-harness')
    return harness.createUnallocatedMissingFilePause()
  }, HARNESS_PATH)
  expect(paused).toMatchObject({
    workerStatus: 'CompletedWithErrors',
    lifecycle: 'resumable-receive',
    continuation: 'resume-receive',
    discoveryComplete: false,
    completeFileCount: '1',
    hasPickerApi: false,
  })
  expect(paused.actions).toContain('save-partial')
  expect(paused.actions).toContain('continue')
  expect(paused.actions).toContain('discard')

  const partialDownloadPromise = page.waitForEvent('download')
  const afterPartial = await page.evaluate(async ({ path, operationId }) => {
    const harness = await import(path) as typeof import('./partial-zip-browser-handoff-harness')
    return harness.exportPartialViaBrowserDownload(operationId)
  }, { path: HARNESS_PATH, operationId: paused.operationId })

  const partialDownload = await partialDownloadPromise
  expect(partialDownload.suggestedFilename().endsWith('-partial.zip')).toBe(true)
  const partialFiles = await readZipFiles(await readDownloadBytes(partialDownload))
  expect([...partialFiles.keys()]).toEqual(['windshare/micro-share/keep.bin'])
  expect(partialFiles.get('windshare/micro-share/keep.bin')).toEqual([7, 7, 7, 7, 7, 7])

  expect(afterPartial).toMatchObject({
    lifecycle: 'resumable-receive',
    continuation: 'resume-receive',
  })
  expect(afterPartial.actions).toContain('continue')
  expect(afterPartial.actions).toContain('save-partial')

  const completeDownloadPromise = page.waitForEvent('download')
  const resumed = await page.evaluate(async ({ path, operationId }) => {
    const harness = await import(path) as typeof import('./partial-zip-browser-handoff-harness')
    return harness.continueAndCompleteRetainedDownload(operationId)
  }, { path: HARNESS_PATH, operationId: paused.operationId })

  expect(resumed).toEqual({
    workerStatus: 'Succeeded',
    lifecycle: 'download-started',
    revisionRequests: [resumed.expectedMissingId],
    expectedMissingId: resumed.expectedMissingId,
  })
  const completeDownload = await completeDownloadPromise
  const completeFiles = await readZipFiles(await readDownloadBytes(completeDownload))
  expect([...completeFiles.keys()].sort()).toEqual([
    'windshare/micro-share/keep.bin',
    'windshare/micro-share/vanished.bin',
  ])
  expect(completeFiles.get('windshare/micro-share/keep.bin')).toEqual([7, 7, 7, 7, 7, 7])
  expect(completeFiles.get('windshare/micro-share/vanished.bin')).toEqual([7, 7, 7, 7])
})

test('streams zero-copy File.slice() partial ZIP in browser download and fences continuation during its lease', async ({
  page, browserName,
}) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  await requireOriginPrivateStorage(page, browserName)
  const nativeSupported = await page.evaluate(async path => {
    const harness = await import(path) as typeof import('../opfs/opfs-native-harness')
    return harness.probeNativeObjectSupport(window)
  }, NATIVE_HARNESS_PATH)
  // Progressive ZIP recovery requires the Dedicated Worker OPFS sync-access route.
  test.skip(!nativeSupported, `Current ${browserName} test environment lacks Dedicated Worker OPFS`)

  const paused = await page.evaluate(async path => {
    const harness = await import(path) as typeof import('./partial-zip-browser-handoff-harness')
    return harness.createUnallocatedMissingFilePause()
  }, HARNESS_PATH)

  const partialDownloadPromise = page.waitForEvent('download')
  const started = await page.evaluate(async ({ path, operationId }) => {
    const harness = await import(path) as typeof import('./partial-zip-browser-handoff-harness')
    return harness.startFileBackedPartialExport(operationId)
  }, { path: HARNESS_PATH, operationId: paused.operationId })
  expect(started.scheduledLeaseCount).toBe(1)

  const partialDownload = await partialDownloadPromise
  expect(partialDownload.suggestedFilename()).toBe('large-share-partial.zip')
  const partialFiles = await readZipFiles(await readDownloadBytes(partialDownload))
  expect([...partialFiles.keys()]).toEqual(['windshare/micro-share/keep.bin'])
  expect(partialFiles.get('windshare/micro-share/keep.bin')).toEqual([7, 7, 7, 7, 7, 7])

  const blocked = await page.evaluate(async ({ path, operationId }) => {
    const harness = await import(path) as typeof import('./partial-zip-browser-handoff-harness')
    return harness.attemptContinueWhileFileBackedLeaseActive(operationId)
  }, { path: HARNESS_PATH, operationId: paused.operationId })
  expect(blocked).toEqual({
    rejectedName: 'InvalidStateError',
    retainedLifecycle: 'resumable-receive',
    retainedContinuation: 'resume-receive',
  })

  const released = await page.evaluate(async path => {
    const harness = await import(path) as typeof import('./partial-zip-browser-handoff-harness')
    return harness.releaseFileBackedPartialLease()
  }, HARNESS_PATH)
  expect(released).toBe(1)

  const completeDownloadPromise = page.waitForEvent('download')
  const resumed = await page.evaluate(async ({ path, operationId }) => {
    const harness = await import(path) as typeof import('./partial-zip-browser-handoff-harness')
    return harness.continueAndCompleteRetainedDownload(operationId)
  }, { path: HARNESS_PATH, operationId: paused.operationId })
  expect(resumed.workerStatus).toBe('Succeeded')
  expect(resumed.lifecycle).toBe('download-started')
  const completeDownload = await completeDownloadPromise
  const completeFiles = await readZipFiles(await readDownloadBytes(completeDownload))
  expect([...completeFiles.keys()].sort()).toEqual([
    'windshare/micro-share/keep.bin',
    'windshare/micro-share/vanished.bin',
  ])
})

async function readDownloadBytes(download: Download): Promise<Uint8Array> {
  const stream = await download.createReadStream()
  if (stream === null) throw new Error('Playwright download stream is unavailable')
  const chunks: Buffer[] = []
  for await (const chunk of stream) chunks.push(Buffer.from(chunk))
  return new Uint8Array(Buffer.concat(chunks))
}

async function readZipFiles(bytes: Uint8Array): Promise<Map<string, number[]>> {
  const archive = new ZipReader(new Uint8ArrayReader(bytes), {
    checkSignature: true, useWebWorkers: false,
  })
  try {
    const entries = await archive.getEntries()
    const files = new Map<string, number[]>()
    for (const entry of entries) {
      if (!entry.directory) {
        files.set(entry.filename, [...await entry.getData!(new Uint8ArrayWriter())])
      }
    }
    return files
  } finally { await archive.close() }
}
