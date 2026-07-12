import { readFile } from 'node:fs/promises'

import {
  Uint8ArrayReader,
  Uint8ArrayWriter,
  ZipReader,
} from '@zip.js/zip.js'

import {
  browserMetrics,
  installBrowserHarness,
  readOpfs,
  releaseBrowserWrites,
} from './fixtures/browser'
import { deterministicBytes, sha256 } from './fixtures/process'
import {
  disableCapabilityArtifacts,
  expect,
  expectFragmentErased,
  navigateToShare,
  test,
  waitUntilComplete,
  waitUntilReady,
} from './fixtures/test'

const ZIP64_END_SIGNATURE = Buffer.from([0x50, 0x4b, 0x06, 0x06])
const MAX_ORDERED_REASSEMBLY_BLOCKS = 8
const BACKPRESSURE_OBSERVATION_MS = 750
const PAUSE_ON_FIRST_WRITE_CALL = 1
const INJECTED_TEMPORARY_REMOVAL_FAILURES = 1

disableCapabilityArtifacts()

test('non-FSA download streams Zip64 while reassembling deliberately out-of-order blocks', async ({
  page,
  stack,
  baseUrl,
}) => {
  const first = deterministicBytes(2_100_000, 101)
  const second = deterministicBytes(1_300_000, 131)
  const tree = await stack.createTree(
    {
      'first.bin': first,
      'nested/second.bin': second,
      'empty.bin': new Uint8Array(),
    },
    ['empty-dir'],
  )
  const share = await stack.share([tree.root], baseUrl)
  expect(share.link).toBeDefined()

  await installBrowserHarness(page, {
    disablePickers: true,
    reorderFirstBlockPair: true,
    pauseBeforeWriteCall: PAUSE_ON_FIRST_WRITE_CALL,
    temporaryRemovalFailures: INJECTED_TEMPORARY_REMOVAL_FAILURES,
    rejectPeerConnections: true,
  })
  await navigateToShare(page, share.link!)
  await expectFragmentErased(page)
  await waitUntilReady(page, share.process)

  const beforeClick = await browserMetrics(page)
  expect(beforeClick.fragmentPresentAtFirstSocket).toBe(false)
  expect(beforeClick.requests).toHaveLength(0)
  expect(beforeClick.signalOffers).toBe(0)
  expect(beforeClick.signalIceCandidates).toBe(0)
  expect(beforeClick.peerConnections).toBe(0)

  const downloadRequest = page.waitForEvent('download')
  await page.getByRole('button', { name: 'Download selected' }).click()
  try {
    await expect
      .poll(async () => (await browserMetrics(page)).output.writePaused)
      .toBe(true)
    // Hold the real OPFS write boundary long enough for a loopback sender to fill
    // every incorrectly unbounded queue. Only the scheduler's fixed window may land.
    await page.waitForTimeout(BACKPRESSURE_OBSERVATION_MS)
    const stalled = await browserMetrics(page)
    expect(stalled.output.completedWriteCalls).toBe(0)
    expect(new Set(stalled.completedBlockDelivery).size).toBeLessThanOrEqual(
      MAX_ORDERED_REASSEMBLY_BLOCKS,
    )
    expect(stalled.output.objectUrlsCreated).toBe(0)
  } finally {
    await releaseBrowserWrites(page)
  }
  const download = await downloadRequest
  await waitUntilComplete(page)
  expect(await download.failure()).toBeNull()
  const archivePath = await download.path()
  expect(archivePath).not.toBeNull()
  const archive = await readFile(archivePath!)

  expect(archive.indexOf(ZIP64_END_SIGNATURE)).toBeGreaterThanOrEqual(0)
  const reader = new ZipReader(new Uint8ArrayReader(archive))
  const archived = await reader.getEntries()
  expect(archived.map((entry) => entry.filename)).toEqual([
    'tree/',
    'tree/empty-dir/',
    'tree/empty.bin',
    'tree/first.bin',
    'tree/nested/',
    'tree/nested/second.bin',
  ])
  expect(archived.every((entry) => entry.zip64)).toBe(true)
  expect(archived.every((entry) => entry.compressionMethod === 0)).toBe(true)
  const hashes = new Map<string, string>()
  for (const entry of archived) {
    if (!entry.directory) {
      hashes.set(entry.filename, sha256(await entry.getData(new Uint8ArrayWriter())))
    }
  }
  expect(hashes.get('tree/first.bin')).toBe(sha256(first))
  expect(hashes.get('tree/nested/second.bin')).toBe(sha256(second))
  expect(hashes.get('tree/empty.bin')).toBe(sha256(new Uint8Array()))
  await reader.close()

  const metrics = await browserMetrics(page)
  expect(metrics.completedBlockArrival.length).toBeGreaterThanOrEqual(2)
  expect(metrics.completedBlockDelivery.slice(0, 2)).toEqual([
    metrics.completedBlockArrival[1],
    metrics.completedBlockArrival[0],
  ])
  expect(metrics.output.writeCalls).toBeGreaterThan(4)
  expect(metrics.output.totalWrittenBytes).toBe(archive.byteLength)
  expect(metrics.output.maxWriteBytes).toBeLessThan(archive.byteLength)
  expect(metrics.output.maxConcurrentWrites).toBe(1)
  expect(metrics.output.objectUrlsCreated).toBe(1)
  expect(metrics.output.objectUrlsRevoked).toBe(0)
  expect(metrics.output.temporaryRemovalCalls).toBe(0)

  const buffered = await page
    .locator('dt', { hasText: 'Buffered' })
    .locator('xpath=following-sibling::dd')
    .textContent()
  const maximumBuffered = Number(buffered?.match(/\/\s*(\d+)/u)?.[1])
  expect(maximumBuffered).toBeGreaterThan(0)
  expect(maximumBuffered).toBeLessThanOrEqual(MAX_ORDERED_REASSEMBLY_BLOCKS)
  const staged = await readOpfs(page)
  expect(staged).toHaveLength(1)
  expect(staged[0]).toMatchObject({
    kind: 'file',
    size: archive.byteLength,
    sha256: sha256(archive),
  })
  expect(staged[0]?.path).toMatch(/^\.windshare-download-/u)
  await page.evaluate(() =>
    window.dispatchEvent(new PageTransitionEvent('pagehide', { persisted: false })),
  )
  await expect
    .poll(async () => (await browserMetrics(page)).output.objectUrlsRevoked)
    .toBe(1)
  await expect
    .poll(async () => (await browserMetrics(page)).output.temporaryRemovalFailures)
    .toBe(INJECTED_TEMPORARY_REMOVAL_FAILURES)
  await expect
    .poll(async () => (await browserMetrics(page)).output.temporaryRemovalCalls)
    .toBeGreaterThan(INJECTED_TEMPORARY_REMOVAL_FAILURES)
  await expect.poll(async () => (await readOpfs(page)).length).toBe(0)
})
