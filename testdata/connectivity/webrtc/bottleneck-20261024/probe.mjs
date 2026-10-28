import assert from 'node:assert/strict'
import { readFile, writeFile, mkdir } from 'node:fs/promises'
import { createHash } from 'node:crypto'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { join } from 'node:path'
import { experimentDirectory } from '../experiment-paths.mjs'

const root = fileURLToPath(new URL('../../../../', import.meta.url))
const directory = experimentDirectory(root, process.env.WS_ADAPTIVE_BUILD ?? 'webrtc-bottleneck-20261024-baseline')
const snapshot = join(directory, 'snapshot')
const config = {
  paths: Number(process.env.WS_BOTTLENECK_PATHS ?? 0),
  warm: process.env.WS_BOTTLENECK_WARM === '1',
  profile: process.env.WS_BOTTLENECK_PROFILE === '1',
  policy: process.env.WS_BOTTLENECK_POLICY ?? 'original',
}
assert([0, 1, 2, 3].includes(config.paths))
const manifest = JSON.parse(await readFile(join(directory, 'sources.json'), 'utf8'))
const binaryName = config.profile ? 'binaries-profile' + (process.env.WS_BOTTLENECK_PROFILE_FLAVOR ? '-' + process.env.WS_BOTTLENECK_PROFILE_FLAVOR : '') : 'binaries'
const binaryDirectory = join(directory, binaryName)
const binaries = JSON.parse(await readFile(join(directory, binaryName + '.json'), 'utf8'))
process.env.WS_RESEARCH_BINARY_DIRECTORY = binaryDirectory
for (const name of ['wind.exe', 'relay-native.exe']) {
  assert.equal(createHash('sha256').update(await readFile(join(binaryDirectory, name))).digest('hex'), binaries[name])
}
let source = (await readFile(new URL('../slowdown-20261022/full-file.mjs', import.meta.url), 'utf8')).replaceAll('\r\n', '\n')
function replace(before, after) {
  assert.equal(source.split(before).length, 2, 'Probe anchor: ' + before)
  source = source.replace(before, after)
}
replace('sizeMiB <= 512', 'sizeMiB <= 2048')
replace("from './linux-browser.mjs'", 'from ' + JSON.stringify(new URL('../slowdown-20261022/linux-browser.mjs', import.meta.url).href))
replace("from '../../../../web/e2e/fixtures/direct-product-stack'", 'from ' + JSON.stringify(pathToFileURL(join(snapshot, 'web/e2e/fixtures/direct-product-stack.ts')).href))
replace("fileURLToPath(new URL('../../../../', import.meta.url))", JSON.stringify(root))
replace("join(root, '.tmp/webrtc-slowdown-20261022', runId)", "join(" + JSON.stringify(directory) + ", 'runs', runId)")
replace("process.env.WS_RESEARCH_PION_STATS = join(output, 'native-stats.ndjson')", "process.env.WS_RESEARCH_PION_STATS = join(output, 'native-stats.ndjson'); process.env.WS_PREFLIGHT_NATIVE_PROFILE = " + (config.profile ? 'output' : "''"))
replace('const report = { runId,', 'const report = { study: ' + JSON.stringify({ ...config, revision: manifest.revision, binaries }) + ', runId,')
replace('const errors = []', "const errors = []; await (await import(" + JSON.stringify(new URL('./routes.mjs', import.meta.url).href) + ")).installRoutes(page, " + JSON.stringify(config) + ");")
replace('const fixture = process.env.WS_RESEARCH_FIXTURE ? await readFile(process.env.WS_RESEARCH_FIXTURE) : randomBytes(sizeMiB * 1024 * 1024)',
  'const fixture = (await readFile(process.env.WS_RESEARCH_FIXTURE ?? join(' + JSON.stringify(directory) + ", 'fixture.bin'))).subarray(0, Math.round(sizeMiB * 1024 * 1024))")
replace('protocolTrace: traceEnabled ? { current: observe } : undefined,',
  'protocolTrace: traceEnabled ? { current: observe } : undefined, connectivityTrace: { current: observe },')
replace('const modules = await loadProductModules()', `const modules = await loadProductModules()
  const blockTimings = [];
  const { V2SessionBlockLane } = await import('/src/content/v2-session-services.ts');
  const fetchBlock = V2SessionBlockLane.prototype.fetchBlock;
  V2SessionBlockLane.prototype.fetchBlock = async function(demand, signal, observer) {
    const timing = { laneId: this.id, index: String(demand.localBlockIndex), started: performance.now(), receivedBytes: 0 };
    blockTimings.push(timing);
    try {
      const record = await fetchBlock.call(this, demand, signal, { ...observer, received: bytes => {
        const now = performance.now(); timing.firstReceived ??= now; timing.lastReceived = now; timing.receivedBytes += bytes;
        observer?.received(bytes);
      } });
      timing.authenticated = performance.now(); return record;
    } catch (error) { timing.failed = String(error); throw error; }
  };`)
replace('          activation = joined.beginDownloadConnectivity()', '          const activationStarted = performance.now();\n          activation = joined.beginDownloadConnectivity()')
replace("while (!admissions.some(row => row.route === 'direct'))", config.warm ? "while (admissions.filter(row => row.route === 'direct').length < " + Math.max(1, config.paths) + ")" : "while (false)")
replace('if (isolated) window.__planProbe.initialPeerStats.forEach(requireIsolatedLAN)', config.warm ? 'if (isolated) window.__planProbe.initialPeerStats.forEach(requireIsolatedLAN)' : '')
replace("          await window.sampleSockets('before')", '')
replace("          const started = performance.now()", "          await window.sampleTransferMetrics('before')\n          const started = performance.now()")
replace("          window.__planProbe.phase = 'hash'", "          await window.sampleTransferMetrics('after')\n          window.__planProbe.phase = 'hash'")
replace("if (isolated) requireIsolatedLAN(entries)", "if (isolated && peer.connectionState === 'connected') requireIsolatedLAN(entries)")
replace('started, timeOrigin: performance.timeOrigin,', 'activationToFirstWriteMs: firstWrite - activationStarted, activationToCompleteMs: closed - activationStarted, demandTimeline: window.__bottleneckDemand, blockTimings, started, timeOrigin: performance.timeOrigin,')
replace("assert.equal(result.peers.length, 1, 'Single connection measurement')", config.paths ? 'assert.equal(result.peers.length, ' + config.paths + ')' : 'assert(result.peers.length >= 1)')
replace("await page.exposeFunction('sampleSockets', sampleSockets)", "await page.exposeFunction('sampleSockets', remoteBrowser ? sampleSockets : () => {})")
replace('    try {\n      const task = page.evaluate', `    const transferMetrics = {}
    await page.exposeFunction('sampleTransferMetrics', async phase => {
      transferMetrics[phase] = { metrics: await cdp.send('Performance.getMetrics'), processes: await browserCDP.send('SystemInfo.getProcessInfo') };
      if (${config.profile}) {
        await writeFile(join(output, 'native-profile-control'), phase === 'before' ? 'start' : 'stop');
        const marker = join(output, phase === 'before' ? 'native-profile-ready' : 'native-profile-stopped');
        const deadline = Date.now() + 5000;
        while (!await readFile(marker).then(() => true, () => false)) {
          if (Date.now() > deadline) throw new Error('Native profiler marker missing: ' + marker);
          await new Promise(resolve => setTimeout(resolve, 25));
        }
        if (phase === 'before') {
          await cdp.send('Profiler.enable');
          await cdp.send('Profiler.setSamplingInterval', { interval: 1000 });
          await cdp.send('Profiler.start');
        } else await writeFile(join(output, n + '-cpu-profile.json'), JSON.stringify(await cdp.send('Profiler.stop')));
      }
    });
    try {
      const task = page.evaluate`)
replace('const row = { repetition: n,', 'const row = { transferMetrics, repetition: n,')
await mkdir(join(directory, 'drivers'), { recursive: true })
const generated = join(directory, 'drivers', 'bottleneck-' + Date.now() + '.mjs')
const frozenRoutes = generated + '.routes.mjs'
const routeSource = await readFile(new URL('./routes.mjs', import.meta.url), 'utf8')
source = source.replace(JSON.stringify(new URL('./routes.mjs', import.meta.url).href), JSON.stringify(pathToFileURL(frozenRoutes).href))
await writeFile(frozenRoutes, routeSource)
await writeFile(generated, source)
await writeFile(generated + '.json', JSON.stringify({ config, routesSHA256: createHash('sha256').update(routeSource).digest('hex'), sha256: createHash('sha256').update(source).digest('hex') }, null, 2))
await import(pathToFileURL(generated).href)
