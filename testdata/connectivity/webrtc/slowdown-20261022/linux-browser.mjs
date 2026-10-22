import { spawn } from 'node:child_process'
import { once } from 'node:events'
import { readFile } from 'node:fs/promises'
import { createInterface } from 'node:readline'
import net from 'node:net'
import { createHash } from 'node:crypto'
import playwright from '@playwright/test'

// Lab endpoints stay outside the repository; the defaults are documentation addresses.
export const lab = {
  sshTarget: process.env.WS_LAB_SSH_TARGET ?? 'lab@192.0.2.105',
  sshPort: process.env.WS_LAB_SSH_PORT ?? '22',
  sshKey: process.env.WS_LAB_SSH_KEY,
  receiverHost: process.env.WS_LAB_RECEIVER_HOST ?? '192.0.2.105',
  senderHost: process.env.WS_LAB_SENDER_HOST ?? '192.0.2.234',
}
export const sshArgs = ['-p', lab.sshPort, ...(lab.sshKey ? ['-i', lab.sshKey] : []), '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=8']
const pause = ms => new Promise(resolve => setTimeout(resolve, ms))
const quote = value => "'" + value.replaceAll("'", "'\\''") + "'"
const launch = extra => spawn('ssh', [...sshArgs, ...extra], { windowsHide: true, stdio: ['pipe', 'pipe', 'pipe'] })
async function until(predicate, milliseconds = 15000) {
  const end = Date.now() + milliseconds
  while (!await predicate()) {
    if (Date.now() > end) throw new Error('Linux browser startup timed out')
    await pause(50)
  }
}
async function stop(child) {
  if (!child || child.exitCode !== null) return
  child.stdin.end()
  await Promise.race([once(child, 'close'), pause(6000)])
  if (child.exitCode === null) { child.kill(); await once(child, 'close') }
}
async function freePort() {
  const server = net.createServer()
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  const port = server.address().port
  await new Promise(resolve => server.close(resolve))
  return port
}
export async function startLinuxBrowser(ports, evidence) {
  let ready, cleanup, browser, tunnel
  const source = await readFile(new URL('./lan-browser.py', import.meta.url), 'utf8')
  const remote = launch([lab.sshTarget, `WS_LAB_RECEIVER_HOST=${quote(lab.receiverHost)} WS_LAB_SENDER_HOST=${quote(lab.senderHost)} python3 -u -c ` + quote(source) + (process.env.WS_RESEARCH_LARGE_BUFFER === '1' ? ' --large-recvbuf' : '') + (process.env.WS_RESEARCH_ISOLATE_BROWSER_DNS === '1' ? ' --isolate-external-dns' : '') + (process.env.WS_RESEARCH_NETLOG === '1' ? ' --capture-netlog' : '')])
  remote.stderr.on('data', data => { evidence.remoteStderr = (evidence.remoteStderr ?? '') + data })
  createInterface({ input: remote.stdout }).on('line', line => {
    const item = JSON.parse(line)
    if (item.event === 'ready') ready = item
    if (item.event === 'cleanup') cleanup = item
    if (item.event === 'netlog') evidence.netlog = item
  })
  async function close() {
    await browser?.close()
    if (tunnel?.exitCode === null) { tunnel.kill(); await once(tunnel, 'close') }
    if (remote.exitCode === null) remote.stdin.write('{"command":"stop"}\n')
    await stop(remote)
    evidence.remoteCleanup = cleanup
    if (ready && cleanup?.status !== 'complete') throw new Error('Linux Chrome cleanup acknowledgement missing')
  }
  try {
    await until(() => ready || remote.exitCode !== null)
    if (!ready) throw new Error(evidence.remoteStderr ?? 'Remote Chrome failed')
    evidence.remote = ready
    const port = await freePort()
    const forwards = [...new Set(ports)].flatMap(port => ['-R', '127.0.0.1:' + port + ':127.0.0.1:' + port])
    tunnel = launch(['-o', 'ExitOnForwardFailure=yes', '-N', '-L', '127.0.0.1:' + port + ':127.0.0.1:' + ready.port, ...forwards, lab.sshTarget])
    tunnel.stderr.on('data', data => { evidence.tunnelStderr = (evidence.tunnelStderr ?? '') + data })
    await until(async () => {
      if (tunnel.exitCode !== null) throw new Error(evidence.tunnelStderr ?? 'SSH forwarding failed')
      try { return (await fetch('http://127.0.0.1:' + port + '/json/version')).ok } catch { return false }
    })
    browser = await playwright.chromium.connectOverCDP('http://127.0.0.1:' + port)
    return {
      browser, context: browser.contexts()[0], close,
      tcpControl: async label => {
        const bytes = Buffer.alloc(32 * 1024 * 1024, 73)
        const socket = net.createConnection({ host: lab.receiverHost, port: ready.tcpPort })
        socket.setTimeout(30000, () => socket.destroy(new Error('LAN TCP control timeout')))
        const chunks = []
        socket.on('data', chunk => chunks.push(chunk))
        const ended = once(socket, 'end')
        await once(socket, 'connect')
        socket.end(bytes)
        await ended
        const result = JSON.parse(Buffer.concat(chunks))
        if (result.bytes !== bytes.length || result.sha256 !== createHash('sha256').update(bytes).digest('hex')) throw new Error('TCP control integrity failed')
        ;(evidence.tcpControls ??= []).push({ label, ...result })
      },
    }
  } catch (error) { await close(); throw error }
}
