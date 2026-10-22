import assert from 'node:assert/strict'
import { readFile, writeFile, mkdir } from 'node:fs/promises'
import { createHash } from 'node:crypto'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { join } from 'node:path'

const root = fileURLToPath(new URL('../../../../', import.meta.url))
const directory = join(root, '.tmp/webrtc-readiness-20261022')
const arm = process.env.WS_RETEST_ARM
assert(['old', 'new'].includes(arm))
const manifest = JSON.parse(await readFile(join(directory, 'snapshots.json'), 'utf8'))
const binary = join(process.env.WS_RESEARCH_BINARY_DIRECTORY, 'wind.exe')
const stage = process.env.WS_RETEST_STAGE
const metadata = { arm, baseRevision: manifest.baseRevision, snapshotSHA256: manifest[arm].sha256, stage,
  cohort: process.env.WS_RETEST_COHORT, pair: process.env.WS_RETEST_PAIR, order: process.env.WS_RETEST_ORDER,
  senderSHA256: createHash('sha256').update(await readFile(binary)).digest('hex') }
let source = await readFile(new URL('../slowdown-20261022/full-file.mjs', import.meta.url), 'utf8')
function replace(before, after) {
  assert.equal(source.split(before).length, 2, 'Probe contract: ' + before)
  source = source.replace(before, after)
}
replace("from './linux-browser.mjs'", 'from ' + JSON.stringify(new URL(stage === 'profile' ? './linux-browser.mjs' : '../slowdown-20261022/linux-browser.mjs', import.meta.url).href))
replace("from '../../../../web/e2e/fixtures/direct-product-stack'", 'from ' + JSON.stringify(pathToFileURL(join(directory, arm, 'web/e2e/fixtures/direct-product-stack.ts')).href))
replace("fileURLToPath(new URL('../../../../', import.meta.url))", JSON.stringify(root))
replace("join(root, '.tmp/webrtc-slowdown-20261022', runId)", "join(root, '.tmp/webrtc-readiness-20261022/runs', runId)")
replace('const report = { runId,', 'const report = { retest: ' + JSON.stringify(metadata) + ', runId,')
replace('const errors = []', `const errors = []
    await (await import(${JSON.stringify(new URL('../interleaved-20261022/mutations.mjs', import.meta.url).href)})).installRoutes(page, { arm: 'new', counters: true })`)
replace('started, timeOrigin: performance.timeOrigin,', 'admissionCounters: window.__retestAdmissionCounters, started, timeOrigin: performance.timeOrigin,')
replace('    try {\n      const task = page.evaluate', `    const transferMetrics = {}
    await page.exposeFunction('sampleTransferMetrics', async phase => {
      transferMetrics[phase] = { metrics: await cdp.send('Performance.getMetrics'), processes: await browserCDP.send('SystemInfo.getProcessInfo') }
    })
    try {
      const task = page.evaluate`)
replace('          const started = performance.now()', "          await window.sampleTransferMetrics('before')\n          const started = performance.now()")
replace("          window.__planProbe.phase = 'hash'", "          await window.sampleTransferMetrics('after')\n          window.__planProbe.phase = 'hash'")
replace('const row = { repetition: n,', 'const row = { transferMetrics, repetition: n,')
if (stage === 'profile') source = (await import('../interleaved-20261022/diagnostics.mjs')).instrument(source)
await mkdir(directory, { recursive: true })
const generated = join(directory, 'probe-' + stage + '-' + arm + '.mjs')
await writeFile(generated, source)
await import(pathToFileURL(generated).href)
