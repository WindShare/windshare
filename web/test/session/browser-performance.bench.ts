import { arch, cpus, platform, release, totalmem } from 'node:os'
import { performance } from 'node:perf_hooks'
import { writeFile } from 'node:fs/promises'
import { expect, test, type CDPSession, type Page } from '@playwright/test'
import type {
  BrowserPerformanceResult,
} from './performance-browser'

const KIB = 1024
const MIB = 1024 * KIB
const MAX_FRAME_BYTES = 65_536
const LOW_WATER_BYTES = 256 * KIB
const HIGH_WATER_BYTES = MIB
const FIXTURE_BYTES = 64 * MIB
const WARMUP_RUNS = 1
const SAMPLE_RUNS = 5
const HARNESS_PATH = '/test/session/performance-browser.ts'
const CHUNK_CASES = [
  { chunkBytes: KIB, framesPerChunk: 1 },
  { chunkBytes: 64 * KIB, framesPerChunk: 2 },
  { chunkBytes: MIB, framesPerChunk: 17 },
  { chunkBytes: 4 * MIB, framesPerChunk: 65 },
] as const

interface CDPMetric {
  readonly name: string
  readonly value: number
}

interface PerformanceSnapshot {
  readonly taskDurationSeconds: number
  readonly jsHeapUsedBytes: number
}

interface SampleEvidence {
  readonly result: BrowserPerformanceResult
  readonly sampleWallMs: number
  readonly taskDurationSeconds: number
  readonly cdpHeapBeforeBytes: number
  readonly cdpHeapAfterBytes: number
  readonly mainThreadUtilization: number
}

test('Chromium production WebRTC flow-control performance baseline', async ({
  browser,
  page,
}, testInfo) => {
  await page.goto('/')
  const cdp = await page.context().newCDPSession(page)
  await cdp.send('Performance.enable')
  const cases: Array<{
    chunkBytes: number
    summary: ReturnType<typeof summarize>
    warmups: readonly BrowserPerformanceResult[]
    samples: readonly SampleEvidence[]
  }> = []

  for (const benchmarkCase of CHUNK_CASES) {
    const warmups: BrowserPerformanceResult[] = []
    for (let warmup = 0; warmup < WARMUP_RUNS; warmup += 1) {
      const result = await runSample(page, benchmarkCase.chunkBytes)
      assertBoundedExactResult(result, benchmarkCase.framesPerChunk)
      warmups.push(result)
    }

    const samples: SampleEvidence[] = []
    for (let sample = 0; sample < SAMPLE_RUNS; sample += 1) {
      await cdp.send('HeapProfiler.collectGarbage')
      const before = await performanceSnapshot(cdp)
      const sampleStarted = performance.now()
      const result = await runSample(page, benchmarkCase.chunkBytes)
      const sampleWallMs = performance.now() - sampleStarted
      const after = await performanceSnapshot(cdp)
      assertBoundedExactResult(result, benchmarkCase.framesPerChunk)
      const taskDurationSeconds =
        after.taskDurationSeconds - before.taskDurationSeconds
      samples.push({
        result,
        sampleWallMs,
        taskDurationSeconds,
        cdpHeapBeforeBytes: before.jsHeapUsedBytes,
        cdpHeapAfterBytes: after.jsHeapUsedBytes,
        mainThreadUtilization:
          taskDurationSeconds / (sampleWallMs / 1_000),
      })
    }
    cases.push({
      chunkBytes: benchmarkCase.chunkBytes,
      summary: summarize(samples),
      warmups,
      samples,
    })
  }

  const hardware = cpus()
  const evidence = {
    recordedAt: new Date().toISOString(),
    browserVersion: browser.version(),
    nodeVersion: process.version,
    platform: {
      name: platform(),
      release: release(),
      arch: arch(),
      cpu: hardware[0]?.model ?? 'unknown',
      logicalCPUs: hardware.length,
      totalMemoryBytes: totalmem(),
    },
    fixtureBytes: FIXTURE_BYTES,
    warmupRuns: WARMUP_RUNS,
    sampleRuns: SAMPLE_RUNS,
    cases,
  }
  const evidencePath = testInfo.outputPath('d5-browser-evidence.json')
  await writeFile(evidencePath, JSON.stringify(evidence, undefined, 2), 'utf8')
  await testInfo.attach('d5-browser-evidence', {
    path: evidencePath,
    contentType: 'application/json',
  })
  console.log(`D5_BROWSER_ENV=${JSON.stringify({
    recordedAt: evidence.recordedAt,
    browserVersion: evidence.browserVersion,
    nodeVersion: evidence.nodeVersion,
    platform: evidence.platform,
    fixtureBytes: evidence.fixtureBytes,
    warmupRuns: evidence.warmupRuns,
    sampleRuns: evidence.sampleRuns,
  })}`)
  for (const benchmarkCase of cases) {
    console.log(`D5_BROWSER_CASE=${JSON.stringify({
      chunkBytes: benchmarkCase.chunkBytes,
      summary: benchmarkCase.summary,
    })}`)
  }
})

async function runSample(
  page: Page,
  chunkBytes: number,
): Promise<BrowserPerformanceResult> {
  return page.evaluate(
    async ({ harnessPath, bytes, fixtureBytes }) => {
      const harness = await import(harnessPath) as {
        runBrowserPerformance(
          chunkBytes: number,
          fixtureBytes: number,
        ): Promise<BrowserPerformanceResult>
      }
      return harness.runBrowserPerformance(bytes, fixtureBytes)
    },
    {
      harnessPath: HARNESS_PATH,
      bytes: chunkBytes,
      fixtureBytes: FIXTURE_BYTES,
    },
  )
}

function assertBoundedExactResult(
  result: BrowserPerformanceResult,
  framesPerChunk: number,
): void {
  expect(result.fixtureBytes).toBe(FIXTURE_BYTES)
  expect(result.framesPerChunk).toBe(framesPerChunk)
  expect(result.receivedWireBytes).toBe(result.wireBytes)
  expect(result.frames).toBe(result.chunks * result.framesPerChunk)
  expect(result.maximumMessageBytes).toBeGreaterThanOrEqual(MAX_FRAME_BYTES)
  expect(result.lowWaterBytes).toBe(LOW_WATER_BYTES)
  expect(result.highWaterBytes).toBe(HIGH_WATER_BYTES)
  expect(result.peakBufferedBytes).toBeLessThan(
    HIGH_WATER_BYTES + MAX_FRAME_BYTES,
  )
  expect(result.highWaterObserved).toBe(true)
  expect(result.lowWaterEvents).toBeGreaterThan(0)
  expect(result.elapsedMs).toBeGreaterThan(0)
  expect(result.throughputMiBps).toBeGreaterThan(0)
  expect(result.selectedCandidatePair.state).toBe('succeeded')
  expect(result.selectedCandidatePair.nominated).toBe(true)
  expect(result.selectedCandidatePair.local.address.length).toBeGreaterThan(0)
  expect(result.selectedCandidatePair.remote.address.length).toBeGreaterThan(0)
  expect(result.selectedCandidatePair.local.protocol.length).toBeGreaterThan(0)
  expect(result.selectedCandidatePair.remote.protocol.length).toBeGreaterThan(0)
}

async function performanceSnapshot(
  cdp: CDPSession,
): Promise<PerformanceSnapshot> {
  const response = await cdp.send('Performance.getMetrics')
  return {
    taskDurationSeconds: metric(response.metrics, 'TaskDuration'),
    jsHeapUsedBytes: metric(response.metrics, 'JSHeapUsedSize'),
  }
}

function metric(metrics: readonly CDPMetric[], name: string): number {
  const found = metrics.find((candidate) => candidate.name === name)
  if (found === undefined) {
    throw new Error(`CDP Performance metric ${name} is unavailable`)
  }
  return found.value
}

function summarize(samples: readonly SampleEvidence[]) {
  return {
    throughputMiBps: distribution(
      samples.map((sample) => sample.result.throughputMiBps),
    ),
    transferElapsedMs: distribution(
      samples.map((sample) => sample.result.elapsedMs),
    ),
    sampleWallMs: distribution(
      samples.map((sample) => sample.sampleWallMs),
    ),
    taskDurationSeconds: distribution(
      samples.map((sample) => sample.taskDurationSeconds),
    ),
    mainThreadUtilization: distribution(
      samples.map((sample) => sample.mainThreadUtilization),
    ),
    activeHeapDeltaBytes: distribution(
      samples.map(
        (sample) =>
          sample.result.heapAfterBytes - sample.result.heapBeforeBytes,
      ),
    ),
    cdpHeapDeltaBytes: distribution(
      samples.map(
        (sample) =>
          sample.cdpHeapAfterBytes - sample.cdpHeapBeforeBytes,
      ),
    ),
    maximumPeakBufferedBytes: Math.max(
      ...samples.map((sample) => sample.result.peakBufferedBytes),
    ),
    minimumLowWaterEvents: Math.min(
      ...samples.map((sample) => sample.result.lowWaterEvents),
    ),
  }
}

function distribution(values: readonly number[]) {
  const mean = values.reduce((total, value) => total + value, 0) / values.length
  const variance = values.length < 2
    ? 0
    : values.reduce(
      (total, value) => total + (value - mean) ** 2,
      0,
    ) / (values.length - 1)
  const standardDeviation = Math.sqrt(variance)
  return {
    mean,
    standardDeviation,
    coefficientOfVariation: mean === 0 ? 0 : standardDeviation / mean,
    minimum: Math.min(...values),
    maximum: Math.max(...values),
  }
}
