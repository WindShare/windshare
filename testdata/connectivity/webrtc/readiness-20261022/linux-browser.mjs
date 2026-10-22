import { spawn } from 'node:child_process'
import { readFile } from 'node:fs/promises'
import { createInterface } from 'node:readline'
import { startLinuxBrowser as startOriginal, lab, sshArgs } from '../slowdown-20261022/linux-browser.mjs'

export async function startLinuxBrowser(ports, evidence) {
  const original = await startOriginal(ports, evidence)
  const source = await readFile(new URL('./udp-samples.py', import.meta.url), 'utf8')
  const quote = value => "'" + value.replaceAll("'", "'\\''") + "'"
  const sampler = spawn('ssh', [...sshArgs, lab.sshTarget,
    'python3 -u -c ' + quote(source)], { windowsHide: true, stdio: ['pipe', 'pipe', 'pipe'] })
  const started = Date.now()
  const events = evidence.udpTimeline = []
  const lines = createInterface({ input: sampler.stdout })
  let samplingError
  lines.on('line', line => {
    try {
      const value = JSON.parse(line)
      if (value.event === 'ready') evidence.udpClock = { sent: started, received: Date.now(), remote: value.time }
      events.push(value)
    } catch (error) { samplingError = String(error) }
  })
  sampler.stderr.on('data', data => { evidence.udpSamplerStderr = (evidence.udpSamplerStderr ?? '') + data })
  const stopped = new Promise(resolve => { sampler.on('close', resolve); sampler.on('error', error => { samplingError = String(error); resolve() }) })
  return { ...original, close: async () => {
    sampler.stdin.end()
    const timeout = setTimeout(() => sampler.kill(), 5000)
    try { await stopped } finally { clearTimeout(timeout); await original.close() }
    if (samplingError || !events.some(value => value.event === 'complete')) {
      throw new Error('UDP sampling did not settle: ' + (samplingError ?? evidence.udpSamplerStderr ?? 'missing completion'))
    }
  } }
}
