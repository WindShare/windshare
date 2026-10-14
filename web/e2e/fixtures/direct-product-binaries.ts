import { execFile } from 'node:child_process'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'

const execFileAsync = promisify(execFile)
const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')
const BUILD_OUTPUT_LIMIT_BYTES = 1_000_000
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
    relay: join(directory, `wsrelay${suffix}`),
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
  // One Go invocation shares compilation work between both commands. The suite
  // owns these binaries; scenario startup never downloads or compiles packages.
  try {
    await execFileAsync('go', [
      'build', '-o', binaries.directory, './cmd/wind', './relay/cmd/wsrelay',
    ], {
      cwd: REPOSITORY_ROOT,
      env: { ...process.env, GOTOOLCHAIN: 'local', GOWORK: 'off' },
      timeout: DIRECT_PRODUCT_BUILD_TIMEOUT_MILLISECONDS,
      windowsHide: true,
      maxBuffer: BUILD_OUTPUT_LIMIT_BYTES,
    })
  } catch (error) {
    throw new Error(
      `Direct product Go build failed (build budget ${DIRECT_PRODUCT_BUILD_TIMEOUT_MILLISECONDS} ms)`,
      { cause: error },
    )
  }
}
