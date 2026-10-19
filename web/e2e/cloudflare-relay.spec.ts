import { execFile } from 'node:child_process'
import { createHash, randomBytes } from 'node:crypto'
import { once } from 'node:events'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'
import { expect, test } from '@playwright/test'
import { createServer } from 'vite'

import { DirectProcess } from './fixtures/direct-process'

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../..')
const FILE_BYTES = 17 * 1024 * 1024
const BUILD_TIMEOUT_MILLISECONDS = 120_000
const CLOUD_TIMEOUT_MILLISECONDS = 120_000
const PROGRESS_BYTES = 4 * 1024 * 1024

test('saved registration credentials enable a CLI-to-Cloudflare browser download', async ({ page }, testInfo) => {
  const configPath = process.env.WINDSHARE_RELAY_CONFIG
  if (!configPath) throw new Error('Set WINDSHARE_RELAY_CONFIG to a saved private relay configuration')
  const config = JSON.parse(await readFile(configPath, 'utf8')) as {
    relays: { url: string; registration_token?: string }[]
  }
  if (config.relays.length !== 1 || !config.relays[0]?.registration_token) {
    throw new Error('This evidence run requires one credentialed Cloudflare relay candidate')
  }
  const candidate = config.relays[0]
  const relay = new URL(candidate.url)
  const output = testInfo.outputPath('run')
  await mkdir(output, { recursive: true })
  const executable = join(output, process.platform === 'win32' ? 'wind.exe' : 'wind')
  await promisify(execFile)('go', ['build', '-o', executable, './cmd/wind'], {
    cwd: REPOSITORY_ROOT,
    env: { ...process.env, GOWORK: 'off', GOTOOLCHAIN: 'local' },
    timeout: BUILD_TIMEOUT_MILLISECONDS,
    windowsHide: true,
  })
  const payload = randomBytes(FILE_BYTES)
  const source = join(output, 'cloudflare.bin')
  await writeFile(source, payload)
  const vite = await createServer({
    root: join(REPOSITORY_ROOT, 'web'),
    cacheDir: join(output, 'vite-cache'),
    logLevel: 'error',
    server: { host: '127.0.0.1', port: 0, strictPort: true, hmr: false, watch: null },
  })
  let sender: DirectProcess | undefined
  const sockets: string[] = []
  let receivedRelayBytes = 0
  let nextProgressBytes = PROGRESS_BYTES
  const browserErrors: string[] = []
  page.on('pageerror', error => browserErrors.push(error.message))
  try {
    const listener = vite.httpServer
    if (!listener) throw new Error('Vite listener unavailable')
    const listening = once(listener, 'listening')
    listener.listen(0, '127.0.0.1')
    await listening
    const address = listener.address()
    if (!address || typeof address === 'string') throw new Error('Vite address unavailable')
    const senderTrace = join(output, 'sender.ndjson')
    sender = new DirectProcess(executable, [
      'share', source, '--front-url', `http://127.0.0.1:${address.port}`, '--trace', senderTrace, '--browser-trace',
    ], {
      cwd: REPOSITORY_ROOT,
      environment: process.env,
      operationId: 'cloudflare-saved-relay-share',
      disclosure: { stdout: 'capability', stderr: 'private' },
    })
    const ready = await sender.waitFor('stdout', /^Link: (.+)$/mu, { timeoutMilliseconds: CLOUD_TIMEOUT_MILLISECONDS })
    const capability = ready[1]!
    sender.consumeReadiness()
    expect(new URL(capability).searchParams.getAll('r')).toEqual([candidate.url])
    expect(capability).not.toContain(candidate.registration_token!)
    page.on('websocket', socket => {
      if (!new URL(socket.url()).pathname.endsWith('/v2/ws')) return
      sockets.push(socket.url())
      socket.on('framereceived', ({ payload: frame }) => {
        if (Buffer.isBuffer(frame)) receivedRelayBytes += frame.byteLength
        if (receivedRelayBytes >= nextProgressBytes) {
          console.info(JSON.stringify({ event: 'cloudflare-relay-progress', receivedRelayBytes }))
          nextProgressBytes += PROGRESS_BYTES
        }
      })
    })
    // Disabling peer transport proves that successful publication alone cannot
    // satisfy this scenario: all encrypted file records must cross the relay.
    await page.addInitScript(() => {
      Object.defineProperties(window, {
        RTCPeerConnection: { configurable: true, value: undefined },
        showDirectoryPicker: { configurable: true, value: undefined },
        showSaveFilePicker: { configurable: true, value: undefined },
      })
      Object.defineProperty(navigator.storage, 'getDirectory', { configurable: true, value: undefined })
    })
    await page.goto(capability)
    const action = page.getByRole('button', { name: 'Download file', exact: true })
    await expect(action).toBeEnabled()
    const downloadStarted = page.waitForEvent('download', { timeout: CLOUD_TIMEOUT_MILLISECONDS })
    const started = Date.now()
    await action.click()
    const download = await downloadStarted
    const downloadedPath = await download.path()
    expect(downloadedPath).not.toBeNull()
    const downloaded = await readFile(downloadedPath!)
    const digest = (bytes: Uint8Array) => createHash('sha256').update(bytes).digest('hex')
    expect(downloaded.byteLength).toBe(FILE_BYTES)
    expect(digest(downloaded)).toBe(digest(payload))
    expect(sockets.length).toBeGreaterThan(0)
    for (const socket of sockets) expect(new URL(socket).host).toBe(relay.host)
    expect(receivedRelayBytes).toBeGreaterThan(FILE_BYTES)
    expect(await readFile(senderTrace, 'utf8')).not.toContain(candidate.registration_token!)
    const evidence = {
      relay: candidate.url,
      shareId: new URL(sockets[0]!).searchParams.get('share_id'),
      fileBytes: FILE_BYTES,
      sha256: digest(downloaded),
      receivedRelayBytes,
      connections: sockets.length,
      downloadMilliseconds: Date.now() - started,
      peerTransport: 'disabled',
    }
    await testInfo.attach('cloudflare-relay-evidence', { body: JSON.stringify(evidence, null, 2), contentType: 'application/json' })
    console.info(JSON.stringify(evidence))
  } catch (error) {
    const failure = JSON.stringify({ sockets, receivedRelayBytes, browserErrors, ui: await page.locator('body').innerText() }, null, 2)
    await writeFile(testInfo.outputPath('failure.json'), failure)
    await testInfo.attach('cloudflare-failure', {
      body: failure,
      contentType: 'application/json',
    })
    const diagnostics = page.getByRole('button', { name: 'Export diagnostics', exact: true }).first()
    if (await diagnostics.isVisible()) {
      await diagnostics.click()
      const exported = page.waitForEvent('download', { timeout: 5_000 })
      await page.getByRole('button', { name: 'Save file', exact: true }).first().click()
      await (await exported).saveAs(testInfo.outputPath('browser-diagnostics.json'))
    }
    throw error
  } finally {
    await page.close()
    try { await sender?.stop() } finally { await vite.close() }
  }
})
