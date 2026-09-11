import { mkdir, mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises'
import { dirname, isAbsolute, join, relative, resolve, sep } from 'node:path'
import { fileURLToPath } from 'node:url'
import { createHash, randomUUID } from 'node:crypto'
import { platform, release, tmpdir } from 'node:os'
import { chromium } from '@playwright/test'
import { createServer } from 'vite'
import { COPY_FAILURE_BYTES, SCHEMA, STAGED_FILE_BYTES, workloads } from './workload.mjs'
import { SAMPLE_INTERVAL_MILLISECONDS, sampleHostTree, summarizeHostSamples } from './host-sampler.mjs'
import { summarizeEvents, timingComparison } from './observations.mjs'

const REFERENCE_HARNESS = '/scripts/browser-evidence/folder-recovery/browser-harness.mjs'
const HARNESS_PAGE = '/scripts/browser-evidence/folder-recovery/index.html'
const MAX_PROFILE_BYTES = 1024 * 1024 * 1024
const MAX_CASE_MILLISECONDS = 60_000
const webRoot = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')

function argumentsFor(argv) {
  const result = { output: null, candidate: REFERENCE_HARNESS, baseline: REFERENCE_HARNESS, repetitions: 3, nativeTarget: null, nativeReady: null, browserExecutable: null }
  for (let index = 0; index < argv.length; index += 1) {
    const key = argv[index]
    if (key === '--output') result.output = resolve(argv[++index])
    else if (key === '--candidate') result.candidate = argv[++index]
    else if (key === '--baseline') result.baseline = argv[++index]
    else if (key === '--repetitions') result.repetitions = Number(argv[++index])
    else if (key === '--native-target') result.nativeTarget = resolve(argv[++index])
    else if (key === '--native-ready') result.nativeReady = resolve(argv[++index])
    else if (key === '--browser-executable') result.browserExecutable = resolve(argv[++index])
    else throw new Error(`Unknown argument: ${key}`)
  }
  if (result.output === null) throw new Error('--output is required')
  if (result.nativeTarget !== null && (result.nativeReady === null || result.browserExecutable === null)) throw new Error('Native evidence requires a ready file and explicit browser executable')
  for (const module of [result.candidate, result.baseline]) {
    if (!/^\/(?:scripts|test)\/[a-zA-Z0-9_./-]+\.(?:mjs|ts)$/u.test(module) || module.includes('..')) throw new Error('Harness must be a local evidence module')
  }
  if (!Number.isSafeInteger(result.repetitions) || result.repetitions < 1 || result.repetitions > 5) throw new Error('Repetitions must be 1–5')
  return result
}

async function removeOwnedProfile(workRoot) {
  const parent = resolve(tmpdir())
  const child = relative(parent, resolve(workRoot))
  if (isAbsolute(child) || child.startsWith(`..${sep}`) || child.includes(sep) || !child.startsWith('windshare-folder-evidence-')) throw new Error('Refusing unowned evidence cleanup')
  await rm(workRoot, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
}

const options = argumentsFor(process.argv.slice(2))
const runId = randomUUID()
const workRoot = await mkdtemp(join(tmpdir(), 'windshare-folder-evidence-'))
const profile = join(workRoot, 'profile')
await mkdir(profile)
const report = {
  schema: SCHEMA, runId, capturedAt: new Date().toISOString(),
  implementation: options.candidate === REFERENCE_HARNESS ? 'browser-api-reference-only' : 'candidate-harness',
  host: { platform: platform(), release: release(), node: process.version },
  destination: { kind: options.nativeTarget ? 'native-fsa-directory' : 'opfs-directory-surrogate', nativeTargetEvidence: options.nativeTarget !== null, path: options.nativeTarget },
  policy: { stagedFileBudgetBytes: STAGED_FILE_BYTES, failureAfterCopyBytes: COPY_FAILURE_BYTES },
  cases: [], events: [], hostSamples: [],
}
let browser
let server
let timer
let sampling
let sampleFailure
const capture = () => {
  if (sampling) return sampling
  sampling = (async () => {
    const sample = await sampleHostTree(profile)
    if (options.nativeTarget) {
      const targetSample = await sampleHostTree(options.nativeTarget)
      sample.profileLogicalFileBytes = sample.logicalFileBytes
      sample.profileAllocatedFileBytes = sample.allocatedFileBytes
      sample.targetLogicalFileBytes = targetSample.logicalFileBytes
      sample.targetAllocatedFileBytes = targetSample.allocatedFileBytes
      sample.logicalFileBytes += targetSample.logicalFileBytes
      sample.allocatedFileBytes = sample.allocatedFileBytes === null || targetSample.allocatedFileBytes === null
        ? null : sample.allocatedFileBytes + targetSample.allocatedFileBytes
      sample.inaccessible.push(...targetSample.inaccessible)
    }
    report.hostSamples.push(sample)
    if (sample.logicalFileBytes > MAX_PROFILE_BYTES) throw new Error('Bounded evidence profile exceeded 1 GiB')
  })().catch(error => { sampleFailure = error }).finally(() => { sampling = undefined })
  return sampling
}

try {
  const sourceFiles = new Set([options.candidate.slice(1), options.baseline.slice(1),
    'test/browser/fsa-namespace-atomicity-harness.ts', 'scripts/browser-evidence/fsa-small-file/content.mjs'])
  for (const directory of ['src/output', 'src/transfer', 'scripts/browser-evidence/folder-recovery']) {
    for (const entry of await readdir(join(webRoot, directory), { withFileTypes: true, recursive: true })) {
      if (entry.isFile() && /\.(?:ts|mjs|html|ps1)$/u.test(entry.name)) {
        sourceFiles.add(relative(webRoot, join(entry.parentPath, entry.name)).split(sep).join('/'))
      }
    }
  }
  report.sourceFileSha256 = {}
  for (const path of [...sourceFiles].sort()) {
    report.sourceFileSha256[path] = createHash('sha256').update(await readFile(join(webRoot, path))).digest('hex')
  }
  server = await createServer({ root: webRoot, configFile: false, logLevel: 'error', server: { host: '127.0.0.1', port: 0, strictPort: true } })
  await server.listen()
  const address = server.httpServer.address()
  if (typeof address !== 'object' || address === null) throw new Error('Vite address is unavailable')
  const origin = `http://127.0.0.1:${address.port}`
  browser = await chromium.launchPersistentContext(profile, { headless: options.nativeTarget === null, executablePath: options.browserExecutable ?? undefined })
  const page = browser.pages()[0] ?? await browser.newPage()
  page.setDefaultTimeout(MAX_CASE_MILLISECONDS)
  report.host.browser = browser.browser()?.version() ?? 'persistent Chromium'
  report.host.browserExecutable = options.browserExecutable ?? chromium.executablePath()
  report.host.browserExecutableSha256 = createHash('sha256').update(await readFile(report.host.browserExecutable)).digest('hex')
  await page.exposeFunction('__folderEvidenceEvent', async event => {
    if (sampleFailure) throw sampleFailure
    report.events.push(event)
    if (event.kind === 'inventory') await capture()
  })
  await page.goto(origin + HARNESS_PAGE)
  report.host.userAgent = await page.evaluate(() => navigator.userAgent)
  if (options.nativeTarget) {
    await page.evaluate(async () => {
      const module = await import('/scripts/browser-evidence/folder-recovery/native-target.mjs')
      module.installNativePicker()
    })
    await writeFile(options.nativeReady, JSON.stringify({ origin, profile, target: options.nativeTarget }), { flag: 'wx' })
    await page.getByRole('button', { name: 'Choose isolated evidence target', exact: true }).click()
    report.destination.authority = await page.evaluate(() => globalThis.__folderNativeReady)
  }
  report.storage = await page.evaluate(async () => ({
    estimate: await navigator.storage.estimate(), persisted: await navigator.storage.persisted(),
    directoryAvailable: typeof navigator.storage.getDirectory === 'function',
  }))
  await capture()
  timer = setInterval(() => { void capture() }, SAMPLE_INTERVAL_MILLISECONDS)
  const fixture = workloads()
  async function run(module, action, input) {
    const startEvent = report.events.length
    const startSample = report.hostSamples.length
    const result = await page.evaluate(async ({ module, action, input, timeout }) => {
      const harness = await import(module)
      let deadline
      try {
        return await Promise.race([
          harness[action](input),
          new Promise((_, reject) => { deadline = setTimeout(() => reject(new Error('Evidence case timed out')), timeout) }),
        ])
      } finally { clearTimeout(deadline) }
    }, { module, action, input, timeout: MAX_CASE_MILLISECONDS })
    await capture()
    if (sampleFailure) throw sampleFailure
    const events = report.events.slice(startEvent)
    const samples = report.hostSamples.slice(startSample)
    const entry = {
      caseId: input.caseId, action, module, input, result,
      observations: summarizeEvents(events), host: summarizeHostSamples(samples),
    }
    report.cases.push(entry)
    return entry
  }
  const mixed = await run(options.candidate, 'runCase', { caseId: `folder-evidence-mixed-${runId}`, files: fixture.mixed, mode: 'automatic' })
  if (mixed.result.status !== 'saved') throw new Error('Mixed case did not save every file')
  const stagedFiles = fixture.mixed.filter(file => file.placement === 'staged')
  const stagedBytes = stagedFiles.reduce((sum, file) => sum + file.sizeBytes, 0)
  if (mixed.observations.cumulativeLocalCopyBytes !== stagedBytes || mixed.observations.stageRemovals !== stagedFiles.length) {
    throw new Error('Mixed case did not copy and reclaim each staged file exactly once')
  }
  await run(options.candidate, 'cleanupCase', { caseId: mixed.caseId })
  const retryId = `folder-evidence-retry-${runId}`
  const failed = await run(options.candidate, 'runCase', {
    caseId: retryId, files: fixture.mixed.slice(0, 2), mode: 'automatic', failAfterBytes: COPY_FAILURE_BYTES,
  })
  if (failed.result.status !== 'copy-failed' || failed.observations.cumulativeLocalCopyBytes !== COPY_FAILURE_BYTES || failed.observations.stageRemovals !== 0) {
    throw new Error('Fault did not preserve a retryable stage after the exact injected write cut')
  }
  await page.reload()
  // Browser modules are application assets; preload them before removing network access.
  await page.evaluate(async module => { await import(module) }, options.candidate)
  await browser.setOffline(true)
  const resumed = await run(options.candidate, 'resumeCase', { caseId: retryId })
  if (resumed.result.status !== 'saved' || resumed.observations.receivedSourceBytes !== 0 ||
      resumed.observations.cumulativeLocalCopyBytes !== STAGED_FILE_BYTES || resumed.observations.stageRemovals !== 1) {
    throw new Error('Offline retry failed to save and reclaim the retained stage without source data')
  }
  await browser.setOffline(false)
  await run(options.candidate, 'cleanupCase', { caseId: retryId })
  const timings = { baseline: [], candidate: [] }
  for (let repetition = 0; repetition <= options.repetitions; repetition += 1) {
    for (const mode of ['baseline', 'candidate']) {
      const module = mode === 'baseline' ? options.baseline : options.candidate
      const entry = await run(module, 'runCase', {
        caseId: `folder-evidence-${mode}-${repetition}-${runId}`, files: fixture.small,
        mode: mode === 'baseline' ? 'direct' : 'automatic', sample: false,
      })
      if (entry.result.status !== 'saved') throw new Error('Small-file case failed')
      if (repetition > 0) timings[mode].push(entry.result.milliseconds)
      await run(module, 'cleanupCase', { caseId: entry.caseId })
    }
  }
  report.smallFileTiming = { ...timingComparison(timings.baseline, timings.candidate), timings, productClaim: options.candidate !== REFERENCE_HARNESS }
  report.status = 'completed'
} catch (error) {
  report.status = 'failed'
  report.error = { name: error.name, message: error.message, stack: error.stack }
  process.exitCode = 1
} finally {
  clearInterval(timer)
  await sampling
  await browser?.close().catch(error => { report.browserCleanupError = error.message })
  await capture()
  report.hostSummary = summarizeHostSamples(report.hostSamples)
  if (options.nativeTarget) report.hostSummary.scope = 'sum of independently scanned isolated browser profile and isolated external FSA target; per-root counts retained in each sample'
  await server?.close()
  await removeOwnedProfile(workRoot)
  report.profileRemoved = true
  await mkdir(dirname(options.output), { recursive: true })
  await writeFile(options.output, `${JSON.stringify(report, null, 2)}\n`, { flag: 'wx' })
  console.log(JSON.stringify({ status: report.status, output: options.output, cases: report.cases.length, host: report.hostSummary, error: report.error }))
}
