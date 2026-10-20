import { execFile, spawn } from 'node:child_process'
import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'
import { chromium } from '@playwright/test'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')
const temporary = await mkdtemp(join(tmpdir(), 'windshare-turn-trial-'))
let child
let browser
let origin
try {
  if (!process.env.WINDSHARE_TURN_TRIAL_KEY) throw new Error('Set WINDSHARE_TURN_TRIAL_KEY to the temporary Cloudflare key file')
  const executable = join(temporary, process.platform === 'win32' ? 'turnprobe.exe' : 'turnprobe')
  await promisify(execFile)('go', ['build', '-o', executable, './transport/webrtc/testdata/browser/turnprobe'], {
    cwd: root, windowsHide: true, timeout: 60_000,
  })
  child = spawn(executable, [], { cwd: root, windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'] })
  child.stderr.pipe(process.stderr)
  origin = await new Promise((resolveReady, reject) => {
    const timer = setTimeout(() => reject(new Error('TURN probe readiness timeout')), 15_000)
    let output = ''
    child.stdout.on('data', (chunk) => {
      output += chunk
      const line = output.split('\n').find((value) => value.startsWith('{'))
      if (!line) return
      clearTimeout(timer)
      resolveReady(JSON.parse(line).origin)
    })
    child.once('error', reject)
    child.once('exit', (code) => { clearTimeout(timer); reject(new Error(`TURN probe exited ${code}`)) })
  })
  browser = await chromium.launch({ headless: true })
  const page = await browser.newPage()
  await page.goto(origin)
  const evidence = await page.evaluate(async () => {
    const timeout = (promise) => {
      let timer
      return Promise.race([promise, new Promise((_, reject) => {
        timer = setTimeout(() => reject(new Error('TURN trial timeout')), 60_000)
      })]).finally(() => clearTimeout(timer))
    }
    const evidence = []
    const active = new Map()
    const run = async (transport, replaces = 0) => {
      const response = await fetch(`/start?transport=${transport}&replaces=${replaces}`, { method: 'POST' })
      if (!response.ok) throw new Error(await response.text())
      const start = await response.json()
      const pc = new RTCPeerConnection({ iceServers: start.iceServers, iceTransportPolicy: 'relay' })
      active.set(start.id, pc)
      const delivered = new Promise((resolveDelivery, rejectDelivery) => {
        pc.ondatachannel = ({ channel }) => {
          channel.binaryType = 'arraybuffer'
          let received = 0
          let started
          channel.onopen = () => { started = performance.now(); channel.send('start') }
          channel.onerror = () => rejectDelivery(new Error('TURN data channel failed'))
          channel.onmessage = ({ data }) => {
            const bytes = new Uint8Array(data)
            if (bytes.some((value, index) => value !== index % 251)) {
              rejectDelivery(new Error('TURN payload corrupted'))
              return
            }
            received += bytes.length
            if (received === start.bytes) resolveDelivery({ received, elapsedMs: performance.now() - started })
          }
        }
      })
      await pc.setRemoteDescription(start.offer)
      await pc.setLocalDescription(await pc.createAnswer())
      if (pc.iceGatheringState !== 'complete') {
        await timeout(new Promise((resolveGather) => {
          pc.onicegatheringstatechange = () => { if (pc.iceGatheringState === 'complete') resolveGather() }
        }))
      }
      const answer = await fetch(`/answer?id=${start.id}`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(pc.localDescription),
      })
      if (!answer.ok) throw new Error(await answer.text())
      const delivery = await timeout(delivered)
      const native = await (await fetch(`/stats?id=${start.id}`)).json()
      const stats = await pc.getStats()
      const selected = [...stats.values()].find((value) => value.type === 'transport' && value.selectedCandidatePairId)
      const pair = stats.get(selected?.selectedCandidatePairId)
      const local = stats.get(pair?.localCandidateId)
      const remote = stats.get(pair?.remoteCandidateId)
      if (!native.selected || native.pair.LocalType !== 'relay' || local?.candidateType !== 'relay') {
        throw new Error('TURN trial did not select relay candidates on both endpoints')
      }
      evidence.push({ transport, replacement: replaces !== 0, ...delivery, usefulMbps: delivery.received * 8 / delivery.elapsedMs / 1000,
        native, browser: { localType: local.candidateType, remoteType: remote?.candidateType,
          relayProtocol: local.relayProtocol, bytesReceived: pair?.bytesReceived, bytesSent: pair?.bytesSent } })
      return start.id
    }
    const close = async (id) => {
      active.get(id).close()
      active.delete(id)
      const resources = await (await fetch(`/close?id=${id}`, { method: 'POST' })).json()
      if (resources.ActiveSockets !== 0) throw new Error('TURN probe retained sockets')
    }
    try {
      for (const transport of ['udp', 'tls']) {
        const first = await run(transport)
        // This overlaps two credential generations. Policy tests separately own
        // expiry, signaling loss, and publication of the replacement lane.
        const replacement = await run(transport, first)
        await close(first)
        await close(replacement)
      }
      return evidence
    } finally {
      for (const id of [...active.keys()]) await close(id).catch(() => undefined)
    }
  })
  console.log(JSON.stringify({ scenario: 'cloudflare-native-browser-turn', evidence }, null, 2))
} finally {
  await browser?.close()
  if (origin) await fetch(`${origin}/stop`, { method: 'POST' }).catch(() => undefined)
  if (child && child.exitCode === null) {
    await Promise.race([
      new Promise((resolveExit) => child.once('exit', resolveExit)),
      new Promise((resolveTimeout) => setTimeout(resolveTimeout, 5000)),
    ])
    if (child.exitCode === null) child.kill()
  }
  await rm(temporary, { recursive: true, force: true })
}
