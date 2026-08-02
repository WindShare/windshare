import { execFile } from 'node:child_process'
import { lstat } from 'node:fs/promises'
import { isAbsolute, resolve } from 'node:path'
import { promisify } from 'node:util'

const execFileAsync = promisify(execFile)
const PROCESS_OWNER_PACKAGE = './cmd/testprocessowner'
const BUILD_TIMEOUT_MS = 120_000
const MAXIMUM_BUILD_OUTPUT_BYTES = 1_048_576

/**
 * Bootstrap follows the same trust boundary as an ordinary `go build`: the
 * checked-out worktree and selected Go toolchain are inputs, not a second
 * package registry or immutable-source authorization system.
 */
export async function buildTestProcessOwnerFixture({
  repositoryRoot,
  goExecutable,
  outputPath,
}) {
  const root = canonicalAbsolutePath(repositoryRoot, 'process owner fixture repository root')
  const go = canonicalAbsolutePath(goExecutable, 'process owner fixture Go executable')
  const output = canonicalAbsolutePath(outputPath, 'process owner fixture output')
  await requireRegularFile(go, 'process owner fixture Go executable')
  await execFileAsync(
    go,
    [
      'build',
      '-trimpath',
      '-buildvcs=false',
      '-ldflags=-buildid=',
      '-o',
      output,
      PROCESS_OWNER_PACKAGE,
    ],
    {
      cwd: root,
      windowsHide: true,
      timeout: BUILD_TIMEOUT_MS,
      maxBuffer: MAXIMUM_BUILD_OUTPUT_BYTES,
    },
  )
  await requireRegularFile(output, 'built process owner fixture')
  return Object.freeze({ path: output })
}

async function requireRegularFile(path, label) {
  const metadata = await lstat(path)
  if (!metadata.isFile() || metadata.isSymbolicLink() || metadata.size < 1) {
    throw new Error(`${label} is not a non-empty regular file`)
  }
}

function canonicalAbsolutePath(value, label) {
  if (typeof value !== 'string' || !isAbsolute(value) || resolve(value) !== value) {
    throw new Error(`${label} must be absolute and canonical`)
  }
  return value
}
