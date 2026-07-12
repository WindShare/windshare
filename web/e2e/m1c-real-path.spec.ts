import type { Browser, BrowserContext, Page } from '@playwright/test'

import { P2P_CONNECT_TIMEOUT_MS, SMALL_SHARE_BYTES } from '../src/connectivity'
import {
  browserMetrics,
  installBrowserHarness,
  readOpfs,
  releaseBrowserWrites,
} from './fixtures/browser'
import {
  installM1CPathHarness,
  m1cPathMetrics,
  type M1CPathMetrics,
} from './fixtures/m1c-path'
import {
  deterministicBytes,
  replaceRelayHint,
  settleCleanupTasks,
  sha256,
} from './fixtures/process'
import {
  disableCapabilityArtifacts,
  expect,
  expectFragmentErased,
  navigateToShare,
  test,
  waitUntilComplete,
  waitUntilReady,
} from './fixtures/test'

const LARGE_SELECTED_BYTES = SMALL_SHARE_BYTES + 512 * 1024
const SMALL_SELECTED_BYTES = 4 * 1024 * 1024
const FALLBACK_PEER_DELAY_MS = P2P_CONNECT_TIMEOUT_MS + 1_200
const NEGOTIATION_RECONNECT_DELAY_MS = 2_000
const FALLBACK_EARLY_TOLERANCE_MS = 750
const PAUSE_ON_SECOND_WRITE = 2

disableCapabilityArtifacts()

async function expectFile(page: Page, path: string, data: Uint8Array): Promise<void> {
  expect((await readOpfs(page)).find((entry) => entry.path === path)).toMatchObject({
    kind: 'file',
    size: data.byteLength,
    sha256: sha256(data),
  })
}

async function expectNoPreGestureActivity(page: Page): Promise<void> {
  const relay = await browserMetrics(page)
  const peer = await m1cPathMetrics(page)
  expect(relay).toMatchObject({
    requests: [],
    signalOffers: 0,
    signalIceCandidates: 0,
    peerConnections: 0,
    offerCalls: 0,
    localIceCandidates: 0,
  })
  expect(peer).toMatchObject({
    peerConnections: 0,
    openedChannels: 0,
    closedChannels: 0,
    remoteDescriptions: 0,
    completedRemoteDescriptions: 0,
    terminalIntents: 0,
    rtcRequests: [],
    rtcCompletedBlocks: [],
    rtcErrorFrames: 0,
  })
}

type PeerLifecycleCardinality = Pick<
  M1CPathMetrics,
  'peerConnections' | 'openedChannels' | 'closedChannels'
>

async function expectCleanPeerLifecycle(
  page: Page,
  expected: PeerLifecycleCardinality,
): Promise<M1CPathMetrics> {
  await expect.poll(async () => {
    const metrics = await m1cPathMetrics(page)
    return {
      peerConnections: metrics.peerConnections,
      openedChannels: metrics.openedChannels,
      closedChannels: metrics.closedChannels,
      terminalIntents: metrics.terminalIntents,
      rtcErrorFrames: metrics.rtcErrorFrames,
    }
  }, { timeout: 20_000 }).toEqual({
    ...expected,
    terminalIntents: 0,
    rtcErrorFrames: 0,
  })
  return await m1cPathMetrics(page)
}

async function openReceiver(
  page: Page,
  capabilityUrl: string,
  sender: Parameters<typeof waitUntilReady>[1],
  options: Parameters<typeof installBrowserHarness>[1],
  peerDelayMs = 0,
): Promise<void> {
  await installBrowserHarness(page, options)
  await installM1CPathHarness(page, { remoteDescriptionDelayMs: peerDelayMs })
  await navigateToShare(page, capabilityUrl)
  await expectFragmentErased(page)
  await waitUntilReady(page, sender)
}

test('large selection is P2P-first with exact selection and no relay payload', async ({
  page,
  stack,
  baseUrl,
}) => {
  const selected = deterministicBytes(LARGE_SELECTED_BYTES, 181)
  const skipped = deterministicBytes(128 * 1024, 191)
  const tree = await stack.createTree({ 'large.bin': selected, 'skip.bin': skipped })
  const share = await stack.share([tree.root], baseUrl)
  await openReceiver(page, share.link!, share.process, { directoryPicker: true })
  await expectNoPreGestureActivity(page)

  await page.getByRole('checkbox', { name: /skip\.bin/u }).uncheck()
  await waitUntilReady(page, share.process)
  await page.getByRole('button', { name: 'Download selected' }).click()
  await expect
    .poll(async () => (await m1cPathMetrics(page)).rtcRequests.flat().length)
    .toBeGreaterThan(0)
  await waitUntilComplete(page)

  const relay = await browserMetrics(page)
  const peer = await expectCleanPeerLifecycle(page, {
    peerConnections: 1,
    openedChannels: 1,
    closedChannels: 1,
  })
  expect(relay).toMatchObject({
    requests: [],
    blockArrival: [],
    completedBlockArrival: [],
    blockDelivery: [],
    completedBlockDelivery: [],
  })
  expect(peer).toMatchObject({
    remoteDescriptions: 1,
    completedRemoteDescriptions: 1,
  })
  expect(peer.rtcCompletedBlocks.length).toBeGreaterThan(0)
  await expectFile(page, 'tree/large.bin', selected)
  expect((await readOpfs(page)).some((entry) => entry.path === 'tree/skip.bin')).toBe(false)
})

test('forced fallback hot-joins P2P, uses both paths, and reconnects during transfer', async ({
  page,
  stack,
  baseUrl,
}) => {
  const selected = deterministicBytes(LARGE_SELECTED_BYTES, 211)
  const skipped = deterministicBytes(96 * 1024, 223)
  const tree = await stack.createTree({ 'fallback.bin': selected, 'skip.bin': skipped })
  const proxy = await stack.createRelayProxy()
  const share = await stack.share([tree.root], baseUrl)
  await openReceiver(
    page,
    replaceRelayHint(share.link!, proxy.url),
    share.process,
    { directoryPicker: true, pauseBeforeWriteCall: PAUSE_ON_SECOND_WRITE },
    FALLBACK_PEER_DELAY_MS,
  )
  await page.getByRole('checkbox', { name: /skip\.bin/u }).uncheck()
  await waitUntilReady(page, share.process)
  await expectNoPreGestureActivity(page)

  const started = Date.now()
  await page.getByRole('button', { name: 'Download selected' }).click()
  await expect
    .poll(async () => (await browserMetrics(page)).requests.length, { timeout: 20_000 })
    .toBeGreaterThan(0)
  expect(Date.now() - started).toBeGreaterThanOrEqual(
    P2P_CONNECT_TIMEOUT_MS - FALLBACK_EARLY_TOLERANCE_MS,
  )
  await expect.poll(async () => (await browserMetrics(page)).output.writePaused).toBe(true)
  await expect
    .poll(async () => (await browserMetrics(page)).completedBlockDelivery.length)
    .toBeGreaterThan(0)
  expect(await m1cPathMetrics(page)).toMatchObject({
    remoteDescriptions: 1,
    completedRemoteDescriptions: 0,
    openedChannels: 0,
    rtcRequests: [],
  })
  await expect
    .poll(async () => (await m1cPathMetrics(page)).openedChannels, { timeout: 20_000 })
    .toBe(1)
  expect(await m1cPathMetrics(page)).toMatchObject({
    remoteDescriptions: 1,
    completedRemoteDescriptions: 1,
  })

  proxy.cutConnections()
  await expect
    .poll(async () => (await browserMetrics(page)).connections, { timeout: 20_000 })
    .toBe(2)
  expect((await m1cPathMetrics(page)).peerConnections).toBe(1)
  await releaseBrowserWrites(page)
  await expect
    .poll(async () => (await m1cPathMetrics(page)).rtcRequests.flat().length)
    .toBeGreaterThan(0)
  await waitUntilComplete(page)

  const relay = await browserMetrics(page)
  const peer = await expectCleanPeerLifecycle(page, {
    peerConnections: 1,
    openedChannels: 1,
    closedChannels: 1,
  })
  expect(relay).toMatchObject({ connections: 2 })
  expect(relay.completedBlockDelivery.length).toBeGreaterThan(0)
  expect(peer).toMatchObject({
    remoteDescriptions: 1,
    completedRemoteDescriptions: 1,
  })
  expect(peer.rtcCompletedBlocks.length).toBeGreaterThan(0)
  await expectFile(page, 'tree/fallback.bin', selected)
  expect((await readOpfs(page)).some((entry) => entry.path === 'tree/skip.bin')).toBe(false)
})

test('small selection starts relay first and reconnects while P2P negotiates', async ({
  page,
  stack,
  baseUrl,
}) => {
  const payload = deterministicBytes(SMALL_SELECTED_BYTES, 239)
  const tree = await stack.createTree({ 'negotiation-reconnect.bin': payload })
  const proxy = await stack.createRelayProxy()
  const share = await stack.share([tree.root], baseUrl)
  await openReceiver(
    page,
    replaceRelayHint(share.link!, proxy.url),
    share.process,
    { directoryPicker: true, pauseBeforeWriteCall: PAUSE_ON_SECOND_WRITE },
    NEGOTIATION_RECONNECT_DELAY_MS,
  )
  await expectNoPreGestureActivity(page)

  await page.getByRole('button', { name: 'Download selected' }).click()
  await expect.poll(async () => (await browserMetrics(page)).requests.length).toBeGreaterThan(0)
  await expect.poll(async () => (await browserMetrics(page)).output.writePaused).toBe(true)
  await expect.poll(async () => (await browserMetrics(page)).signalOffers).toBe(1)
  await expect.poll(async () => (await m1cPathMetrics(page)).remoteDescriptions).toBe(1)
  expect(await m1cPathMetrics(page)).toMatchObject({
    peerConnections: 1,
    openedChannels: 0,
    remoteDescriptions: 1,
    completedRemoteDescriptions: 0,
  })
  proxy.cutConnections()

  await expect
    .poll(async () => (await browserMetrics(page)).connections, { timeout: 20_000 })
    .toBe(2)
  await expect
    .poll(async () => (await m1cPathMetrics(page)).peerConnections, { timeout: 20_000 })
    .toBe(2)
  await expect
    .poll(async () => (await m1cPathMetrics(page)).openedChannels, { timeout: 20_000 })
    .toBe(1)
  await releaseBrowserWrites(page)
  await waitUntilComplete(page)

  const relay = await browserMetrics(page)
  const peer = await expectCleanPeerLifecycle(page, {
    peerConnections: 2,
    openedChannels: 1,
    closedChannels: 2,
  })
  expect(relay).toMatchObject({ connections: 2 })
  expect(relay.completedBlockDelivery.length).toBeGreaterThan(0)
  expect(peer).toMatchObject({
    remoteDescriptions: 2,
    completedRemoteDescriptions: 1,
  })
  await expectFile(page, 'tree/negotiation-reconnect.bin', payload)
})

async function newReceiverPage(
  browser: Browser,
  capabilityUrl: string,
  sender: Parameters<typeof waitUntilReady>[1],
  contexts: BrowserContext[],
  pauseBeforeWriteCall = 0,
): Promise<Page> {
  const context = await browser.newContext()
  // A partially initialized receiver still owns native browser resources.
  contexts.push(context)
  const page = await context.newPage()
  await openReceiver(
    page,
    capabilityUrl,
    sender,
    { directoryPicker: true, pauseBeforeWriteCall },
  )
  return page
}

async function runWithReceiverContexts(
  operation: (contexts: BrowserContext[]) => Promise<void>,
): Promise<void> {
  const contexts: BrowserContext[] = []
  const failures: unknown[] = []
  try {
    await operation(contexts)
  } catch (error) {
    failures.push(error)
  }
  try {
    await settleCleanupTasks(
      [...contexts].reverse().map((context) => context.close()),
      'M1c receiver contexts',
    )
  } catch (error) {
    failures.push(error)
  }
  if (failures.length === 1) throw failures[0]
  if (failures.length > 1) {
    throw new AggregateError(failures, 'M1c receiver scenario and cleanup both failed')
  }
}

test('a slow failed receiver cannot starve or tear down sibling receivers', async ({
  browser,
  stack,
  baseUrl,
}) => {
  const payload = deterministicBytes(2_500_000, 251)
  const tree = await stack.createTree({ 'fanout.bin': payload })
  const share = await stack.share([tree.root], baseUrl)
  await runWithReceiverContexts(async (contexts) => {
    const slow = await newReceiverPage(browser, share.link!, share.process, contexts, 1)
    const fast = await newReceiverPage(browser, share.link!, share.process, contexts)

    await slow.getByRole('button', { name: 'Download selected' }).click()
    await expect.poll(async () => (await browserMetrics(slow)).output.writePaused).toBe(true)
    await fast.getByRole('button', { name: 'Download selected' }).click()
    await waitUntilComplete(fast)
    await expectFile(fast, 'tree/fanout.bin', payload)

    await slow.getByRole('button', { name: 'Stop download' }).click()
    await releaseBrowserWrites(slow)
    await slow
      .getByRole('status')
      .filter({ hasText: 'Download stopped and partial output cleaned up.' })
      .waitFor()

    const afterFailure = await newReceiverPage(browser, share.link!, share.process, contexts)
    await afterFailure.getByRole('button', { name: 'Download selected' }).click()
    await waitUntilComplete(afterFailure)
    await expectFile(afterFailure, 'tree/fanout.bin', payload)
  })
})
