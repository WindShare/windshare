import assert from 'node:assert/strict'
import { readFile, writeFile, mkdir } from 'node:fs/promises'
import { createHash } from 'node:crypto'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { join } from 'node:path'
import { extendWorkflow } from '../preflight-20261023/workflows.mjs'
import { experimentDirectory } from '../experiment-paths.mjs'

const root = fileURLToPath(new URL('../../../../', import.meta.url))
const directory = experimentDirectory(root, process.env.WS_ADAPTIVE_BUILD ?? 'webrtc-adaptive-20261023')
const snapshot = join(directory, 'snapshot')
const config = {
  arm: process.env.WS_ADAPTIVE_ARM ?? 'adaptive',
  workflow: process.env.WS_PREFLIGHT_WORKFLOW ?? 'file',
  receivers: Number(process.env.WS_PREFLIGHT_RECEIVERS ?? 1),
  fault: process.env.WS_ADAPTIVE_FAULT ?? 'none',
  turnOnly: process.env.WS_STUDY_TURN_CONFIG !== undefined,
}
assert(['adaptive', 'single'].includes(config.arm))
assert(['none', 'retire'].includes(config.fault))
const manifest = JSON.parse(await readFile(join(directory, 'sources.json'), 'utf8'))
const binaries = JSON.parse(await readFile(join(directory, 'binaries.json'), 'utf8'))
process.env.WS_RESEARCH_BINARY_DIRECTORY = join(directory, 'binaries')
let source = (await readFile(new URL('../slowdown-20261022/full-file.mjs', import.meta.url), 'utf8')).replaceAll('\r\n', '\n')
function replace(before, after) {
  assert.equal(source.split(before).length, 2, 'Probe anchor: ' + before)
  source = source.replace(before, after)
}
replace("from './linux-browser.mjs'", 'from ' + JSON.stringify(new URL('../slowdown-20261022/linux-browser.mjs', import.meta.url).href))
replace("from '../../../../web/e2e/fixtures/direct-product-stack'", 'from ' + JSON.stringify(pathToFileURL(join(snapshot, 'web/e2e/fixtures/direct-product-stack.ts')).href))
replace("fileURLToPath(new URL('../../../../', import.meta.url))", JSON.stringify(root))
replace("join(root, '.tmp/webrtc-slowdown-20261022', runId)", "join(" + JSON.stringify(directory) + ", 'runs', runId)")
replace('const report = { runId,', 'const report = { study: ' + JSON.stringify({ ...config, revision: manifest.revision, binaries }) + ', runId,')
replace('const errors = []', `const errors = []
  await (await import(${JSON.stringify(new URL('./routes.mjs', import.meta.url).href)})).installRoutes(page, ${JSON.stringify(config)})`)
replace('const fixture = process.env.WS_RESEARCH_FIXTURE ? await readFile(process.env.WS_RESEARCH_FIXTURE) : randomBytes(sizeMiB * 1024 * 1024)',
  'const fixture = (await readFile(process.env.WS_RESEARCH_FIXTURE ?? join(' + JSON.stringify(directory) + ", 'fixture.bin'))).subarray(0, Math.round(sizeMiB * 1024 * 1024))")
replace('protocolTrace: traceEnabled ? { current: observe } : undefined,',
  'protocolTrace: traceEnabled ? { current: observe } : undefined, connectivityTrace: { current: observe },')
replace("          activation = joined.beginDownloadConnectivity()", "          const activationStarted = performance.now();\n          activation = joined.beginDownloadConnectivity()")
// Start content immediately. Readiness of extra connections is never a measurement barrier.
replace("while (!admissions.some(row => row.route === 'direct'))", "while (false)")
replace("if (isolated) window.__planProbe.initialPeerStats.forEach(requireIsolatedLAN)", "")
replace("          await window.sampleSockets('before')", "")
replace("if (isolated) requireIsolatedLAN(entries)", "if (isolated && peer.connectionState === 'connected') requireIsolatedLAN(entries)")
replace("          const started = performance.now()", "          await window.sampleTransferMetrics('before')\n          const started = performance.now()")
replace('started, timeOrigin: performance.timeOrigin,', 'activationToFirstWriteMs: firstWrite - activationStarted, activationToCompleteMs: closed - activationStarted, demandTimeline: window.__adaptiveDemand, fault: window.__adaptiveFault, started, timeOrigin: performance.timeOrigin,')
replace("assert.equal(result.peers.length, 1, 'Single connection measurement')", "assert(result.peers.length >= 1)")
replace("await page.exposeFunction('sampleSockets', sampleSockets)", "await page.exposeFunction('sampleSockets', remoteBrowser ? sampleSockets : () => {})\n await page.exposeFunction('sampleTransferMetrics', () => {})")
source = await extendWorkflow(source, config)
if (config.turnOnly) {
  assert.equal(config.receivers, 1)
  const turn = JSON.parse(await readFile(process.env.WS_STUDY_TURN_CONFIG, 'utf8'))
  replace('senderTrace: traceEnabled })', 'senderTrace: traceEnabled, turn: ' + JSON.stringify(turn) + ' })')
  replace("policy: 'p2p-only', nativePeerUsable: () => true,", "policy: 'auto', nativePeerUsable: () => true,")
  replace('configuration: { iceServers: [] }', "configuration: { iceServers: [], iceTransportPolicy: 'relay' }")
  replace("assert.deepEqual(Object.keys(result.dispatches), ['direct'])", "assert.deepEqual(Object.keys(result.dispatches), ['turn'])")
}
await mkdir(join(directory, 'drivers'), { recursive: true })
const generated = join(directory, 'drivers', 'probe-' + Date.now() + '.mjs')
await writeFile(generated, source)
await writeFile(generated + '.json', JSON.stringify({ config, sha256: createHash('sha256').update(source).digest('hex') }, null, 2))
await import(pathToFileURL(generated).href)
