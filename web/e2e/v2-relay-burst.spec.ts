import { createHash, randomBytes } from 'node:crypto'
import { readFile } from 'node:fs/promises'
import { expect, test } from '@playwright/test'
import { capabilityUrl, DirectProductStack } from './fixtures/direct-product-stack'
import { decodeDeliveryJoin, type RelayReceiveCapacity } from '../src/transport/relay/delivery-handshake-codec'
import { decodeDeliveryConsumed, type RelayDeliveryConsumed } from '../src/transport/relay/delivery-consumed-codec'

const FILE_BYTES = 17 * 1024 * 1024
const PRODUCTION_BLOCK_BYTES = 1024 * 1024
const DOWNLOAD_TIMEOUT_MILLISECONDS = 20_000

test('a production-size relay burst preserves the session and downloaded bytes', async ({ page }) => {
  const stack = new DirectProductStack('relay-burst-backpressure')
  const payload = randomBytes(FILE_BYTES)
  let relayClosures = 0
  const capacities: RelayReceiveCapacity[] = []
  const consumption: RelayDeliveryConsumed[] = []
  page.on('websocket', socket => {
    if (new URL(socket.url()).pathname !== '/v2/ws') return
    socket.on('close', () => { relayClosures += 1 })
    socket.on('framesent', ({ payload }) => {
      if (!Buffer.isBuffer(payload)) return
      const magic = payload.subarray(0, 4).toString('ascii')
      if (magic === 'WS2J') capacities.push(decodeDeliveryJoin(payload).receiveCapacity)
      if (magic === 'WS2B') consumption.push(decodeDeliveryConsumed(payload))
    })
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
    expect(capacities).toHaveLength(1)
    expect(consumption.length).toBeGreaterThan(1)
    expect(consumption.at(-1)!.totalEncodedBytes).toBeGreaterThan(capacities[0]!.encodedBytes)
    for (let index = 1; index < consumption.length; index += 1) {
      expect(consumption[index]!.totalFrames).toBeGreaterThan(consumption[index - 1]!.totalFrames)
      expect(consumption[index]!.totalEncodedBytes).toBeGreaterThan(consumption[index - 1]!.totalEncodedBytes)
    }
  } finally {
    await stack.dispose()
  }
})
