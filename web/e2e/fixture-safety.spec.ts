import { execFile } from 'node:child_process'
import { readFile, readdir, mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, extname, join, resolve } from 'node:path'
import process from 'node:process'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'

import { ManagedProcess, settleCleanupTasks } from './fixtures/process'
import { disableCapabilityArtifacts, expect, test } from './fixtures/test'

const DIAGNOSTIC_TIMEOUT_MS = 200
const DUMMY_CAPABILITY = 'capability-must-not-appear'
const DUMMY_SEPARATE_KEY = 'AURVTU1ZLUM1LUFSVElGQUM'
const DUMMY_DECODED_SECRET = 'DUMMY-C5-ARTIFAC'
const ARTIFACT_PROBE_TIMEOUT_MS = 30_000
const MAX_PROBE_OUTPUT_BYTES = 1_000_000
const WEB_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const PLAYWRIGHT_CLI = resolve(WEB_ROOT, 'node_modules/@playwright/test/cli.js')
const ARTIFACT_PROBE_CONFIG = resolve(
  WEB_ROOT,
  'e2e/fixtures/separate-key-artifact.config.ts',
)
const execFileAsync = promisify(execFile)

disableCapabilityArtifacts()

test('fixture failures redact capabilities and surface orphaned cleanup', async () => {
  const child = new ManagedProcess(
    process.execPath,
    [
      '-e',
      `process.stdout.write('Link: http://127.0.0.1/share#${DUMMY_CAPABILITY}\\nKey: ${DUMMY_CAPABILITY}\\n'); setInterval(() => undefined, 1_000)`,
    ],
    { redactDiagnostics: true },
  )
  let readinessFailure: unknown
  try {
    await child.waitFor('stdout', /output-that-will-never-exist/u, DIAGNOSTIC_TIMEOUT_MS)
  } catch (error) {
    readinessFailure = error
  } finally {
    await child.stop()
  }
  expect(readinessFailure).toBeInstanceOf(Error)
  expect(String(readinessFailure)).toContain('<redacted capability stdout;')
  expect(String(readinessFailure)).not.toContain(DUMMY_CAPABILITY)

  let successfulCleanupRan = false
  const cleanupFailure = new Error('simulated orphan cleanup failure')
  await expect(
    settleCleanupTasks([
      Promise.resolve().then(() => { successfulCleanupRan = true }),
      Promise.reject(cleanupFailure),
    ], 'Regression fixture'),
  ).rejects.toBe(cleanupFailure)
  expect(successfulCleanupRan).toBe(true)
})

test('rejects a clean child exit before readiness', async () => {
  const child = new ManagedProcess(process.execPath, ['-e', 'process.exit(0)'])

  await expect(child.waitFor('stdout', /^READY$/mu)).rejects.toThrow(
    'E2E child exited before /^READY$/mu appeared in stdout (code=0, signal=null)',
  )
  await expect(child.stop()).rejects.toThrow(
    'E2E child exited before cleanup (code=0, signal=null)',
  )
})

test('rejects a clean child exit after readiness but before cleanup', async () => {
  const child = new ManagedProcess(process.execPath, [
    '-e',
    "process.stdout.write('READY\\n', () => process.exit(0))",
  ])

  await child.waitFor('stdout', /^READY$/mu)
  await child.waitForExit()
  await expect(child.stop()).rejects.toThrow(
    'E2E child exited before cleanup (code=0, signal=null)',
  )
})

async function artifactFiles(directory: string): Promise<string[]> {
  const entries = await readdir(directory, { withFileTypes: true })
  const nested = await Promise.all(entries.map(async (entry) => {
    const path = join(directory, entry.name)
    return entry.isDirectory() ? artifactFiles(path) : [path]
  }))
  return nested.flat()
}

test('failed separate-key submissions leave every generated artifact capability-free', async ({
  baseUrl,
}) => {
  const outputDirectory = await mkdtemp(join(tmpdir(), 'windshare-c5-artifacts-'))
  try {
    let probeFailure: unknown
    try {
      await execFileAsync(
        process.execPath,
        [PLAYWRIGHT_CLI, 'test', '--config', ARTIFACT_PROBE_CONFIG],
        {
          cwd: WEB_ROOT,
          env: {
            ...process.env,
            WINDSHARE_ARTIFACT_PROBE_BASE_URL: baseUrl,
            WINDSHARE_ARTIFACT_PROBE_KEY: DUMMY_SEPARATE_KEY,
            WINDSHARE_ARTIFACT_PROBE_OUTPUT: outputDirectory,
          },
          timeout: ARTIFACT_PROBE_TIMEOUT_MS,
          windowsHide: true,
          maxBuffer: MAX_PROBE_OUTPUT_BYTES,
        },
      )
    } catch (error) {
      probeFailure = error
    }

    expect(probeFailure).toBeDefined()
    const failure = probeFailure as { readonly stdout?: string; readonly stderr?: string }
    const diagnostics = `${failure.stdout ?? ''}\n${failure.stderr ?? ''}`
    for (const forbidden of [DUMMY_SEPARATE_KEY, DUMMY_DECODED_SECRET]) {
      expect(diagnostics).not.toContain(forbidden)
    }

    const files = await artifactFiles(outputDirectory)
    expect(files.filter((path) => path.endsWith('error-context.md'))).toHaveLength(3)
    expect(files.some((path) => extname(path) === '.png')).toBe(true)
    expect(files.some((path) => extname(path) === '.webm')).toBe(true)
    for (const path of files) {
      const text = (await readFile(path)).toString('latin1')
      for (const forbidden of [DUMMY_SEPARATE_KEY, DUMMY_DECODED_SECRET]) {
        expect(text).not.toContain(forbidden)
      }
    }
  } finally {
    await rm(outputDirectory, { recursive: true, force: true })
  }
})
