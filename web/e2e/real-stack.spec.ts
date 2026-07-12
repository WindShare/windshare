import {
  browserMetrics,
  installBrowserHarness,
  readOpfs,
  releaseBrowserWrites,
} from './fixtures/browser'
import {
  deterministicBytes,
  E2E_BLOCK_SIZE,
  replaceRelayHint,
  sha256,
} from './fixtures/process'
import {
  disableCapabilityArtifacts,
  expect,
  expectFragmentErased,
  navigateToShare,
  submitSeparateKey,
  test,
  waitUntilComplete,
  waitUntilReady,
} from './fixtures/test'

const POST_ABORT_QUIESCENCE_MS = 500
const PAUSE_ON_SECOND_WRITE_CALL = 2

disableCapabilityArtifacts()

test('split-key FSA transfer materializes only the selected tree with exact hashes', async ({
  page,
  stack,
  baseUrl,
}) => {
  const keep = deterministicBytes(180_000, 11)
  const note = deterministicBytes(75_000, 29)
  const skipped = deterministicBytes(90_000, 47)
  const tree = await stack.createTree(
    {
      'keep.bin': keep,
      'nested/note.bin': note,
      'skip.bin': skipped,
      'empty.txt': new Uint8Array(),
    },
    ['empty-dir'],
  )
  const share = await stack.share([tree.root], baseUrl, { splitKey: true })
  expect(share.bareLink).toBeDefined()
  expect(share.key).toBeDefined()

  await installBrowserHarness(page, { directoryPicker: true, rejectPeerConnections: true })
  await navigateToShare(page, share.bareLink!)
  await expectFragmentErased(page)
  const keyInput = page.getByLabel('Separate key')
  await expect(keyInput).toBeVisible()
  await submitSeparateKey(page, share.key!)
  await waitUntilReady(page, share.process)

  const beforeClick = await browserMetrics(page)
  expect(beforeClick.fragmentPresentAtFirstSocket).toBe(false)
  expect(beforeClick.requests).toHaveLength(0)
  expect(beforeClick.signalOffers).toBe(0)
  expect(beforeClick.signalIceCandidates).toBe(0)
  expect(beforeClick.peerConnections).toBe(0)
  expect(beforeClick.offerCalls).toBe(0)
  expect(beforeClick.localIceCandidates).toBe(0)

  const skippedFile = page.getByRole('checkbox', { name: /skip\.bin/u })
  await skippedFile.focus()
  await page.keyboard.press('Space')
  await expect(skippedFile).not.toBeChecked()
  await waitUntilReady(page, share.process)
  const downloadButton = page.getByRole('button', { name: 'Download selected' })
  await expect(downloadButton).toBeEnabled()
  await downloadButton.focus()
  await page.keyboard.press('Enter')
  await waitUntilComplete(page)
  await expect(page.getByRole('status').filter({ hasText: 'Download complete.' })).toBeFocused()

  const entries = await readOpfs(page)
  expect(entries.map((entry) => `${entry.kind}:${entry.path}`)).toEqual([
    'directory:tree',
    'directory:tree/empty-dir',
    'file:tree/empty.txt',
    'file:tree/keep.bin',
    'directory:tree/nested',
    'file:tree/nested/note.bin',
  ])
  const files = new Map(
    entries
      .filter((entry) => entry.kind === 'file')
      .map((entry) => [entry.path, entry] as const),
  )
  expect(files.get('tree/keep.bin')).toMatchObject({ size: keep.byteLength, sha256: sha256(keep) })
  expect(files.get('tree/nested/note.bin')).toMatchObject({
    size: note.byteLength,
    sha256: sha256(note),
  })
  expect(files.get('tree/empty.txt')).toMatchObject({
    size: 0,
    sha256: sha256(new Uint8Array()),
  })
  expect(files.has('tree/skip.bin')).toBe(false)

  const completed = await browserMetrics(page)
  expect(completed.requests.length).toBeGreaterThan(0)
  expect(completed.output.pickerCalls).toBe(1)
  expect(completed.output.maxConcurrentWrites).toBe(1)
})

test('a severed browser relay rejoins and resumes without requesting the persisted block', async ({
  page,
  stack,
  baseUrl,
}) => {
  const payload = deterministicBytes(2_500_000, 73)
  const tree = await stack.createTree({ 'resume.bin': payload })
  const relayProxy = await stack.createRelayProxy()
  const share = await stack.share([tree.root], baseUrl)
  expect(share.link).toBeDefined()
  const browserLink = replaceRelayHint(share.link!, relayProxy.url)

  await installBrowserHarness(page, {
    directoryPicker: true,
    pauseBeforeWriteCall: PAUSE_ON_SECOND_WRITE_CALL,
    rejectPeerConnections: true,
  })
  await navigateToShare(page, browserLink)
  await expectFragmentErased(page)
  await waitUntilReady(page, share.process)

  const beforeClick = await browserMetrics(page)
  expect(beforeClick.fragmentPresentAtFirstSocket).toBe(false)
  expect(beforeClick.requests).toHaveLength(0)
  expect(beforeClick.signalOffers).toBe(0)
  expect(beforeClick.signalIceCandidates).toBe(0)

  await page.getByRole('button', { name: 'Download selected' }).click()
  let persistedIndices: number[]
  try {
    await expect
      .poll(async () => (await browserMetrics(page)).output.completedWriteCalls, {
        message: 'the relay cut must occur only after a block reaches the real OPFS handle',
      })
      .toBeGreaterThan(0)
    await expect
      .poll(async () => (await browserMetrics(page)).output.writePaused)
      .toBe(true)
    await expect
      .poll(async () => (await browserMetrics(page)).output.completedPositionalWrites.length)
      .toBeGreaterThan(0)
    const beforeCut = await browserMetrics(page)
    expect(beforeCut.completedBlockDelivery.length).toBeGreaterThan(0)
    persistedIndices = beforeCut.output.completedPositionalWrites.map(({ position, size }) => {
      expect(position % E2E_BLOCK_SIZE).toBe(0)
      expect(size).toBeGreaterThan(0)
      return position / E2E_BLOCK_SIZE
    })
    expect(persistedIndices.length).toBeGreaterThan(0)
    relayProxy.cutConnections()
  } finally {
    await releaseBrowserWrites(page)
  }
  await expect
    .poll(
      async () =>
        (await browserMetrics(page)).requests.filter((event) => event.connection > 1).length,
      {
        message: `receiver did not request through a replacement relay; sender stderr=${share.process.stderr}`,
        timeout: 20_000,
      },
    )
    .toBeGreaterThan(0)
  await expect
    .poll(async () => (await browserMetrics(page)).connections, {
      timeout: 20_000,
    })
    .toBeGreaterThanOrEqual(2)
  await waitUntilComplete(page)

  const completed = await browserMetrics(page)
  expect(completed.connections).toBeGreaterThanOrEqual(2)
  const resumedIndices = completed.requests
    .filter((event) => event.connection > 1)
    .flatMap((event) => event.indices)
  expect(resumedIndices.length).toBeGreaterThan(0)
  for (const persistedIndex of persistedIndices) {
    expect(resumedIndices).not.toContain(persistedIndex)
  }

  const entries = await readOpfs(page)
  expect(entries.find((entry) => entry.path === 'tree/resume.bin')).toMatchObject({
    kind: 'file',
    size: payload.byteLength,
    sha256: sha256(payload),
  })
})

test('stopping a real FSA transfer removes partial output without reconnecting', async ({
  page,
  stack,
  baseUrl,
}) => {
  const payload = deterministicBytes(2_500_000, 151)
  const tree = await stack.createTree({ 'abort.bin': payload })
  const share = await stack.share([tree.root], baseUrl)
  expect(share.link).toBeDefined()

  await installBrowserHarness(page, {
    directoryPicker: true,
    pauseBeforeWriteCall: PAUSE_ON_SECOND_WRITE_CALL,
    rejectPeerConnections: true,
  })
  await navigateToShare(page, share.link!)
  await waitUntilReady(page, share.process)
  await page.getByRole('button', { name: 'Download selected' }).click()
  let connectionCountBeforeStop: number
  try {
    await expect
      .poll(async () => (await browserMetrics(page)).output.writePaused)
      .toBe(true)
    const beforeStop = await browserMetrics(page)
    expect(beforeStop.output.completedWriteCalls).toBeGreaterThan(0)
    connectionCountBeforeStop = beforeStop.connections

    const stop = page.getByRole('button', { name: 'Stop download' })
    await stop.focus()
    await page.keyboard.press('Enter')
  } finally {
    await releaseBrowserWrites(page)
  }
  const stopped = page
    .getByRole('status')
    .filter({ hasText: 'Download stopped and partial output cleaned up.' })
  await stopped.waitFor()
  await expect(stopped).toBeFocused()
  await expect.poll(async () => (await readOpfs(page)).length).toBe(0)
  await expect
    .poll(async () => (await browserMetrics(page)).relayCloseEvents)
    .toBeGreaterThan(0)
  await page.waitForTimeout(POST_ABORT_QUIESCENCE_MS)

  const afterStop = await browserMetrics(page)
  expect(afterStop.connections).toBe(connectionCountBeforeStop)
  expect(afterStop.output.objectUrlsCreated).toBe(0)
})
