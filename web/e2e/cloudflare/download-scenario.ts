import { execFile } from 'node:child_process'
import { createHash, randomBytes } from 'node:crypto'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'
import { expect, type APIRequestContext, type Download, type Page, type TestInfo } from '@playwright/test'

import { DirectProcess } from '../fixtures/direct-process'
import { startProductFrontend } from '../fixtures/product-frontend'
import type { RelayTarget } from './isolated-deployment'
import { RelayTestControl, type RelayStatus } from './relay-control'
import { EVIDENCE_POLL_MILLISECONDS, RecoveryEvidence, senderRecoveryEvidence, senderTraceSequence } from './recovery-evidence'

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')
const BLOCK_BYTES = 1024 * 1024
const RESET_FILE_BYTES = 17 * BLOCK_BYTES
// The edge can update minutes before the object. Keep real content outstanding
// through propagation; an already finished download must never pass this scenario.
const REDEPLOYMENT_FILE_BYTES = 97 * BLOCK_BYTES
const COMPLETION_TIMEOUT_MILLISECONDS = 600_000
const BUILD_TIMEOUT_MILLISECONDS = 120_000
const CLOUD_TIMEOUT_MILLISECONDS = 120_000
const PROGRESS_BYTES = 4 * BLOCK_BYTES

export interface DownloadInterruption {
  readonly kind: 'object-reset' | 'redeploy'
  readonly run: (control: RelayTestControl, before: RelayStatus) => Promise<unknown>
}

function observeBrowser(page: Page) {
  const sockets: { url: string; closed: boolean; receivedBytes: number }[] = []
  const downloads: Download[] = []
  const browserErrors: string[] = []
  const observation = { sockets, downloads, browserErrors, documentLoads: 0, receivedRelayBytes: 0 }
  let nextProgressBytes = PROGRESS_BYTES
  page.on('download', download => downloads.push(download))
  // Capability sanitization changes history without loading a new document.
  page.on('domcontentloaded', () => { observation.documentLoads += 1 })
  page.on('pageerror', error => browserErrors.push(error.message))
  page.on('websocket', socket => {
    if (!new URL(socket.url()).pathname.endsWith('/v2/ws')) return
    const connection = { url: socket.url(), closed: false, receivedBytes: 0 }
    sockets.push(connection)
    socket.on('close', () => { connection.closed = true })
    socket.on('framereceived', ({ payload: frame }) => {
      if (Buffer.isBuffer(frame)) {
        connection.receivedBytes += frame.byteLength
        observation.receivedRelayBytes += frame.byteLength
      }
      if (observation.receivedRelayBytes >= nextProgressBytes) {
        console.info(JSON.stringify({ event: 'cloudflare-relay-progress', receivedRelayBytes: observation.receivedRelayBytes }))
        nextProgressBytes += PROGRESS_BYTES
      }
    })
  })
  return observation
}

export async function runDownloadScenario(
  page: Page, request: APIRequestContext, testInfo: TestInfo, target: RelayTarget, interruption: DownloadInterruption,
): Promise<void> {
  const fileBytes = interruption.kind === 'redeploy' ? REDEPLOYMENT_FILE_BYTES : RESET_FILE_BYTES
  const output = testInfo.outputPath('run')
  await mkdir(output, { recursive: true })
  const executable = join(output, process.platform === 'win32' ? 'wind.exe' : 'wind')
  await promisify(execFile)('go', ['build', '-o', executable, './cmd/wind'], {
    cwd: REPOSITORY_ROOT, env: { ...process.env, GOWORK: 'off', GOTOOLCHAIN: 'local' },
    timeout: BUILD_TIMEOUT_MILLISECONDS, windowsHide: true,
  })
  const payload = randomBytes(fileBytes)
  const source = join(output, 'cloudflare.bin')
  await writeFile(source, payload)
  const { vite, url } = await startProductFrontend(output)
  let sender: DirectProcess | undefined
  const observation = observeBrowser(page)
  const { sockets, downloads } = observation
  const recovery = new RecoveryEvidence()
  let interruptionEvidence: unknown
  let control: RelayTestControl | undefined
  try {
    const senderTrace = join(output, 'sender.ndjson')
    sender = new DirectProcess(executable, [
      'share', source, '--front-url', url, '--trace', senderTrace, '--browser-trace', '--block-size', String(BLOCK_BYTES),
    ], {
      cwd: REPOSITORY_ROOT,
      environment: { ...process.env, WINDSHARE_RELAY_CONFIG: target.configPath },
      operationId: 'cloudflare-' + interruption.kind,
      disclosure: { stdout: 'capability', stderr: 'private' },
    })
    const ready = await sender.waitFor('stdout', /^Link: (.+)$/mu, { timeoutMilliseconds: CLOUD_TIMEOUT_MILLISECONDS })
    const capability = ready[1]!
    sender.consumeReadiness()
    expect(new URL(capability).searchParams.getAll('r')).toEqual([target.url])
    expect(capability).not.toContain(target.registrationToken)
    // Every encrypted file record must cross this relay, independent of local P2P reachability.
    await page.addInitScript(() => {
      Object.defineProperties(window, {
        RTCPeerConnection: { configurable: true, value: undefined },
        showDirectoryPicker: { configurable: true, value: undefined },
        showSaveFilePicker: { configurable: true, value: undefined },
      })
    })
    await page.goto(capability)
    const action = page.getByRole('button', { name: 'Download file', exact: true })
    await expect(action).toBeEnabled()
    const shareId = new URL(sockets[0]!.url).searchParams.get('share_id')!
    control = new RelayTestControl(request, target.url, target.testSecret, target.registrationToken, shareId)
    const started = Date.now()
    await action.click()
    await expect.poll(async () => {
      await recovery.capture(page)
      return recovery.writtenBytes()
    }, { timeout: CLOUD_TIMEOUT_MILLISECONDS, intervals: [EVIDENCE_POLL_MILLISECONDS] }).toBeGreaterThanOrEqual(PROGRESS_BYTES)
    const before = await control.status()
    expect(before.sockets.length).toBeGreaterThanOrEqual(2)
    await recovery.capture(page)
    const cut = recovery.cut(BLOCK_BYTES, fileBytes)
    expect(downloads).toHaveLength(0)
    const senderCut = senderTraceSequence(await readFile(senderTrace, 'utf8'))
    const originalSocket = sockets.at(-1)!
    interruptionEvidence = await recovery.during(page, () => interruption.run(control!, before))
    // A deploy command succeeding after the file finished is not an in-flight update.
    expect(downloads, 'Interruption must finish while the original download is active').toHaveLength(0)
    expect(recovery.writtenBytes()).toBeLessThan(fileBytes)
    const writtenBytesAfterInterruption = recovery.writtenBytes()
    console.info(JSON.stringify({ event: 'cloudflare-interruption', kind: interruption.kind, shareId, cut, interruptionEvidence }))
    await expect.poll(async () => {
      await recovery.capture(page)
      return downloads.length
    }, { timeout: COMPLETION_TIMEOUT_MILLISECONDS, intervals: [EVIDENCE_POLL_MILLISECONDS] }).toBe(1)
    const downloadedPath = await downloads[0]!.path()
    expect(downloadedPath).not.toBeNull()
    const downloaded = await readFile(downloadedPath!)
    const digest = (bytes: Uint8Array) => createHash('sha256').update(bytes).digest('hex')
    expect(downloaded.byteLength).toBe(fileBytes)
    expect(digest(downloaded)).toBe(digest(payload))
    expect(observation.documentLoads, 'Automatic recovery must not refresh the page').toBe(1)
    for (const socket of sockets) expect(new URL(socket.url).origin).toBe(new URL(target.url.replace('https:', 'wss:')).origin)
    expect(observation.receivedRelayBytes).toBeGreaterThan(fileBytes)
    await recovery.capture(page)
    const browserRecovery = recovery.verify(cut, fileBytes)
    if (originalSocket.closed) {
      expect(sockets.length).toBeGreaterThan(1)
      expect(browserRecovery.browserRecovery, 'A lost connection must recover automatically').not.toHaveLength(0)
    }
    const senderArtifact = await readFile(senderTrace, 'utf8')
    expect(senderArtifact).not.toContain(target.registrationToken)
    const senderRecovery = senderRecoveryEvidence(senderArtifact, senderCut)
    if (interruption.kind === 'object-reset') {
      expect(originalSocket.closed).toBe(true)
      expect(senderRecovery.outcome).toBe('resumed')
    }
    const evidence = {
      relay: target.url, shareId, interruption: { kind: interruption.kind, details: interruptionEvidence },
      ...browserRecovery, senderRecovery, originalSocketClosed: originalSocket.closed, writtenBytesAfterInterruption,
      fileBytes, sha256: digest(downloaded), receivedRelayBytes: observation.receivedRelayBytes,
      connections: sockets.length, downloadMilliseconds: Date.now() - started, peerTransport: 'disabled',
    }
    await writeFile(testInfo.outputPath('evidence.json'), JSON.stringify(evidence, null, 2))
    await testInfo.attach('cloudflare-relay-evidence', { body: JSON.stringify(evidence, null, 2), contentType: 'application/json' })
    console.info(JSON.stringify(evidence))
  } catch (error) {
    const failure = JSON.stringify({
      ...observation, downloads: downloads.length, interruptionEvidence, relayStatus: control?.lastStatus,
      ui: await page.locator('body').innerText().catch(() => 'page unavailable'),
    }, null, 2)
    await writeFile(testInfo.outputPath('failure.json'), failure)
    await testInfo.attach('cloudflare-failure', { body: failure, contentType: 'application/json' })
    throw error
  } finally {
    try {
      await recovery.capture(page).catch(() => undefined)
      await writeFile(testInfo.outputPath('browser-diagnostics.ndjson'), recovery.bundle())
      await writeFile(testInfo.outputPath('browser-events.ndjson'), recovery.collected())
    } finally {
      try { await page.close() } finally {
        try { await sender?.stop() } finally { await vite.close() }
      }
    }
  }
}
