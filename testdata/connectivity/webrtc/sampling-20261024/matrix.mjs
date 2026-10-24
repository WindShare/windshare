import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { createHash } from 'node:crypto'
import { spawn } from 'node:child_process'
import { once } from 'node:events'
import { fileURLToPath } from 'node:url'

const root = fileURLToPath(new URL('../../../../', import.meta.url))
const build = process.env.WS_ADAPTIVE_BUILD ?? 'webrtc-sampling-stress-20261024'
const directory = path.join(root, '.tmp', build)
const suite = process.argv[2]
const configuration = JSON.parse(fs.readFileSync(process.argv[3], 'utf8'))
const cases = Array.isArray(configuration) ? configuration : configuration[suite]
assert(suite && Array.isArray(cases))
const output = path.join(directory, suite + '-ledger.json')
assert(!fs.existsSync(output), 'Never overwrite evidence')
const lab = JSON.parse(fs.readFileSync(path.join(root, '.tmp/webrtc-preflight-20261023/lab.json'), 'utf8'))
const sha = file => createHash('sha256').update(fs.readFileSync(file)).digest('hex')
const ledger = { suite, sourcesSHA256: sha(path.join(directory, 'sources.json')), startedMs: Date.now(), runs: [] }
const save = () => fs.writeFileSync(output, JSON.stringify(ledger, null, 2))
for (let index = 0; index < cases.length; index++) {
  const config = cases[index]
  const label = suite + '-' + index + '-' + (config.name ?? 'case')
  const runBuild = config.build ?? build
  const runDirectory = path.join(root, '.tmp', runBuild)
  const row = { ...config, build: runBuild, label, startedMs: Date.now(), sourcesSHA256: sha(path.join(runDirectory, 'sources.json')) }
  ledger.runs.push(row); save()
  const log = fs.openSync(path.join(directory, label + '.log'), 'w')
  try {
    const env = { ...process.env, ...lab, WS_ADAPTIVE_BUILD: runBuild,
      WS_SAMPLING_FAULT: config.fault ?? 'none', WS_PREFLIGHT_WORKFLOW: config.workflow ?? 'file',
      WS_PREFLIGHT_RECEIVERS: String(config.receivers ?? 1),
      WS_BOTTLENECK_PATHS: String(config.paths ?? 0), WS_BOTTLENECK_WARM: config.warm ? '1' : '0',
      WS_BOTTLENECK_PROFILE: config.profile ? '1' : '0', WS_BOTTLENECK_PROFILE_FLAVOR: config.profileFlavor ?? '', WS_BOTTLENECK_POLICY: config.policy ?? 'original',
      WS_RESEARCH_TRACE: config.trace ? 'on' : 'off', WS_RESEARCH_OUTPUT: config.sink ?? 'opfs',
      WS_RESEARCH_NETLOG: '0', WS_RESEARCH_LARGE_BUFFER: '0', WS_RESEARCH_ISOLATE_BROWSER_DNS: '1',
      WS_RESEARCH_LABEL: label,
      WS_RESEARCH_FIXTURE: path.resolve(runDirectory, config.fixture ?? 'fixture.bin'),
    }
    const child = spawn(process.execPath, ['--import', './testdata/connectivity/webrtc/slowdown-20261022/loader.mjs',
      './testdata/connectivity/webrtc/sampling-20261024/probe.mjs', 'baseline', String(config.sizeMiB ?? 256),
      '1', String(config.slowWriteMs ?? 0), config.interaction ?? 'none', String(config.blockMiB ?? 1), config.topology ?? 'lan-isolated'],
      { cwd: root, windowsHide: true, stdio: ['ignore', log, log], env })
    const [code] = await once(child, 'close')
    row.exitCode = code
    for (const name of fs.readdirSync(path.join(runDirectory, 'runs'))) {
      const file = path.join(runDirectory, 'runs', name, 'report.json')
      if (!fs.existsSync(file)) continue
      const report = JSON.parse(fs.readFileSync(file, 'utf8'))
      if (report.label !== label) continue
      row.report = path.relative(root, file).replaceAll('\\', '/')
      row.reportSHA256 = sha(file); row.status = report.status; row.error = report.error
      row.rounds = report.rounds.map(r => ({
        MBps: r.MBps, clickMBps: r.size / r.activationToCompleteMs / 1000,
        completionMs: r.activationToCompleteMs, firstOutputMs: r.activationToFirstWriteMs,
        peers: r.peers.length, fault: r.fault, interactive: r.interactive, bytes: r.size, sha256: r.sha256, errors: r.errors,
      }))
    }
    assert.equal(code, 0); assert.equal(row.status, 'passed')
  } catch (error) { row.failure = String(error) }
  finally { fs.closeSync(log); row.finishedMs = Date.now(); save(); console.log(JSON.stringify(row)) }
}
ledger.status = ledger.runs.some(r => r.failure) ? 'failed' : 'passed'
ledger.finishedMs = Date.now(); save()
if (ledger.status === 'failed') process.exitCode = 1
