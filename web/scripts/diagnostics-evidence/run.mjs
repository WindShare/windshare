/* global console, process, setTimeout, clearTimeout */

import { createHash } from 'node:crypto'
import { writeFile } from 'node:fs/promises'
import { cpus } from 'node:os'
import { dirname, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { chromium } from '@playwright/test'
import { createServer } from 'vite'

const SCRIPT_ROOT = dirname(fileURLToPath(import.meta.url))
const WEB_ROOT = resolve(SCRIPT_ROOT, '..', '..')
const REPOSITORY_ROOT = resolve(WEB_ROOT, '..')
const DEFAULT_ENTRY_COUNT = 32_768
const DEFAULT_PAGE_SIZE = 256
const DEFAULT_SAMPLE_COUNT = 3
const DEFAULT_WARMUP_COUNT = 1
const RUN_TIMEOUT_MS = 120_000
const BYTES_PER_MEBIBYTE = 1_048_576
const PERCENTILE_SCALE = 100
const DECIMAL_PLACES = 2
const RUNNER_COMPONENT = 'windshare-diagnostics-evidence-runner'
const RUN_OPERATION_ID = 'paired-large-directory'

async function main() {
  const options = parseArguments(process.argv.slice(2))
  milestone('started', {
    profile: {
      entries: options.entries,
      pageSize: options.pageSize,
      samples: options.samples,
      warmups: options.warmups,
      writesReport: options.output !== undefined,
    },
  })
  const vite = await startViteServer()
  milestone('browser_server_ready')
  let browser
  try {
    browser = await chromium.launch({
      headless: true,
      args: ['--enable-precise-memory-info', '--js-flags=--expose-gc'],
    })
    const environment = environmentSnapshot(browser.version())
    await runWarmups(browser, vite.origin, options)
    milestone('warmups_completed', { warmupPairs: options.warmups })
    const samples = await runPairedSamples(browser, vite.origin, options)
    const report = createReport(options, environment, samples)
    verifyReport(report)
    const markdown = renderMarkdown(report)
    console.log(JSON.stringify(report, null, 2))
    if (options.output !== undefined) {
      await writeFile(options.output, markdown, 'utf8')
      milestone('report_written', { path: relative(REPOSITORY_ROOT, options.output) })
    }
    milestone('completed', { decisionFingerprintSha256: report.decisionFingerprintSha256 })
  } finally {
    await Promise.allSettled([
      ...(browser === undefined ? [] : [browser.close()]),
      vite.server.close(),
    ])
  }
}

async function runWarmups(browser, origin, options) {
  for (let warmup = 0; warmup < options.warmups; warmup += 1) {
    await runBrowserSample(browser, origin, options, false)
    await runBrowserSample(browser, origin, options, true)
  }
}

async function runPairedSamples(browser, origin, options) {
  const pairs = []
  let baselineDecisions
  for (let pair = 0; pair < options.samples; pair += 1) {
    const order = pair % 2 === 0 ? [false, true] : [true, false]
    milestone('pair_started', { pair: pair + 1, order: order.map(modeName) })
    const completed = new Map()
    for (const traceEnabled of order) {
      completed.set(traceEnabled, await runBrowserSample(browser, origin, options, traceEnabled))
    }
    const disabled = completed.get(false)
    const enabled = completed.get(true)
    assertPairIntegrity(disabled, enabled, options)
    const decisions = stableJson(disabled.decisions)
    if (baselineDecisions !== undefined && decisions !== baselineDecisions) {
      throw new Error(`pair ${pair + 1} changed deterministic workload decisions`)
    }
    baselineDecisions = decisions
    pairs.push(Object.freeze({ pair: pair + 1, order: order.map(modeName), disabled, enabled }))
    milestone('pair_completed', { pair: pair + 1 })
  }
  return Object.freeze(pairs)
}

async function runBrowserSample(browser, origin, options, traceEnabled) {
  const page = await browser.newPage()
  page.setDefaultTimeout(RUN_TIMEOUT_MS)
  try {
    await page.goto(`${origin}/scripts/diagnostics-evidence/index.html`, {
      waitUntil: 'networkidle',
      timeout: RUN_TIMEOUT_MS,
    })
    const session = await page.context().newCDPSession(page)
    await session.send('HeapProfiler.collectGarbage')
    return await page.evaluate(async (input) => {
      const collect = globalThis.gc
      if (typeof collect === 'function') collect()
      return globalThis.runWindShareDiagnosticsEvidence(input)
    }, {
      entryCount: options.entries,
      pageSize: options.pageSize,
      traceEnabled,
    })
  } finally {
    await page.close()
  }
}

function assertPairIntegrity(disabled, enabled, options) {
  if (stableJson(disabled.decisions) !== stableJson(enabled.decisions)) {
    throw new Error('trace off/on changed product, lane, or settlement decisions')
  }
  if (stableJson(disabled.usefulWork) !== stableJson(enabled.usefulWork)) {
    throw new Error('trace off/on changed useful work')
  }
  if (disabled.usefulWork.entries !== options.entries) {
    throw new Error('workload did not process the configured entry count')
  }
  if (disabled.trace.payloadConstructionCount !== 0) {
    throw new Error('trace-disabled workload constructed trace payloads')
  }
  for (const field of [
    'retainedIncidentCount',
    'retainedIncidentBytes',
    'retainedEventCount',
    'retainedEventBytes',
    ...healthFields(),
  ]) {
    if (disabled.trace[field] !== '0') {
      throw new Error(`trace-disabled ${field} was not zero`)
    }
  }
  if (enabled.trace.payloadConstructionCount <= 0 || BigInt(enabled.trace.retainedEventCount) <= 0n) {
    throw new Error('trace-enabled workload retained no production trace evidence')
  }
  if (enabled.trace.state !== 'sealed' || enabled.trace.sealReason !== 'manual_disable') {
    throw new Error('trace-enabled workload did not seal through the production switch')
  }
  if (disabled.heartbeatDelayMilliseconds.length === 0 ||
      enabled.heartbeatDelayMilliseconds.length === 0) {
    throw new Error('workload was too short to measure event-loop responsiveness')
  }
}

function createReport(options, environment, pairs) {
  const disabled = pairs.map((pair) => pair.disabled)
  const enabled = pairs.map((pair) => pair.enabled)
  const decisionJson = stableJson(disabled[0].decisions)
  return Object.freeze({
    schema: 'windshare_diagnostics_performance_evidence_v1',
    recordedAt: new Date().toISOString(),
    profile: Object.freeze({
      entries: options.entries,
      pageSize: options.pageSize,
      samplesPerMode: options.samples,
      warmupsPerMode: options.warmups,
      pairing: 'alternating_order_within_one_browser',
    }),
    environment,
    decisionFingerprintSha256: createHash('sha256').update(decisionJson).digest('hex'),
    decisionsIdentical: true,
    decisions: disabled[0].decisions,
    summary: Object.freeze({
      traceOff: summarizeSamples(disabled),
      traceOn: summarizeSamples(enabled),
    }),
    pairs,
  })
}

function summarizeSamples(samples) {
  const wall = samples.map((sample) => sample.wallMilliseconds)
  const bytes = Number(samples[0].usefulWork.bytes)
  const entries = samples[0].usefulWork.entries
  const heartbeat = samples.flatMap((sample) => sample.heartbeatDelayMilliseconds)
  const heaps = samples.map((sample) => sample.heap)
  const heapAvailable = heaps.every((heap) => heap !== null)
  return Object.freeze({
    wallMillisecondsMedian: round(percentile(wall, 50)),
    entriesPerSecondMedian: round(percentile(wall.map((value) => entries * 1000 / value), 50)),
    logicalMebibytesPerSecondMedian: round(percentile(
      wall.map((value) => bytes * 1000 / value / BYTES_PER_MEBIBYTE),
      50,
    )),
    eventLoopDelayMilliseconds: Object.freeze({
      p50: round(percentile(heartbeat, 50)),
      p95: round(percentile(heartbeat, 95)),
      max: round(Math.max(...heartbeat)),
      observations: heartbeat.length,
    }),
    heap: heapAvailable
      ? Object.freeze({
          source: heaps[0].source,
          beforeBytesMedian: Math.round(percentile(heaps.map((heap) => heap.beforeBytes), 50)),
          peakBytesMedian: Math.round(percentile(heaps.map((heap) => heap.peakBytes), 50)),
          afterBytesMedian: Math.round(percentile(heaps.map((heap) => heap.afterBytes), 50)),
        })
      : null,
  })
}

function renderMarkdown(report) {
  const off = report.summary.traceOff
  const on = report.summary.traceOn
  const lines = [
    '# Receive diagnostics performance evidence',
    '',
    'This is paired, non-gating Chromium evidence. It compares the same deterministic virtual large-directory receive with detailed trace disabled and enabled; it is intentionally outside `make web`, `make browser`, and `make ci`.',
    '',
    '## Reproduce',
    '',
    '```powershell',
    `node web/scripts/diagnostics-evidence/run.mjs --entries ${report.profile.entries} --page-size ${report.profile.pageSize} --samples ${report.profile.samplesPerMode} --warmups ${report.profile.warmupsPerMode} --output .codex/orchestration/receive-observability/performance-evidence.md`,
    '```',
    '',
    'The runner alternates pair order, uses the production bounded recorder and trace validators, and exits nonzero unless useful work plus product, lane, and settlement decisions are byte-identical. Trace-off must also construct zero trace payloads and retain zero trace data.',
    '',
    '## Recorded run',
    '',
    `- Profile: ${report.profile.entries.toLocaleString('en-US')} files, page size ${report.profile.pageSize}, ${report.profile.samplesPerMode} measured pairs after ${report.profile.warmupsPerMode} warm-up pair.`,
    `- Recorded at: ${report.recordedAt}.`,
    `- Environment: ${report.environment.platform}/${report.environment.arch}, ${report.environment.cpuCount} × ${report.environment.cpuModel}; Node ${report.environment.node}; Chromium ${report.environment.chromium}.`,
    `- Decision fingerprint: \`${report.decisionFingerprintSha256}\`; all paired product/lane/settlement decisions identical: **yes**.`,
    '',
    '| Mode | Wall median (ms) | Files/s median | Logical MiB/s median | Loop p50 / p95 / max (ms) | Heap before / peak / after median |',
    '|---|---:|---:|---:|---:|---:|',
    summaryRow('Trace off', off),
    summaryRow('Trace on', on),
    '',
    'Logical MiB/s is the aggregate declared file size scheduled by the virtual receive, not disk or network throughput. Heap is reported only when Chromium exposes precise `performance.memory.usedJSHeapSize`; otherwise the runner records `unavailable` rather than substituting process RSS. Peak is sampled at every virtual catalog page boundary.',
    '',
    '### Retention and trace health',
    '',
    '| Pair | Order | Mode | Incidents / bytes | Trace events / bytes | Dropped | Overwritten | Sampled | Coalesced | Payloads built |',
    '|---:|---|---|---:|---:|---:|---:|---:|---:|---:|',
    ...report.pairs.flatMap(pairRows),
    '',
    'The four trace health counters above are exact cumulative recorder values for each isolated sample. They are not merged into an ambiguous loss total. The workload is synthetic by design: it avoids host-file creation while exercising progressive catalog, projection, progress, checkpoint, lane, receive, and settlement evidence in the browser.',
    '',
  ]
  return `${lines.join('\n')}\n`
}

function summaryRow(label, summary) {
  const loop = summary.eventLoopDelayMilliseconds
  const heap = summary.heap === null
    ? 'unavailable'
    : [heapMiB(summary.heap.beforeBytesMedian), heapMiB(summary.heap.peakBytesMedian), heapMiB(summary.heap.afterBytesMedian)].join(' / ')
  return `| ${label} | ${summary.wallMillisecondsMedian} | ${summary.entriesPerSecondMedian} | ${summary.logicalMebibytesPerSecondMedian} | ${loop.p50} / ${loop.p95} / ${loop.max} | ${heap} |`
}

function pairRows(pair) {
  return [
    sampleRow(pair.pair, pair.order.join(' → '), pair.disabled),
    sampleRow(pair.pair, '', pair.enabled),
  ]
}

function sampleRow(pair, order, sample) {
  const trace = sample.trace
  return `| ${pair} | ${order} | ${sample.mode} | ${trace.retainedIncidentCount} / ${trace.retainedIncidentBytes} | ${trace.retainedEventCount} / ${trace.retainedEventBytes} | ${trace.droppedCount} | ${trace.overwrittenCount} | ${trace.sampledCount} | ${trace.coalescedCount} | ${trace.payloadConstructionCount} |`
}

function verifyReport(report) {
  const first = renderMarkdown(report)
  const second = renderMarkdown(JSON.parse(JSON.stringify(report)))
  if (first !== second) throw new Error('report rendering is not deterministic')
}

async function startViteServer() {
  const server = await createServer({
    root: WEB_ROOT,
    configFile: false,
    clearScreen: false,
    logLevel: 'error',
    server: { host: '127.0.0.1', strictPort: true },
  })
  const httpServer = server.httpServer
  if (httpServer === null) throw new Error('Vite did not create an evidence HTTP server')
  await new Promise((resolveListen, rejectListen) => {
    httpServer.once('listening', resolveListen)
    httpServer.once('error', rejectListen)
    void httpServer.listen(0, '127.0.0.1')
  })
  const address = httpServer.address()
  if (address === null || typeof address === 'string') {
    await server.close()
    throw new Error('Vite did not publish an evidence listener')
  }
  return Object.freeze({ server, origin: `http://127.0.0.1:${address.port}` })
}

function parseArguments(arguments_) {
  const values = new Map()
  for (let index = 0; index < arguments_.length; index += 2) {
    const name = arguments_[index]
    const value = arguments_[index + 1]
    if (!name?.startsWith('--') || value === undefined) {
      throw new Error(`expected --name value pair near ${name ?? '<end>'}`)
    }
    if (values.has(name)) throw new Error(`duplicate argument ${name}`)
    values.set(name, value)
  }
  const supported = new Set(['--entries', '--page-size', '--samples', '--warmups', '--output'])
  for (const name of values.keys()) {
    if (!supported.has(name)) throw new Error(`unsupported argument ${name}`)
  }
  return Object.freeze({
    entries: positiveInteger(values.get('--entries'), DEFAULT_ENTRY_COUNT, '--entries'),
    pageSize: positiveInteger(values.get('--page-size'), DEFAULT_PAGE_SIZE, '--page-size'),
    samples: positiveInteger(values.get('--samples'), DEFAULT_SAMPLE_COUNT, '--samples'),
    warmups: nonNegativeInteger(values.get('--warmups'), DEFAULT_WARMUP_COUNT, '--warmups'),
    output: values.has('--output') ? resolve(REPOSITORY_ROOT, values.get('--output')) : undefined,
  })
}

function positiveInteger(value, fallback, name) {
  const parsed = value === undefined ? fallback : Number(value)
  if (!Number.isSafeInteger(parsed) || parsed <= 0) throw new Error(`${name} must be positive`)
  return parsed
}

function nonNegativeInteger(value, fallback, name) {
  const parsed = value === undefined ? fallback : Number(value)
  if (!Number.isSafeInteger(parsed) || parsed < 0) throw new Error(`${name} must be non-negative`)
  return parsed
}

function percentile(values, percentage) {
  if (values.length === 0) throw new Error('percentile requires observations')
  const sorted = [...values].sort((left, right) => left - right)
  const rank = Math.ceil((percentage / PERCENTILE_SCALE) * sorted.length) - 1
  return sorted[Math.max(0, rank)]
}

function round(value) {
  return Number(value.toFixed(DECIMAL_PLACES))
}

function heapMiB(bytes) {
  return `${round(bytes / BYTES_PER_MEBIBYTE)} MiB`
}

function stableJson(value) {
  return JSON.stringify(value)
}

function healthFields() {
  return ['droppedCount', 'overwrittenCount', 'sampledCount', 'coalescedCount']
}

function modeName(enabled) {
  return enabled ? 'trace_on' : 'trace_off'
}

function environmentSnapshot(chromiumVersion) {
  const processors = cpus()
  return Object.freeze({
    platform: process.platform,
    arch: process.arch,
    cpuModel: processors[0]?.model ?? 'unknown',
    cpuCount: processors.length,
    node: process.version,
    chromium: chromiumVersion,
  })
}

function withTimeout(task, milliseconds, label) {
  let timer
  return Promise.race([
    task,
    new Promise((_, reject) => {
      timer = setTimeout(() => reject(new Error(`${label} timed out`)), milliseconds)
    }),
  ]).finally(() => clearTimeout(timer))
}

function milestone(name, context = {}) {
  console.error(JSON.stringify({
    component: RUNNER_COMPONENT,
    operationId: RUN_OPERATION_ID,
    milestone: name,
    ...context,
  }))
}

withTimeout(main(), RUN_TIMEOUT_MS, 'diagnostics evidence').catch((error) => {
  milestone('failed', {
    errorClass: error instanceof Error ? error.name : 'UnknownFailure',
    detail: error instanceof Error ? error.message : String(error),
  })
  process.exitCode = 1
})
