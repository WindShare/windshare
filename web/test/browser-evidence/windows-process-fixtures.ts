import { execFile } from 'node:child_process'
import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'

import { afterAll } from 'vitest'

import { createWindowsJobContainmentBackend } from '../../scripts/browser-evidence/process/windows-job-backend.ts'
import {
  runSyntheticSample,
  type SyntheticSampleOptions,
} from './framework-fixtures.ts'

const FRAMEWORK_REPOSITORY_ROOT = fileURLToPath(new URL('../../../', import.meta.url))
const execFilePromise = promisify(execFile)
let windowsJobHelperWorkspace: string | undefined
let windowsJobHelperPromise: Promise<string> | undefined

afterAll(async () => {
  if (windowsJobHelperWorkspace !== undefined) {
    await rm(windowsJobHelperWorkspace, { recursive: true, force: true })
  }
})

// This separate fixture module is imported only by the process integration
// suite, so contract tests cannot gain native process authority from host OS.
export async function runNativeWindowsSyntheticSample(
  options: Omit<SyntheticSampleOptions, 'containmentBackend'>,
): Promise<Awaited<ReturnType<typeof runSyntheticSample>>> {
  const helperPath = await loadFrameworkWindowsJobHelper()
  return runSyntheticSample({
    ...options,
    containmentBackend: createWindowsJobContainmentBackend(helperPath),
  })
}

async function loadFrameworkWindowsJobHelper(): Promise<string> {
  if (process.platform !== 'win32') {
    throw new Error('the native Windows process fixture is available only on Windows')
  }
  windowsJobHelperPromise ??= buildFrameworkWindowsJobHelper()
  return windowsJobHelperPromise
}

async function buildFrameworkWindowsJobHelper(): Promise<string> {
  windowsJobHelperWorkspace = await mkdtemp(join(tmpdir(), 'windshare-windows-job-helper-'))
  const helperPath = join(windowsJobHelperWorkspace, 'windowsjob.exe')
  await execFilePromise(
    'go',
    [
      'build',
      '-o',
      helperPath,
      './web/scripts/browser-evidence/windowsjob',
    ],
    {
      cwd: FRAMEWORK_REPOSITORY_ROOT,
      windowsHide: true,
    },
  )
  return helperPath
}
