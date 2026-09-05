import { execFile, spawn } from 'node:child_process'
import { mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { createInterface } from 'node:readline'
import { promisify } from 'node:util'
import { chromium, firefox, webkit } from '@playwright/test'

const root = resolve(import.meta.dirname, '../../..')
const temporary = await mkdtemp(join(tmpdir(), 'windshare-tcp-evidence-'))
const executable = join(temporary, process.platform === 'win32' ? 'tcp-evidence.exe' : 'tcp-evidence')
const outcomes = []
const mapped = process.argv.includes('--mapped')
const outputPath = process.argv.slice(2).find(argument => !argument.startsWith('--'))
let server
try {
  await promisify(execFile)('go', ['build', '-o', executable, './transport/webrtc/provider/testdata/tcpserver'], {
    cwd: root, windowsHide: true, timeout: 60_000,
    env: { ...process.env, GOWORK: 'off', GOTOOLCHAIN: 'local' },
  })
  server = spawn(executable, [], { cwd: root, windowsHide: true, stdio: ['ignore', 'pipe', 'inherit'] })
  const origin = await new Promise((resolveOrigin, reject) => {
    const timeout = setTimeout(() => reject(new Error('TCP fixture readiness timed out')), 10_000)
    const lines = createInterface({ input: server.stdout })
    lines.once('line', (line) => { clearTimeout(timeout); lines.close(); resolveOrigin(line) })
    server.once('error', reject)
  })
  for (const [name, engine] of Object.entries({ chromium, firefox, webkit })) {
    let browser
    try {
      browser = await engine.launch({ headless: true })
      for (const family of [4, 6]) {
        const page = await browser.newPage()
        await page.goto(origin)
        const outcome = await page.evaluate(async ({ family, mapped }) => {
          const peer = new RTCPeerConnection({ iceServers: [] })
          const channel = peer.createDataChannel('tcp-capability')
          try {
            const opened = new Promise((resolveOpen, reject) => {
              channel.onopen = resolveOpen
              channel.onerror = () => reject(new Error('datachannel error'))
            })
            const gathered = new Promise((resolveGather) => {
              peer.onicecandidate = (event) => { if (event.candidate === null) resolveGather() }
            })
            await peer.setLocalDescription(await peer.createOffer())
            await Promise.race([gathered, new Promise((_, reject) => setTimeout(() => reject(new Error('gathering timeout')), 5_000))])
            const response = await fetch('/offer?family=' + family + '&mapped=' + (mapped ? '1' : '0'), {
              method: 'POST', body: JSON.stringify(peer.localDescription),
            })
            if (!response.ok) throw new Error(await response.text())
            const fixture = await response.json()
            await peer.setRemoteDescription(fixture.answer)
            await Promise.race([opened, new Promise((_, reject) => setTimeout(() => reject(new Error('TCP open timeout')), 8_000))])
            const echoed = new Promise((resolveEcho) => { channel.onmessage = (event) => resolveEcho(event.data) })
            channel.send('authenticated-payload')
            const payload = await Promise.race([echoed, new Promise((_, reject) => setTimeout(() => reject(new Error('payload timeout')), 2_000))])
            const pair = await (await fetch('/result?id=' + encodeURIComponent(fixture.id))).json()
            if (payload !== 'tcp-proof:authenticated-payload' || pair.protocol !== 'tcp') throw new Error('TCP payload/pair proof mismatch')
            if (mapped && pair.localPort === Number(fixture.id.slice(fixture.id.lastIndexOf(':') + 1))) throw new Error('mapping did not allocate a distinct external port')
            return { status: 'supported', payload, baseEndpoint: fixture.id, pair }
          } catch (error) {
            return { status: 'unsupported', reason: String(error) }
          } finally {
            peer.close()
          }
        }, { family, mapped })
        outcomes.push({ browser: name, version: browser.version(), family, mapped, ...outcome })
        await page.close()
      }
    } catch (error) {
      outcomes.push({ browser: name, status: 'unavailable', reason: String(error) })
    } finally {
      await browser?.close()
    }
  }
  const evidence = { kind: 'local-tcp-capability', platform: process.platform, pion: { webrtc: 'v4.2.16', ice: 'v4.2.7' }, outcomes }
  console.log(JSON.stringify(evidence, null, 2))
  if (outputPath) await writeFile(resolve(outputPath), JSON.stringify(evidence, null, 2) + '\n')
} finally {
  if (server && server.exitCode === null) {
    const completion = new Promise((resolveExit) => server.once('exit', resolveExit))
    server.kill()
    await completion
  }
  await rm(temporary, { recursive: true, force: true })
}
