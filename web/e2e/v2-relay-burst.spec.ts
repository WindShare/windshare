import { createHash, randomBytes } from 'node:crypto'
import { readFile } from 'node:fs/promises'
import { expect, test } from '@playwright/test'
import { capabilityUrl, DirectProductStack } from './fixtures/direct-product-stack'

const FILE_BYTES = 17 * 1024 * 1024
const PRODUCTION_BLOCK_BYTES = 1024 * 1024
const DOWNLOAD_TIMEOUT_MILLISECONDS = 20_000

test('a production-size relay burst preserves the session and downloaded bytes', async ({ page }) => {
  const stack = new DirectProductStack('relay-burst-backpressure')
  const payload = randomBytes(FILE_BYTES)
  let relayClosures = 0
  page.on('websocket', socket => {
    if (new URL(socket.url()).pathname === '/v2/ws') socket.on('close', () => { relayClosures += 1 })
  })
  // The relay must sustain native block geometry on its own. A tiny fixture or
  // a warmed direct lane would hide a queue-overflow/reconnect loop.
  await page.addInitScript(() => {
    Object.defineProperties(window, {
      RTCPeerConnection: { configurable: true, value: undefined },
      showDirectoryPicker: { configurable: true, value: undefined },
      showSaveFilePicker: { configurable: true, value: undefined },
    })
    Object.defineProperty(navigator.storage, 'getDirectory', { configurable: true, value: undefined })
  })
  await stack.start()
  try {
    const file = await stack.createFile('relay-burst.bin', payload)
    const share = await stack.share(file, { blockSizeBytes: PRODUCTION_BLOCK_BYTES })
    await page.goto(capabilityUrl(share))
    const action = page.getByRole('button', { name: 'Download file', exact: true })
    await expect(action).toBeEnabled()
    const downloadStarted = page.waitForEvent('download', { timeout: DOWNLOAD_TIMEOUT_MILLISECONDS })
    await action.click()
    const download = await downloadStarted
    const path = await download.path()
    expect(path).not.toBeNull()
    const received = await readFile(path!)
    expect(received.byteLength).toBe(FILE_BYTES)
    expect(createHash('sha256').update(received).digest('hex'))
      .toBe(createHash('sha256').update(payload).digest('hex'))
    expect(relayClosures).toBe(0)
  } finally {
    await stack.dispose()
  }
})
