import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { createHash } from 'node:crypto'
import { spawn } from 'node:child_process'
import { once } from 'node:events'
import { fileURLToPath } from 'node:url'
import { experimentDirectory } from '../experiment-paths.mjs'

const root = fileURLToPath(new URL('../../../../', import.meta.url))
const buildName = process.env.WS_ADAPTIVE_BUILD ?? 'webrtc-adaptive-20261023-verified'
const build = experimentDirectory(root, buildName)
const historical = path.join(root, '.tmp/webrtc-load-recheck-20261023')
const work = experimentDirectory(root, 'webrtc-adaptive-controls-' + Date.now())
fs.mkdirSync(work)
const fixture = path.join(root, '.tmp/webrtc-wan-20261021/payload-long.bin')
const sha = file => createHash('sha256').update(fs.readFileSync(file)).digest('hex')
assert.equal(sha(path.join(historical, 'probe.exe')), '9e063fe2c5a8e6591865f87fee062891893d2160f26ae8106ed770b827f524ce')
assert.equal(sha(path.join(historical, 'decoder.js')), '300b2280bf4e0201b713b15186dc2fed893ccaa58cdc457fe5f6333d78b9ad41')
assert.equal(sha(fixture), 'b972fd5178f6ec01c4809b4562c8fc1bab581e50aba7b86c3d334fccd66202af')
for (const file of ['probe.exe', 'decoder.js', 'run.cjs', 'remote.py']) fs.copyFileSync(path.join(historical, file), path.join(work, file))
const lab = JSON.parse(fs.readFileSync(path.join(root, '.tmp/webrtc-preflight-20261023/lab.json')))
const env = { ...process.env, ...lab, WS_ADAPTIVE_BUILD: buildName, WS_RESEARCH_OUTPUT: 'opfs', WS_RESEARCH_TRACE: 'off', WS_RESEARCH_ISOLATE_BROWSER_DNS: '1', WS_RESEARCH_FIXTURE: fixture, WS_ADAPTIVE_ARM: 'adaptive', WS_ADAPTIVE_FAULT: 'none', WS_PREFLIGHT_WORKFLOW: 'file', WS_PREFLIGHT_RECEIVERS: '1' }
const ledger = { scope: 'Archived paced four-PC cached encrypted input to authenticated memory, interleaved with current demand-driven complete product output to OPFS. Same source bytes. Different connection and output policies; this is a historical control, not an isolated estimate of pipeline overhead.', fixture: { bytes: fs.statSync(fixture).size, sha256: sha(fixture) }, harness: Object.fromEntries(['probe.exe', 'decoder.js', 'run.cjs', 'remote.py'].map(file => [file, sha(path.join(work, file))])), runs: [] }
const save = () => fs.writeFileSync(path.join(work, 'ledger.json'), JSON.stringify(ledger, null, 2))
for (let repetition = 1; repetition <= 4; repetition++) {
  for (const arm of repetition % 2 ? ['cached', 'product'] : ['product', 'cached']) {
    const label = 'control-' + repetition + '-' + arm
    const row = { arm, repetition, label, startedMs: Date.now() }
    ledger.runs.push(row); save()
    let args
    if (arm === 'cached') {
      fs.writeFileSync(path.join(work, label + '.config.json'), JSON.stringify([{ name: 'four-mux-long-pace16', file: fixture, sealed: true, nativeMux: true, count: 4, dynamic: true, pace: 16, repetition }]))
      args = [path.join(work, 'run.cjs'), label]
    } else args = ['--import', './testdata/connectivity/webrtc/slowdown-20261022/loader.mjs', './testdata/connectivity/webrtc/adaptive-20261023/probe.mjs', 'baseline', String(ledger.fixture.bytes / 1048576), '1', '0', 'none', '1', 'lan-isolated']
    const log = fs.openSync(path.join(work, label + '.log'), 'w')
    try {
      const child = spawn(process.execPath, args, { cwd: root, windowsHide: true, env: { ...env, WS_RESEARCH_LABEL: path.basename(work) + '-' + label }, stdio: ['ignore', log, log] })
      const [code] = await once(child, 'close'); row.exitCode = code
      assert.equal(code, 0)
      if (arm === 'cached') {
        row.report = path.relative(root, path.join(work, label + '.json'))
        const data = JSON.parse(fs.readFileSync(path.join(root, row.report)))
        assert(!data.error && data.remoteCleanup?.status === 'complete' && !data.trials[0].error)
        row.MBps = data.trials[0].result.content.verifiedMBps
      } else {
        for (const name of fs.readdirSync(path.join(build, 'runs'))) {
          const file = path.join(build, 'runs', name, 'report.json')
          if (!fs.existsSync(file)) continue
          const data = JSON.parse(fs.readFileSync(file))
          if (data.label !== path.basename(work) + '-' + label) continue
          assert.equal(data.status, 'passed'); row.report = path.relative(root, file)
          row.MBps = data.rounds[0].MBps; row.firstOutputMs = data.rounds[0].activationToFirstWriteMs; row.completionMs = data.rounds[0].activationToCompleteMs; row.peers = data.rounds[0].peers.length
        }
        assert(row.report)
      }
    } catch (error) { row.error = String(error) }
    finally { fs.closeSync(log); row.finishedMs = Date.now(); save(); console.log(JSON.stringify(row)) }
  }
}
ledger.status = ledger.runs.some(row => row.error) ? 'failed' : 'passed'; save()
console.log(JSON.stringify({ ledger: path.join(work, 'ledger.json'), status: ledger.status }))
