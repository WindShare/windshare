import assert from 'node:assert/strict'
import { readFile, writeFile, mkdir } from 'node:fs/promises'
import { createHash } from 'node:crypto'
import { execFileSync } from 'node:child_process'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { join } from 'node:path'

const root = fileURLToPath(new URL('../../../../', import.meta.url))
const directory = join(root, '.tmp/webrtc-interleaved-20261022')
const arm = process.env.WS_RETEST_ARM
assert(['old', 'new', 'reserve-zero', 'readiness-wake'].includes(arm), 'Explicit arm is required')
const snapshot = arm === 'old' ? 'old' : 'new'
const revision = execFileSync('git', ['rev-parse', snapshot === 'old' ? '197fe6f4' : '86446a6a'], { cwd: root, encoding: 'utf8', windowsHide: true }).trim()
const binary = join(process.env.WS_RESEARCH_BINARY_DIRECTORY ?? join(root, '.tmp/single-connection-implementation/baseline'), 'wind.exe')
const stage = process.env.WS_RETEST_STAGE ?? 'baseline'
const diagnostic = process.env.WS_RETEST_DIAGNOSTIC === '1'
const metadata = { arm, revision, stage, diagnostic, cohort: process.env.WS_RETEST_COHORT ?? 'initial', pair: process.env.WS_RETEST_PAIR, order: process.env.WS_RETEST_ORDER, senderSHA256: createHash('sha256').update(await readFile(binary)).digest('hex') }
let source = await readFile(new URL('../slowdown-20261022/full-file.mjs', import.meta.url), 'utf8')
function replace(before, after) {
  assert.equal(source.split(before).length, 2, 'Probe contract: ' + before)
  source = source.replace(before, after)
}
replace("from './linux-browser.mjs'", 'from ' + JSON.stringify(new URL('../slowdown-20261022/linux-browser.mjs', import.meta.url).href))
replace("from '../../../../web/e2e/fixtures/direct-product-stack'", 'from ' + JSON.stringify(pathToFileURL(join(directory, snapshot, 'web/e2e/fixtures/direct-product-stack.ts')).href))
replace("fileURLToPath(new URL('../../../../', import.meta.url))", JSON.stringify(root))
replace("join(root, '.tmp/webrtc-slowdown-20261022', runId)", "join(root, '.tmp/webrtc-interleaved-20261022/runs', runId)")
replace('const report = { runId,', 'const report = { retest: ' + JSON.stringify(metadata) + ', runId,')
replace('const errors = []', `const errors = []
    await (await import(${JSON.stringify(new URL('./mutations.mjs', import.meta.url).href)})).installRoutes(page, ${JSON.stringify({ arm, counters: process.env.WS_RETEST_COUNTERS === '1' })})`)
replace('started, timeOrigin: performance.timeOrigin,', 'admissionCounters: window.__retestAdmissionCounters, started, timeOrigin: performance.timeOrigin,')
replace('    try {\n      const task = page.evaluate', `    const transferMetrics = {}
    await page.exposeFunction('sampleTransferMetrics', async phase => {
      transferMetrics[phase] = { metrics: await cdp.send('Performance.getMetrics'), processes: await browserCDP.send('SystemInfo.getProcessInfo') }
    })
    try {
      const task = page.evaluate`)
replace("          const started = performance.now()", "          await window.sampleTransferMetrics('before')\n          const started = performance.now()")
replace("          window.__planProbe.phase = 'hash'", "          await window.sampleTransferMetrics('after')\n          window.__planProbe.phase = 'hash'")
replace('const row = { repetition: n,', 'const row = { transferMetrics, repetition: n,')
replace("scope: 'Production CLI file read/seal, protocol, browser authentication and SingleFileStreamOutputSession to the recorded sinkMode (OPFS or memory). Persistent browser profile; production p2p-only policy; existing test plan-settlement adapter. Connection/catalog setup and post-close SHA-256 excluded from throughput. Not UI, crash durability, resume or WAN evidence.'", "scope: 'Frozen old/new browser snapshots, same production sender binary and shared fixture. Production CLI read/seal, transport, browser authentication and OPFS close. Setup and final hash excluded. Existing test plan-settlement adapter. Transfer-only CDP counters sampled outside the timed interval; broader legacy counters retained separately. Single verified UDP LAN path; not WAN, UI, cold-disk or crash durability evidence.'")
if (diagnostic) source = (await import('./diagnostics.mjs')).instrument(source)
await mkdir(directory, { recursive: true })
const generated = join(directory, 'probe-' + stage + '-' + arm + '.mjs')
await writeFile(generated, source)
await import(pathToFileURL(generated).href)
