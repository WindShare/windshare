import { execFile } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'
import { expect, type Browser, type Page, type TestInfo } from '@playwright/test'

import { DirectProcess } from '../fixtures/direct-process'
import { startProductFrontend } from '../fixtures/product-frontend'
import { ISOLATION_BLOCK_BYTES, ISOLATION_FILE_BYTES, runRelayIsolationScenario } from '../fixtures/relay-isolation-scenario'
import type { RelayTarget } from './isolated-deployment'

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')
const BUILD_TIMEOUT_MILLISECONDS = 120_000
const CLOUD_TIMEOUT_MILLISECONDS = 180_000
const DOWNLOAD_TIMEOUT_MILLISECONDS = 600_000
const PROCESS_STOP_TIMEOUT_MILLISECONDS = 5_000

export async function runCloudflareIsolationScenario(
  browser: Browser, page: Page, testInfo: TestInfo, target: RelayTarget,
): Promise<void> {
  const output = testInfo.outputPath('run')
  await mkdir(output, { recursive: true })
  const executable = join(output, process.platform === 'win32' ? 'wind.exe' : 'wind')
  await promisify(execFile)('go', ['build', '-o', executable, './cmd/wind'], {
    cwd: REPOSITORY_ROOT, env: { ...process.env, GOWORK: 'off', GOTOOLCHAIN: 'local' },
    timeout: BUILD_TIMEOUT_MILLISECONDS, windowsHide: true,
  })
  const payload = randomBytes(ISOLATION_FILE_BYTES)
  const source = join(output, 'isolation.bin')
  const senderTrace = join(output, 'sender.ndjson')
  await writeFile(source, payload)
  const { vite, url } = await startProductFrontend(output)
  let sender: DirectProcess | undefined
  try {
    sender = new DirectProcess(executable, [
      'share', source, '--front-url', url, '--trace', senderTrace, '--block-size', String(ISOLATION_BLOCK_BYTES),
    ], {
      cwd: REPOSITORY_ROOT,
      environment: { ...process.env, WINDSHARE_CONFIG: target.configPath },
      operationId: 'cloudflare-receiver-isolation',
      disclosure: { stdout: 'capability', stderr: 'private' },
    })
    const ready = await sender.waitFor('stdout', /^Link: (.+)$/mu, { timeoutMilliseconds: CLOUD_TIMEOUT_MILLISECONDS })
    const capability = ready[1]!
    sender.consumeReadiness()
    expect(new URL(capability).searchParams.getAll('r')).toEqual([target.url])
    await runRelayIsolationScenario(browser, page, testInfo, {
      url: capability, payload, senderTrace: () => readFile(senderTrace, 'utf8'),
      timeoutMilliseconds: DOWNLOAD_TIMEOUT_MILLISECONDS,
    })
  } finally {
    await page.close()
    await sender?.stop(PROCESS_STOP_TIMEOUT_MILLISECONDS)
    await vite.close()
  }
}
