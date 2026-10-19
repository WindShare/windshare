import { spawn } from 'node:child_process'
import { once } from 'node:events'
import { randomBytes } from 'node:crypto'
import { readFile, writeFile } from 'node:fs/promises'
import { join } from 'node:path'
import { setTimeout as delay } from 'node:timers/promises'
import { digest, event, REPOSITORY, save } from './deployment.mjs'

const READY_TIMEOUT_MS = 120_000
const DOWNLOAD_TIMEOUT_MS = 600_000
const PROGRESS_INTERVAL_MS = 15_000
const MAX_CAPTURE_CHARACTERS = 256 * 1024
const STOP_TIMEOUT_MS = 5_000
const MIB = 1024 * 1024

function startSender(executable, source, frontend, deployment, output) {
  const child = spawn(executable, ['share', source, '--front-url', frontend, '--trace', join(output, 'sender.ndjson')], {
    cwd: REPOSITORY, env: { ...process.env, WINDSHARE_RELAY_CONFIG: deployment.relayConfig },
    stdio: ['ignore', 'pipe', 'pipe'], windowsHide: true,
  })
  let stderr = ''
  child.stderr.on('data', chunk => { stderr = (stderr + chunk).slice(-MAX_CAPTURE_CHARACTERS) })
  const closed = once(child, 'close')
  const ready = new Promise((resolve, reject) => {
    let stdout = ''
    const timer = setTimeout(() => reject(new Error('Sender readiness timeout: ' + stderr)), READY_TIMEOUT_MS)
    child.stdout.on('data', chunk => {
      stdout = (stdout + chunk).slice(-MAX_CAPTURE_CHARACTERS)
      const found = /^Link: (.+)$/mu.exec(stdout)
      if (found) { clearTimeout(timer); resolve(found[1]) }
    })
    child.once('error', error => { clearTimeout(timer); reject(error) })
    child.once('close', code => { clearTimeout(timer); reject(new Error('Sender exited: ' + code + ' ' + stderr)) })
  })
  return {
    ready,
    async stop() {
      if (child.exitCode === null) child.kill('SIGINT')
      if (!await Promise.race([closed.then(() => true), delay(STOP_TIMEOUT_MS).then(() => false)])) {
        child.kill('SIGKILL')
        await closed
      }
      await writeFile(join(output, 'sender-stderr.log'), stderr)
    },
  }
}

function observe(page) {
  const counts = { sent: {}, received: {} }
  const state = { opened: 0, closed: 0, shareId: undefined, errors: [], counts }
  page.on('pageerror', error => state.errors.push(error.message))
  page.on('websocket', socket => {
    if (new URL(socket.url()).pathname !== '/v2/ws') return
    state.opened++
    state.shareId = new URL(socket.url()).searchParams.get('share_id')
    socket.on('close', () => { state.closed++ })
    for (const [eventName, direction] of [['framesent', 'sent'], ['framereceived', 'received']]) {
      socket.on(eventName, ({ payload }) => {
        const bytes = Buffer.isBuffer(payload) ? payload : Buffer.from(payload)
        const magic = bytes.subarray(0, 4).toString('ascii')
        const count = counts[direction][magic] ??= { messages: 0, bytes: 0 }
        count.messages++
        count.bytes += bytes.byteLength
      })
    }
  })
  return { snapshot: () => structuredClone(state) }
}

async function receiver(browser, capability, feedbackDelayMs) {
  const context = await browser.newContext({ acceptDownloads: true })
  const page = await context.newPage()
  const observation = observe(page)
  await page.addInitScript(({ feedbackDelayMs }) => {
    Object.defineProperties(window, {
      RTCPeerConnection: { configurable: true, value: undefined },
      showDirectoryPicker: { configurable: true, value: undefined },
      showSaveFilePicker: { configurable: true, value: undefined },
    })
    // Delayed consumption feedback exercises relay backpressure without delaying
    // liveness traffic or altering the production Worker or encrypted payloads.
    if (feedbackDelayMs > 0) {
      const send = WebSocket.prototype.send
      WebSocket.prototype.send = function (data) {
        const bytes = data instanceof ArrayBuffer ? new Uint8Array(data)
          : ArrayBuffer.isView(data) ? new Uint8Array(data.buffer, data.byteOffset, data.byteLength) : undefined
        if (bytes && String.fromCharCode(...bytes.subarray(0, 4)) === 'WS2B') {
          const retained = bytes.slice()
          setTimeout(() => { if (this.readyState === WebSocket.OPEN) send.call(this, retained) }, feedbackDelayMs)
        } else send.call(this, data)
      }
    }
  }, { feedbackDelayMs })
  try {
    await page.goto(capability)
    const button = page.getByRole('button', { name: 'Download file', exact: true })
    await button.waitFor({ state: 'visible', timeout: READY_TIMEOUT_MS })
    await page.waitForFunction(() => [...document.querySelectorAll('button')].some(button =>
      button.textContent === 'Download file' && !button.disabled), undefined, { timeout: READY_TIMEOUT_MS })
    return { context, page, button, observation, feedbackDelayMs }
  } catch (error) {
    await context.close()
    throw error
  }
}

export async function runScenario(browser, built, frontend, deployment, output, scenario) {
  const payload = randomBytes(scenario.fileMiB * MIB)
  const source = join(output, 'payload.bin')
  await writeFile(source, payload)
  const sender = startSender(built.executable, source, frontend, deployment, output)
  const peers = []
  let progress
  let result
  try {
    const capability = await sender.ready
    const configured = new URL(capability).searchParams.getAll('r')
    if (configured.length !== 1 || configured[0] !== deployment.url) throw new Error('Unexpected relay candidate')
    for (const feedbackDelayMs of scenario.feedbackDelaysMs) peers.push(await receiver(browser, capability, feedbackDelayMs))
    const before = peers.map(peer => peer.observation.snapshot())
    const startedAt = new Date().toISOString()
    const started = performance.now()
    event('capacity-scenario-start', { scenario: scenario.name, workerName: deployment.workerName, startedAt, before })
    progress = setInterval(() => event('capacity-progress', {
      scenario: scenario.name, elapsedSeconds: Math.round((performance.now() - started) / 1000),
      peers: peers.map(peer => peer.observation.snapshot()),
    }), PROGRESS_INTERVAL_MS)
    const downloads = []
    if (scenario.idleMs) {
      await delay(scenario.idleMs)
    } else {
      await Promise.all(peers.map(async peer => {
        const downloadPromise = peer.page.waitForEvent('download', { timeout: DOWNLOAD_TIMEOUT_MS })
        const begin = performance.now()
        await peer.button.click()
        const download = await downloadPromise
        const path = await download.path()
        if (!path) throw new Error('Browser did not publish its download')
        const received = await readFile(path)
        if (received.byteLength !== payload.byteLength || digest(received) !== digest(payload)) {
          throw new Error('Downloaded payload failed SHA-256 verification')
        }
        downloads.push({ feedbackDelayMs: peer.feedbackDelayMs, bytes: received.byteLength,
          sha256: digest(received), elapsedMs: performance.now() - begin })
      }))
    }
    const completedAt = new Date().toISOString()
    const elapsedMs = performance.now() - started
    const after = peers.map(peer => peer.observation.snapshot())
    if (after.some((peer, index) => peer.errors.length || peer.closed !== before[index].closed ||
        peer.opened !== before[index].opened)) throw new Error('Unexpected browser failure or relay reconnection')
    result = { scenario, workerName: deployment.workerName, startedAt, completedAt, elapsedMs, before, after, downloads,
      sourceBytes: payload.byteLength, sourceSha256: digest(payload), peerTransport: 'disabled',
      aggregateMiBPerSecond: downloads.reduce((sum, item) => sum + item.bytes, 0) / MIB / (elapsedMs / 1000),
      outcome: 'passed' }
    await save(join(output, 'scenario.json'), result)
    event('capacity-scenario-complete', result)
    return result
  } catch (error) {
    await save(join(output, 'failure.json'), { error: String(error), at: new Date().toISOString(),
      peers: peers.map(peer => peer.observation.snapshot()) })
    throw error
  } finally {
    clearInterval(progress)
    for (const peer of peers) await peer.context.close()
    await sender.stop()
  }
}
