import {
  browserMetrics,
  installBrowserHarness,
  readOpfs,
  releaseBrowserWrites,
} from '../../e2e/fixtures/browser'
import {
  deterministicBytes,
  replaceRelayHint,
  sha256,
} from '../../e2e/fixtures/process'
import {
  disableCapabilityArtifacts,
  expect,
  navigateToShare,
  test,
  waitUntilComplete,
  waitUntilReady,
} from '../../e2e/fixtures/test'

disableCapabilityArtifacts()

test('real relay and Pion sender admit P2P only after the download gesture', async ({
  page,
  stack,
  baseUrl,
}) => {
  const payload = deterministicBytes(2_500_000, 91)
  const tree = await stack.createTree({ 'p2p.bin': payload })
  const relayProxy = await stack.createRelayProxy()
  const share = await stack.share([tree.root], baseUrl)
  const browserLink = replaceRelayHint(share.link!, relayProxy.url)
  await installBrowserHarness(page, {
    directoryPicker: true,
    pauseBeforeWriteCall: 2,
  })
  await navigateToShare(page, browserLink)
  await waitUntilReady(page, share.process)

  const before = await browserMetrics(page)
  expect(before).toMatchObject({
    signalOffers: 0,
    signalIceCandidates: 0,
    peerConnections: 0,
    offerCalls: 0,
    localIceCandidates: 0,
  })

  await page.getByRole('button', { name: 'Download selected' }).click()
  await expect.poll(async () => (await browserMetrics(page)).offerCalls).toBe(1)
  await expect.poll(async () => (await browserMetrics(page)).signalOffers).toBe(1)
  await expect.poll(async () => (await browserMetrics(page)).localIceCandidates).toBeGreaterThan(0)
  await expect
    .poll(async () => (await browserMetrics(page)).signalIceCandidates)
    .toBeGreaterThan(0)
  await expect
    .poll(async () => page.locator('dt', { hasText: 'Connections' }).locator('..').locator('dd').textContent())
    .toBe('2')

  try {
    relayProxy.cutConnections()
    await expect.poll(async () => (await browserMetrics(page)).connections).toBeGreaterThanOrEqual(2)
  } finally {
    await releaseBrowserWrites(page)
  }
  await waitUntilComplete(page)
  const completed = await browserMetrics(page)
  expect(completed.peerConnections, JSON.stringify(completed)).toBe(1)
  expect(completed.offerCalls).toBe(1)
  expect(completed.output.pickerCalls).toBe(1)
  expect((await readOpfs(page)).find((entry) => entry.path === 'tree/p2p.bin')).toMatchObject({
    size: payload.byteLength,
    sha256: sha256(payload),
  })
})
