import { execFile } from 'node:child_process'
import { createHash, randomBytes } from 'node:crypto'
import { once } from 'node:events'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'
import { expect, test, type Download } from '@playwright/test'
import { createServer } from 'vite'

import { DirectProcess } from './fixtures/direct-process'
import { PreviewControl } from './cloudflare/preview-control'
import { RecoveryEvidence, senderRecoveryEvidence, senderTraceSequence } from './cloudflare/recovery-evidence'

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../..')
const BLOCK_BYTES = 1024 * 1024
const FILE_BYTES = 17 * BLOCK_BYTES
const BUILD_TIMEOUT_MILLISECONDS = 120_000
const CLOUD_TIMEOUT_MILLISECONDS = 120_000
const PROGRESS_BYTES = 4 * BLOCK_BYTES
const EVIDENCE_POLL_MILLISECONDS = 250

test('the original Cloudflare download resumes after one Preview reset', async ({ page, request }, testInfo) => {
  const previewSecret = process.env.WINDSHARE_RELAY_PREVIEW_SECRET
  if (!previewSecret) throw new Error('Set WINDSHARE_RELAY_PREVIEW_SECRET for the isolated tests/entry.mjs Preview')
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
  const sockets: { url: string; closed: boolean }[] = []
  const downloads: Download[] = []
  const recovery = new RecoveryEvidence()
  let resetEvidence: Awaited<ReturnType<PreviewControl['resetOnce']>> | undefined
  let documentLoads = 0
  page.on('download', download => downloads.push(download))
  // Capability sanitization uses same-document history changes; only a new
  // document would restart the receiver and invalidate this recovery evidence.
  page.on('domcontentloaded', () => { documentLoads += 1 })
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
      '--block-size', String(BLOCK_BYTES),
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
      const connection = { url: socket.url(), closed: false }
      sockets.push(connection)
      socket.on('close', () => { connection.closed = true })
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
    const shareId = new URL(sockets[0]!.url).searchParams.get('share_id')!
    const preview = new PreviewControl(request, candidate.url, previewSecret, shareId)
    const before = await preview.status()
    expect(before.sockets.length).toBeGreaterThanOrEqual(2)
    const started = Date.now()
    await action.click()
    await expect.poll(async () => {
      await recovery.capture(page)
      return recovery.writtenBytes()
    }, { timeout: CLOUD_TIMEOUT_MILLISECONDS, intervals: [EVIDENCE_POLL_MILLISECONDS] }).toBeGreaterThanOrEqual(PROGRESS_BYTES)
    const beforeReset = await preview.status()
    await recovery.capture(page)
    const cut = recovery.cut(BLOCK_BYTES, FILE_BYTES)
    expect(downloads).toHaveLength(0)
    const senderCut = senderTraceSequence(await readFile(senderTrace, 'utf8'))
    const interruptedSocket = sockets.at(-1)!
    resetEvidence = await preview.resetOnce(beforeReset)
    console.info(JSON.stringify({ event: 'cloudflare-reset', shareId, ...resetEvidence, ...cut }))
    await expect.poll(async () => {
      await recovery.capture(page)
      return downloads.length
    }, { timeout: CLOUD_TIMEOUT_MILLISECONDS, intervals: [EVIDENCE_POLL_MILLISECONDS] }).toBe(1)
    const download = downloads[0]!
    const downloadedPath = await download.path()
    expect(downloadedPath).not.toBeNull()
    const downloaded = await readFile(downloadedPath!)
    const digest = (bytes: Uint8Array) => createHash('sha256').update(bytes).digest('hex')
    expect(downloaded.byteLength).toBe(FILE_BYTES)
    expect(digest(downloaded)).toBe(digest(payload))
    expect(sockets.length).toBeGreaterThan(1)
    expect(interruptedSocket.closed).toBe(true)
    expect(documentLoads, 'Automatic recovery must not refresh the page').toBe(1)
    for (const socket of sockets) expect(new URL(socket.url).host).toBe(relay.host)
    expect(receivedRelayBytes).toBeGreaterThan(FILE_BYTES)
    await recovery.capture(page)
    const browserRecovery = recovery.verify(cut, FILE_BYTES)
    const senderArtifact = await readFile(senderTrace, 'utf8')
    expect(senderArtifact).not.toContain(candidate.registration_token!)
    const senderRecovery = senderRecoveryEvidence(senderArtifact, senderCut)
    const evidence = {
      relay: candidate.url,
      shareId,
      resetRequests: 1,
      reset: resetEvidence,
      ...browserRecovery,
      senderRecovery,
      fileBytes: FILE_BYTES,
      sha256: digest(downloaded),
      receivedRelayBytes,
      connections: sockets.length,
      downloadMilliseconds: Date.now() - started,
      peerTransport: 'disabled',
    }
    await writeFile(testInfo.outputPath('evidence.json'), JSON.stringify(evidence, null, 2))
    await testInfo.attach('cloudflare-relay-evidence', { body: JSON.stringify(evidence, null, 2), contentType: 'application/json' })
    console.info(JSON.stringify(evidence))
  } catch (error) {
    const failure = JSON.stringify({ sockets, receivedRelayBytes, resetEvidence, documentLoads, browserErrors,
      ui: await page.locator('body').innerText().catch(() => 'page unavailable') }, null, 2)
    await writeFile(testInfo.outputPath('failure.json'), failure)
    await testInfo.attach('cloudflare-failure', {
      body: failure,
      contentType: 'application/json',
    })
    throw error
  } finally {
    try {
      await recovery.capture(page).catch(() => undefined)
      await writeFile(testInfo.outputPath('browser-diagnostics.ndjson'), recovery.bundle())
      await writeFile(testInfo.outputPath('browser-events.ndjson'), recovery.collected())
    } finally {
      await page.close()
      try { await sender?.stop() } finally { await vite.close() }
    }
  }
})
