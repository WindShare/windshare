import { execFile } from 'node:child_process'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'

import prepareDirectProductBinaries from './direct-product-setup'

const WORKER_BUILD_TIMEOUT_MILLISECONDS = 180_000

export default async function prepareNetworkProducts(): Promise<() => Promise<void>> {
  const cleanup = await prepareDirectProductBinaries()
  try {
    await promisify(execFile)('worker-build', ['--release', '--locked'], {
      cwd: resolve(dirname(fileURLToPath(import.meta.url)), '../../../relay-rust/crates/relay-cloudflare'),
      timeout: WORKER_BUILD_TIMEOUT_MILLISECONDS, windowsHide: true, maxBuffer: 1_000_000,
    })
    return cleanup
  } catch (error) {
    await cleanup()
    throw error
  }
}
