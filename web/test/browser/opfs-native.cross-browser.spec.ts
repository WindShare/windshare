import { expect, test } from '@playwright/test'
import { Uint8ArrayReader, Uint8ArrayWriter, ZipReader } from '@zip.js/zip.js'

import { requireOriginPrivateStorage } from './browser-storage-support'

const HARNESS_PATH = '/test/browser/opfs/opfs-native-harness.ts'
const READER_HARNESS_PATH = '/test/browser/opfs/opfs-reader-harness.ts'

test('recovers a real Worker ZIP cut and finalizes offline without staging another payload object', async ({
  page, context, browserName,
}) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
  const workers: string[] = []
  page.on('worker', worker => workers.push(worker.url()))
  const supported = await page.evaluate(async path => {
    const harness = await import(path) as typeof import('./opfs/opfs-native-harness')
    return harness.probeNativeObjectSupport(window)
  }, HARNESS_PATH)
  // Unsupported engines offer portable output; this test specifically requires the native Worker route.
  test.skip(!supported, `${browserName} does not support native OPFS in a Dedicated Worker`)
  expect(workers.some(url => url.includes('/native-object/support-worker.ts'))).toBe(true)
  const cut = await page.evaluate(async ({ path, key }) => {
    const harness = await import(path) as typeof import('./opfs/opfs-native-harness')
    return harness.createNativeArchiveCut(key)
  }, { path: HARNESS_PATH, key: crypto.randomUUID() })
  expect(cut.rangesBeforeLaterDiscovery).toEqual(['0:2', '3:5'])
  expect(cut.discoveryComplete).toBe(false)
  expect(workers.some(url => url.includes('/native-object/worker.ts'))).toBe(true)
  expect(BigInt(cut.growth[0]!)).toBe(BigInt(cut.firstPayloadOffset) + 5n)

  const competingPage = await context.newPage()
  try {
    await competingPage.goto('/')
    const competing = await competingPage.evaluate(async ({ path, fixture }) => {
      const harness = await import(path) as typeof import('./opfs/opfs-native-harness')
      return harness.competingNativeWriter(fixture)
    }, { path: HARNESS_PATH, fixture: cut.fixture })
    expect(competing).not.toBe('unexpectedly-opened')
    expect(['NoModificationAllowedError', 'InvalidStateError']).toContain(competing)
  } finally { await competingPage.close() }

  const failedCut = await page.evaluate(async path => {
    const harness = await import(path) as typeof import('./opfs/opfs-native-harness')
    return harness.failNativeArchiveMetadataCommit()
  }, HARNESS_PATH)
  expect(failedCut.failure).toBe('injected metadata commit failure')
  expect(failedCut.generation).toBe(cut.generation)
  expect(failedCut.ranges).toEqual(['0:2', '3:5'])
  expect(failedCut.physicallyFlushedUncommittedByte).toBe(99)
  expect(failedCut.traces.slice(-3)).toEqual(['cut-started', 'flush-succeeded', 'stopped'])

  await page.reload()
  const resumed = await page.evaluate(async ({ path, fixture }) => {
    const harness = await import(path) as typeof import('./opfs/opfs-native-harness')
    return harness.resumeNativeArchive(fixture)
  }, { path: HARNESS_PATH, fixture: cut.fixture })
  expect(resumed).toEqual({
    recoveredRanges: ['0:2', '3:5'], discoveryComplete: true, artifactState: 'receiving',
  })

  await page.reload()
  // Worker code is an application asset. Load it before making sender/network access impossible.
  await page.evaluate(async ({ path, fixture }) => {
    const harness = await import(path) as typeof import('./opfs/opfs-native-harness')
    return harness.prepareOfflineNativeFinalization(fixture)
  }, { path: HARNESS_PATH, fixture: cut.fixture })
  await context.setOffline(true)
  const sealed = await page.evaluate(async path => {
    const harness = await import(path) as typeof import('./opfs/opfs-native-harness')
    return harness.finalizeNativeArchiveOffline()
  }, HARNESS_PATH)
  expect(sealed.artifactState).toBe('sealed')
  expect(sealed.objectNames).toEqual(['archive.bin'])
  expect(Number(sealed.exactBytes)).toBe(sealed.bytes.length)

  const reader = new ZipReader(new Uint8ArrayReader(Uint8Array.from(sealed.bytes)), {
    checkSignature: true, useWebWorkers: false,
  })
  try {
    const entries = await reader.getEntries()
    expect(entries.map(entry => entry.filename)).toEqual(['z.bin', 'a.bin', 'empty/'])
    const files = []
    for (const entry of entries) {
      if (!entry.directory) files.push([...await entry.getData!(new Uint8ArrayWriter())])
    }
    expect(files).toEqual([[0, 1, 2, 3, 4], [9, 8, 7]])
  } finally { await reader.close() }
})

test('defers cross-tab cleanup until the active artifact reader releases ownership', async ({
  page, context, browserName,
}) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
  const operationId = `reader-${crypto.randomUUID()}`
  await page.evaluate(async ({ path, operationId }) => {
    const harness = await import(path) as typeof import('./opfs/opfs-reader-harness')
    await harness.retainArtifactReader(operationId)
  }, { path: READER_HARNESS_PATH, operationId })
  const cleaner = await context.newPage()
  try {
    await cleaner.goto('/')
    await cleaner.evaluate(async ({ path, operationId }) => {
      const harness = await import(path) as typeof import('./opfs/opfs-reader-harness')
      harness.beginArtifactCleanup(operationId)
    }, { path: READER_HARNESS_PATH, operationId })
    await expect.poll(() => cleaner.evaluate(async ({ path, operationId }) => {
      const harness = await import(path) as typeof import('./opfs/opfs-reader-harness')
      return harness.observeArtifactCleanup(operationId)
    }, { path: READER_HARNESS_PATH, operationId })).toEqual({
      pendingCleanup: true, cleanupFinished: false, bytes: [1, 2, 3],
    })
    await page.evaluate(async path => {
      const harness = await import(path) as typeof import('./opfs/opfs-reader-harness')
      harness.releaseArtifactReader()
    }, READER_HARNESS_PATH)
    const removed = await cleaner.evaluate(async ({ path, operationId }) => {
      const harness = await import(path) as typeof import('./opfs/opfs-reader-harness')
      return harness.finishArtifactCleanup(operationId)
    }, { path: READER_HARNESS_PATH, operationId })
    expect(removed).toBe(true)
  } finally { await cleaner.close() }
})
