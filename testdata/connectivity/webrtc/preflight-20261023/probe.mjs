import assert from 'node:assert/strict'
import { readFile, writeFile, mkdir } from 'node:fs/promises'
import { createHash } from 'node:crypto'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { join } from 'node:path'
import { extendWorkflow } from './workflows.mjs'

const root = fileURLToPath(new URL('../../../../', import.meta.url))
const directory = join(root, '.tmp/webrtc-preflight-20261023')
const snapshot = join(directory, 'snapshot')
const config = {
  connections: Number(process.env.WS_PREFLIGHT_CONNECTIONS ?? 1),
  late: process.env.WS_PREFLIGHT_LATE === '1',
  fault: process.env.WS_PREFLIGHT_FAULT ?? 'none',
  profile: process.env.WS_PREFLIGHT_PROFILE === '1',
  cold: process.env.WS_PREFLIGHT_COLD === '1',
  launch: process.env.WS_PREFLIGHT_LAUNCH ?? 'serial',
  workflow: process.env.WS_PREFLIGHT_WORKFLOW ?? 'file',
  receivers: Number(process.env.WS_PREFLIGHT_RECEIVERS ?? 1),
}
assert([1, 2, 4].includes(config.connections))
assert(['none', 'retire', 'unavailable'].includes(config.fault))
assert(['serial', 'parallel'].includes(config.launch))
assert(['file', 'zip'].includes(config.workflow))
assert([1, 2].includes(config.receivers))
const manifest = JSON.parse(await readFile(join(directory, 'sources.json'), 'utf8'))
const binaries = JSON.parse(await readFile(join(directory, config.profile ? 'binaries-profile.json' : 'binaries.json'), 'utf8'))
process.env.WS_RESEARCH_BINARY_DIRECTORY = join(directory, config.profile ? 'binaries-profile' : 'binaries')
let source = (await readFile(new URL('../slowdown-20261022/full-file.mjs', import.meta.url), 'utf8')).replaceAll('\r\n', '\n')
function replace(before, after) {
  assert.equal(source.split(before).length, 2, 'Probe contract: ' + before)
  source = source.replace(before, after)
}
replace("from './linux-browser.mjs'", 'from ' + JSON.stringify(new URL('../slowdown-20261022/linux-browser.mjs', import.meta.url).href))
replace("from '../../../../web/e2e/fixtures/direct-product-stack'", 'from ' + JSON.stringify(pathToFileURL(join(snapshot, 'web/e2e/fixtures/direct-product-stack.ts')).href))
replace("fileURLToPath(new URL('../../../../', import.meta.url))", JSON.stringify(root))
replace("join(root, '.tmp/webrtc-slowdown-20261022', runId)", "join(root, '.tmp/webrtc-preflight-20261023/runs', runId)")
replace("process.env.WS_RESEARCH_PION_STATS = join(output, 'native-stats.ndjson')", "process.env.WS_RESEARCH_PION_STATS = join(output, 'native-stats.ndjson'); process.env.WS_PREFLIGHT_NATIVE_PROFILE = " + (config.profile ? 'output' : "''"))
replace('const report = { runId,', 'const report = { preflight: ' + JSON.stringify({ ...config, revision: manifest.revision, binaries }) + ', runId,')
replace('const errors = []', `const errors = []
    await (await import(${JSON.stringify(new URL('./routes.mjs', import.meta.url).href)})).installRoutes(page, ${JSON.stringify(config)})`)
replace('const fixture = process.env.WS_RESEARCH_FIXTURE ? await readFile(process.env.WS_RESEARCH_FIXTURE) : randomBytes(sizeMiB * 1024 * 1024)',
  'const fixture = (await readFile(join(' + JSON.stringify(directory) + ", 'fixture.bin'))).subarray(0, Math.round(sizeMiB * 1024 * 1024))")
replace("const modules = await loadProductModules()", `const modules = await loadProductModules()
        const researchConfig = ${JSON.stringify(config)};
        const blockTimings = [];
        const { V2SessionBlockLane } = await import('/src/content/v2-session-services.ts');
        const originalFetchBlock = V2SessionBlockLane.prototype.fetchBlock;
        V2SessionBlockLane.prototype.fetchBlock = async function(demand, signal, observer) {
          const timing = { index: String(demand.localBlockIndex), started: performance.now(), receivedBytes: 0 };
          blockTimings.push(timing);
          try {
            const record = await originalFetchBlock.call(this, demand, signal, {
              ...observer, received: bytes => {
                const now = performance.now();
                timing.firstReceived ??= now; timing.lastReceived = now; timing.receivedBytes += bytes;
                observer?.received(bytes);
              },
            });
            timing.authenticated = performance.now();
            return record;
          } catch (error) { timing.failed = String(error); throw error; }
        }`)
replace('protocolTrace: traceEnabled ? { current: observe } : undefined,', 'protocolTrace: traceEnabled ? { current: observe } : undefined, connectivityTrace: traceEnabled ? { current: observe } : undefined,')
replace("          activation = joined.beginDownloadConnectivity()",  "          const activationStarted = performance.now();\n          activation = joined.beginDownloadConnectivity()")
replace("while (!admissions.some(row => row.route === 'direct'))", "while (!researchConfig.cold && admissions.filter(row => row.route === 'direct').length < (researchConfig.late ? 1 : researchConfig.connections))")
replace("          await window.sampleSockets('before')", "          if (!researchConfig.cold) await window.sampleSockets('before')")
replace('          const started = performance.now()', `          await window.sampleTransferMetrics('before')
          const started = performance.now()
          let researchTimer
          if (researchConfig.late || researchConfig.fault !== 'none') {
            researchTimer = setTimeout(async () => {
              window.__planProbe.faultStateBefore = window.__preflightControl.state()
              if (researchConfig.late) window.__preflightControl.grow(researchConfig.connections)
              if (researchConfig.fault === 'retire') await window.__preflightControl.retireLast()
              if (researchConfig.fault === 'unavailable') window.__preflightControl.failOptional()
              window.__planProbe.faultStateAfter = window.__preflightControl.state()
            }, 1000)
          }`)
replace("          window.__planProbe.phase = 'hash'", `          clearTimeout(researchTimer)
          await window.sampleTransferMetrics('after')
          window.__planProbe.phase = 'hash'`)
replace('if (isolated) window.__planProbe.initialPeerStats.forEach(requireIsolatedLAN)', "if (isolated && !researchConfig.cold) window.__planProbe.initialPeerStats.forEach(requireIsolatedLAN)")
replace('if (isolated) requireIsolatedLAN(entries)', "if (isolated && peer.connectionState === 'connected') requireIsolatedLAN(entries)")
replace('started, timeOrigin: performance.timeOrigin,', `activationToFirstWriteMs: firstWrite - activationStarted, activationToCompleteMs: closed - activationStarted,
            researchEvents: window.__preflightControl.events, buffers: window.__preflightBuffers, blockTimings,
            started, timeOrigin: performance.timeOrigin,`)
replace("assert.equal(result.peers.length, 1, 'Single connection measurement')", `if (!${config.cold}) assert(result.admissions.filter(row => row.route === 'direct').length >= ${config.connections}, 'Requested paths were admitted')
      assert.equal(result.buffers.reserved, 0, 'No retained active block reservation')`)
replace('    try {\n      const task = page.evaluate', `    const transferMetrics = {}
    await page.exposeFunction('sampleTransferMetrics', async phase => {
      transferMetrics[phase] = { metrics: await cdp.send('Performance.getMetrics'), processes: await browserCDP.send('SystemInfo.getProcessInfo') }
      if (${config.profile}) {
        await writeFile(join(output, 'native-profile-control'), phase === 'before' ? 'start' : 'stop')
        const marker = join(output, phase === 'before' ? 'native-profile-ready' : 'native-profile-stopped')
        const deadline = Date.now() + 5000
        while (!await readFile(marker).then(() => true, () => false)) {
          if (Date.now() > deadline) throw new Error('Native profiler marker missing: ' + marker)
          await new Promise(resolve => setTimeout(resolve, 25))
        }
        if (phase === 'before') {
          await cdp.send('Profiler.enable')
          await cdp.send('Profiler.setSamplingInterval', { interval: 1000 })
          await cdp.send('Profiler.start')
        } else {
          await writeFile(join(output, n + '-cpu-profile.json'), JSON.stringify(await cdp.send('Profiler.stop')))
        }
      }
    })
    try {
      const task = page.evaluate`)
replace('const row = { repetition: n,', 'const row = { transferMetrics, repetition: n,')
replace("if (remoteBrowser && n === 0) report.outputControl", "if (n === 0) report.outputControl")
// Loopback cannot sample Linux sockets as evidence for the local flow.
replace('await page.exposeFunction(\'sampleSockets\', sampleSockets)', "await page.exposeFunction('sampleSockets', remoteBrowser ? sampleSockets : () => {})")
source = await extendWorkflow(source, config)
await mkdir(join(directory, 'drivers'), { recursive: true })
const generated = join(directory, 'drivers', 'probe-' + Date.now() + '.mjs')
await writeFile(generated, source)
await writeFile(generated + '.routes.mjs', await readFile(new URL('./routes.mjs', import.meta.url)))
await writeFile(generated + '.json', JSON.stringify({ config, sha256: createHash('sha256').update(source).digest('hex') }, null, 2))
await import(pathToFileURL(generated).href)
