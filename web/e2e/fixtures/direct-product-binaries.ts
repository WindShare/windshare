import { execFile } from 'node:child_process'
import { copyFile } from 'node:fs/promises'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'

const execFileAsync = promisify(execFile)
const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')
const BUILD_OUTPUT_LIMIT_BYTES = 8 * 1024 * 1024
export const DIRECT_PRODUCT_BUILD_TIMEOUT_MILLISECONDS = 180_000
export const DIRECT_PRODUCT_BINARY_DIRECTORY_ENV = 'WINDSHARE_BROWSER_BINARY_DIRECTORY'

export interface DirectBinaryPaths {
  readonly directory: string
  readonly relay: string
  readonly wind: string
}

export function directBinaryPaths(
  directory: string,
  platform: NodeJS.Platform = process.platform,
): DirectBinaryPaths {
  if (resolve(directory) !== directory) {
    throw new TypeError('Direct product binary directory path must be absolute and canonical')
  }
  const suffix = platform === 'win32' ? '.exe' : ''
  return Object.freeze({
    directory,
    relay: join(directory, `relay-native${suffix}`),
    wind: join(directory, `wind${suffix}`),
  })
}

export function preparedDirectBinaryPaths(): DirectBinaryPaths {
  const directory = process.env[DIRECT_PRODUCT_BINARY_DIRECTORY_ENV]
  if (directory === undefined || directory.length === 0) {
    throw new Error('Direct product binaries are not prepared; run a product Playwright configuration')
  }
  return directBinaryPaths(directory)
}

export async function buildDirectProductBinaries(binaries: DirectBinaryPaths): Promise<void> {
  // Both compilers must finish before cleanup may remove suite-owned binaries.
  // Cargo keeps its incremental cache; scenarios receive an independent executable.
  const results = await Promise.allSettled([
    build('go', ['build', '-o', binaries.wind, './cmd/wind'], REPOSITORY_ROOT),
    buildNativeRelay(binaries.relay),
  ])
  const failures = results.flatMap(result => result.status === 'rejected' ? [result.reason] : [])
  if (failures.length !== 0) throw new AggregateError(failures, 'Direct product binary preparation failed')
}

async function buildNativeRelay(destination: string): Promise<void> {
  const output = await build('rustup', [
    'run', 'stable', 'cargo', 'build', '--locked', '-p', 'windshare-relay-native',
    '--bin', 'relay-native', '--message-format=json-render-diagnostics',
  ], join(REPOSITORY_ROOT, 'relay-rust'))
  for (const line of output.split('\n').filter(line => line.trim().length !== 0)) {
    const artifact = JSON.parse(line) as { reason?: string; target?: { name?: string }; executable?: string | null }
    if (artifact.reason === 'compiler-artifact' && artifact.target?.name === 'relay-native' && artifact.executable) {
      await copyFile(artifact.executable, destination)
      return
    }
  }
  throw new Error('Cargo did not produce the native relay executable')
}

async function build(command: string, args: string[], cwd: string): Promise<string> {
  try {
    const result = await execFileAsync(command, args, {
      cwd,
      env: { ...process.env, GOTOOLCHAIN: 'local', GOWORK: 'off' },
      timeout: DIRECT_PRODUCT_BUILD_TIMEOUT_MILLISECONDS,
      windowsHide: true,
      maxBuffer: BUILD_OUTPUT_LIMIT_BYTES,
    })
    return result.stdout
  } catch (error) {
    throw new Error(
      `Direct product ${command} build failed (build budget ${DIRECT_PRODUCT_BUILD_TIMEOUT_MILLISECONDS} ms)`,
      { cause: error },
    )
  }
}
