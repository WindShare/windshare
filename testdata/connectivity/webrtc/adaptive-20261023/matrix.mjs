import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import os from 'node:os'
import { createHash } from 'node:crypto'
import { spawn } from 'node:child_process'
import { once } from 'node:events'
import { fileURLToPath, pathToFileURL } from 'node:url'

const root = fileURLToPath(new URL('../../../../', import.meta.url))
const build = process.env.WS_ADAPTIVE_BUILD ?? 'webrtc-adaptive-20261023-final'
const directory = path.join(root, '.tmp', build)
const suite = process.argv[2] ?? 'large'
const ledgerPath = path.join(directory, suite + '-ledger.json')
assert(!fs.existsSync(ledgerPath), 'Use a new suite name instead of overwriting evidence')
const sha = file => createHash('sha256').update(fs.readFileSync(file)).digest('hex')
const lab = JSON.parse(fs.readFileSync(path.join(root, '.tmp/webrtc-preflight-20261023/lab.json')))
const binaries = JSON.parse(fs.readFileSync(path.join(directory, 'binaries.json')))
for (const name of ['wind.exe', 'relay-native.exe']) assert.equal(sha(path.join(directory, 'binaries', name)), binaries[name])
const env = { ...process.env, ...lab, WS_ADAPTIVE_BUILD: build, WS_RESEARCH_OUTPUT: 'opfs', WS_RESEARCH_TRACE: 'off', WS_RESEARCH_NETLOG: '0', WS_RESEARCH_ISOLATE_BROWSER_DNS: '1', WS_RESEARCH_LARGE_BUFFER: '0' }
const ledger = { startedMs: Date.now(), suite, sourcesSHA256: sha(path.join(directory, 'sources.json')), binaries, machine: { cpu: os.cpus()[0].model, cores: os.cpus().length, memory: os.totalmem() }, runs: [] }
const save = () => fs.writeFileSync(ledgerPath, JSON.stringify(ledger, null, 2))
let previous = os.cpus()
const load = setInterval(() => {
  const current = os.cpus()
  const rows = current.map((cpu, index) => {
    const total = Object.values(cpu.times).reduce((a, b) => a + b, 0) - Object.values(previous[index].times).reduce((a, b) => a + b, 0)
    const idle = cpu.times.idle - previous[index].times.idle
    return { total, busy: total - idle, percent: total ? 100 * (total - idle) / total : 0 }
  })
  const total = rows.reduce((sum, row) => sum + row.total, 0)
  fs.appendFileSync(path.join(directory, suite + '-load.ndjson'), JSON.stringify({ at: Date.now(), cpuPercent: 100 * rows.reduce((sum, row) => sum + row.busy, 0) / total, maxCorePercent: Math.max(...rows.map(row => row.percent)), freeMemory: os.freemem() }) + '\n')
  previous = current
}, 1000)

async function run(scenario, arm, repetition, sizeMiB, options = {}) {
  const label = `${suite}-${scenario}-${repetition}-${arm}`
  const row = { label, scenario, arm, repetition, sizeMiB, ...options, startedMs: Date.now() }
  ledger.runs.push(row); save()
  const log = fs.openSync(path.join(directory, label + '.log'), 'w')
  try {
    const child = spawn(process.execPath, ['--import', pathToFileURL(path.join(root, 'testdata/connectivity/webrtc/slowdown-20261022/loader.mjs')).href, path.join(root, 'testdata/connectivity/webrtc/adaptive-20261023/probe.mjs'), 'baseline', String(sizeMiB), '1', String(options.slowWriteMs ?? 0), options.interaction ?? 'none', '1', 'lan-isolated'], {
      cwd: root, windowsHide: true, stdio: ['ignore', log, log], env: { ...env, WS_RESEARCH_LABEL: label, WS_ADAPTIVE_ARM: arm, WS_ADAPTIVE_FAULT: options.fault ?? 'none', WS_PREFLIGHT_WORKFLOW: options.workflow ?? 'file', WS_PREFLIGHT_RECEIVERS: String(options.receivers ?? 1) },
    })
    const [code] = await once(child, 'close')
    row.exitCode = code
    const reports = fs.existsSync(path.join(directory, 'runs')) ? fs.readdirSync(path.join(directory, 'runs')).map(name => path.join(directory, 'runs', name, 'report.json')).filter(file => fs.existsSync(file)) : []
    for (const file of reports) {
      const report = JSON.parse(fs.readFileSync(file))
      if (report.label !== label) continue
      row.report = path.relative(root, file).replaceAll('\\', '/')
      row.status = report.status; row.error = report.error
      row.rounds = report.rounds.map(round => ({ MBps: round.MBps, firstOutputMs: round.activationToFirstWriteMs, completionMs: round.activationToCompleteMs, size: round.size, sha256: round.sha256, peers: round.peers.length, lifecycle: round.lifecycle, workerStatus: round.workerStatus, fault: round.fault, preview: round.preview, errors: round.errors }))
    }
    assert.equal(code, 0, label + ' process failed')
    assert.equal(row.status, 'passed', label + ' report failed')
  } catch (error) { row.failure = String(error); ledger.failures = (ledger.failures ?? 0) + 1 }
  finally { fs.closeSync(log); row.finishedMs = Date.now(); save(); console.log(JSON.stringify(row)) }
}

try {
  if (suite.startsWith('large')) {
    for (let repetition = 1; repetition <= 4; repetition++) {
      for (const arm of repetition % 2 ? ['single', 'adaptive'] : ['adaptive', 'single']) await run('file', arm, repetition, 256)
    }
  } else if (suite === 'workflows') {
    for (let repetition = 1; repetition <= 3; repetition++) for (const arm of repetition % 2 ? ['single', 'adaptive'] : ['adaptive', 'single']) await run('short', arm, repetition, 0.125)
    for (const arm of ['single', 'adaptive']) {
      for (let repetition = 1; repetition <= 2; repetition++) {
        await run('pressure', arm, repetition, 64, { slowWriteMs: 125 })
        await run('zip', arm, repetition, 64, { workflow: 'zip' })
        await run('competition', arm, repetition, 128, { receivers: 2 })
      }
    }
    await run('preview', 'adaptive', 1, 256, { interaction: 'preview' })
    await run('cancel-preview', 'adaptive', 1, 256, { interaction: 'cancel-preview' })
    await run('failure', 'adaptive', 1, 256, { fault: 'retire' })
  } else if (suite === 'confirmation') {
    for (const arm of ['single', 'adaptive']) await run('short', arm, 1, 0.125)
    await run('pressure', 'adaptive', 1, 64, { slowWriteMs: 125 })
    await run('zip', 'adaptive', 1, 64, { workflow: 'zip' })
    await run('competition', 'adaptive', 1, 128, { receivers: 2 })
    await run('preview', 'adaptive', 1, 256, { interaction: 'preview' })
    await run('cancel-preview', 'adaptive', 1, 256, { interaction: 'cancel-preview' })
    await run('failure', 'adaptive', 1, 256, { fault: 'retire' })
  } else throw new Error('Unknown suite: ' + suite)
  ledger.status = ledger.failures ? 'failed' : 'passed'
} finally { clearInterval(load); ledger.finishedMs = Date.now(); save() }
if (ledger.failures) process.exitCode = 1
