#!/usr/bin/env node

import { spawn } from 'node:child_process'
import {
  lstat,
  mkdir,
  open,
  realpath,
} from 'node:fs/promises'
import { dirname, isAbsolute, join, resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

export const HELPER_BUILD_MANIFEST_SCHEMA_VERSION =
  'windshare.browser-network-matrix.helper-build/v2'

const GO_BUILD_DEADLINE_MS = 300_000
const GO_BUILD_REAP_DEADLINE_MS = 5_000
const GO_BUILD_MAXIMUM_OUTPUT_BYTES = 1 << 20
const HELPER_MAXIMUM_BYTES = 128 << 20
const SCRIPT_DIRECTORY = dirname(fileURLToPath(import.meta.url))
const REPOSITORY_ROOT = resolve(SCRIPT_DIRECTORY, '..', '..', '..', '..')

export class HelperBuildError extends Error {
  constructor(message, { cause, operation, outputDirectory, outputOwned }) {
    super(message, { cause })
    this.name = 'HelperBuildError'
    this.operation = operation
    this.outputDirectory = outputDirectory
    this.outputOwned = outputOwned
  }
}

export function helperBuildPlan(outputDirectory, platform = process.platform, architecture = process.arch) {
  const goArchitecture = requireGoArchitecture(architecture)
  if (platform !== 'win32' && platform !== 'linux') {
    throw new Error(`browser network matrix helper builds are unsupported on ${platform}`)
  }
  const executableSuffix = platform === 'win32' ? '.exe' : ''
  const operations = [{
    operation: 'artifact-publisher',
    role: 'artifact-publisher',
    cwd: join(REPOSITORY_ROOT, 'core'),
    packagePath: './osfs/cmd/browsermatrixpublish',
    outputPath: join(outputDirectory, `browsermatrixpublish${executableSuffix}`),
  }, {
    operation: 'test-process-owner',
    role: 'test-process-owner',
    cwd: REPOSITORY_ROOT,
    packagePath: './cmd/testprocessowner',
    outputPath: join(outputDirectory, `testprocessowner${executableSuffix}`),
  }]
  return Object.freeze(operations.map((operation) => Object.freeze({
    ...operation,
    platform,
    goArchitecture,
    deadlineMs: GO_BUILD_DEADLINE_MS,
    maximumOutputBytes: GO_BUILD_MAXIMUM_OUTPUT_BYTES,
    arguments: Object.freeze([
      'build',
      '-trimpath',
      '-buildvcs=false',
      '-ldflags=-buildid=',
      '-o',
      operation.outputPath,
      operation.packagePath,
    ]),
  })))
}

export async function buildNetworkMatrixHelpers(
  outputDirectory,
  {
    platform = process.platform,
    architecture = process.arch,
    runOperation = runGoBuildOperation,
    onProgress = () => undefined,
  } = {},
) {
  let canonicalOutput
  let outputOwned = false
  let operation = 'validate-output-directory'
  try {
    canonicalOutput = await requireNewCanonicalOutputDirectory(outputDirectory, process.platform)
    await mkdir(canonicalOutput, { recursive: false, mode: 0o700 })
    outputOwned = true
    const plan = helperBuildPlan(canonicalOutput, platform, architecture)
    const helpers = []
    for (const build of plan) {
      operation = build.operation
      onProgress(Object.freeze({ operation, outcome: 'started', outputPath: build.outputPath }))
      await runOperation(build)
      const helper = await validateBuiltHelper(build)
      helpers.push(helper)
      onProgress(Object.freeze({
        operation,
        outcome: 'completed',
        outputPath: helper.path,
      }))
    }

    operation = 'write-helper-manifest'
    const manifest = Object.freeze({
      schemaVersion: HELPER_BUILD_MANIFEST_SCHEMA_VERSION,
      platform,
      architecture: requireGoArchitecture(architecture),
      helpers: Object.freeze(helpers),
    })
    const manifestPath = join(canonicalOutput, 'helper-manifest.json')
    const handle = await open(manifestPath, 'wx', 0o600)
    try {
      await handle.writeFile(`${JSON.stringify(manifest)}\n`, 'utf8')
      await handle.sync()
    } finally {
      await handle.close()
    }
    return Object.freeze({ outputDirectory: canonicalOutput, manifestPath, manifest })
  } catch (cause) {
    const retained = outputOwned
      ? `; partial owned output is retained at ${canonicalOutput}`
      : ''
    throw new HelperBuildError(`helper build ${operation} failed: ${errorMessage(cause)}${retained}`, {
      cause,
      operation,
      outputDirectory: canonicalOutput ?? outputDirectory,
      outputOwned,
    })
  }
}

export async function runBuildHelpersCli(
  arguments_,
  {
    stdout = process.stdout,
    stderr = process.stderr,
    build = buildNetworkMatrixHelpers,
  } = {},
) {
  const operands = arguments_[0] === '--' ? arguments_.slice(1) : arguments_
  if (operands.length !== 1) {
    stderr.write('usage: pnpm build:browser-network-matrix-helpers -- ABSOLUTE_NEW_OUTPUT_DIR\n')
    return 2
  }
  const outputOperand = decodePackageManagerPathOperand(operands[0])
  try {
    const result = await build(outputOperand, {
      onProgress: (event) => stderr.write(`${JSON.stringify({
        component: 'browser-network-matrix-helper-build',
        ...event,
      })}\n`),
    })
    stdout.write(`${JSON.stringify({
      outcome: 'completed',
      outputDirectory: result.outputDirectory,
      manifestPath: result.manifestPath,
    })}\n`)
    return 0
  } catch (cause) {
    const failure = cause instanceof HelperBuildError ? cause : undefined
    stderr.write(`${JSON.stringify({
      outcome: 'failed',
      operation: failure?.operation ?? 'bootstrap',
      outputDirectory: failure?.outputDirectory ?? outputOperand,
      partialOutputRetained: failure?.outputOwned ?? false,
      error: errorMessage(cause),
    })}\n`)
    return 1
  }
}

function decodePackageManagerPathOperand(value) {
  if (process.platform !== 'win32') return value
  // pnpm's Windows script shim can pass a drive path with every separator
  // doubled. Decode only that complete transport shape before canonical-path
  // validation; mixed or UNC spellings remain invalid rather than being guessed.
  const pieces = value.split('\\\\')
  if (
    pieces.length < 2 || !/^[A-Za-z]:$/u.test(pieces[0]) ||
    pieces.slice(1).some((piece) => piece.length === 0 || piece.includes('\\'))
  ) return value
  return pieces.join('\\')
}

async function requireNewCanonicalOutputDirectory(pathValue, platform) {
  const canonicalPath = typeof pathValue === 'string' ? resolve(pathValue) : ''
  const platformLexicalPath = platform === 'win32' && typeof pathValue === 'string'
    ? pathValue.replaceAll('/', '\\')
    : pathValue
  if (
    typeof pathValue !== 'string' || pathValue.length === 0 || pathValue.includes('\0') ||
    !isAbsolute(pathValue) || canonicalPath !== platformLexicalPath
  ) throw new Error('helper output directory must be an absolute canonical path')
  const parent = dirname(canonicalPath)
  const parentMetadata = await lstat(parent)
  if (!parentMetadata.isDirectory() || parentMetadata.isSymbolicLink()) {
    throw new Error('helper output parent must be a real directory')
  }
  const resolvedParent = await realpath(parent)
  if (!samePlatformPath(resolvedParent, parent, platform)) {
    throw new Error('helper output parent must not traverse a symbolic-link alias')
  }
  try {
    await lstat(canonicalPath)
  } catch (cause) {
    if (isErrno(cause, 'ENOENT')) return canonicalPath
    throw cause
  }
  throw new Error('helper output directory must not already exist')
}

async function runGoBuildOperation(operation) {
  const environment = {
    ...process.env,
    CGO_ENABLED: '0',
    GOARCH: operation.goArchitecture,
    GOENV: 'off',
    GOEXPERIMENT: '',
    GOFLAGS: '',
    GOOS: operation.platform === 'win32' ? 'windows' : 'linux',
    GOTOOLCHAIN: 'local',
    GOWORK: join(REPOSITORY_ROOT, 'go.work'),
    ...(operation.goArchitecture === 'amd64' ? { GOAMD64: 'v1' } : { GOARM64: 'v8.0' }),
  }
  const goExecutable = process.env.WINDSHARE_GO_EXECUTABLE ?? 'go'
  const child = spawn(goExecutable, operation.arguments, {
    cwd: operation.cwd,
    env: environment,
    shell: false,
    windowsHide: true,
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  const stdout = child.stdout
  const stderr = child.stderr
  if (stdout === null || stderr === null) {
    child.kill('SIGKILL')
    throw new Error(`${operation.operation} did not expose bounded build output pipes`)
  }
  const chunks = []
  let outputBytes = 0
  let overflow = false
  const append = (chunk) => {
    const bytes = Buffer.from(chunk)
    const remaining = operation.maximumOutputBytes - outputBytes
    if (remaining > 0) chunks.push(bytes.subarray(0, remaining))
    outputBytes += bytes.byteLength
    if (outputBytes > operation.maximumOutputBytes) {
      overflow = true
      child.kill('SIGKILL')
    }
  }
  stdout.on('data', append)
  stderr.on('data', append)

  let timeout
  let reapTimeout
  let deadlineFailure
  try {
    const terminal = await new Promise((resolveTerminal, rejectTerminal) => {
      child.once('error', rejectTerminal)
      child.once('close', (code, signal) => resolveTerminal({ code, signal }))
      timeout = setTimeout(() => {
        deadlineFailure = new Error(`${operation.operation} exceeded its ${operation.deadlineMs}ms deadline`)
        child.kill('SIGKILL')
        reapTimeout = setTimeout(() => {
          rejectTerminal(new Error(`${operation.operation} did not reap after forced termination`))
        }, GO_BUILD_REAP_DEADLINE_MS)
        reapTimeout.unref()
      }, operation.deadlineMs)
      timeout.unref()
    })
    if (deadlineFailure !== undefined) throw deadlineFailure
    if (overflow) throw new Error(`${operation.operation} exceeded its build-output byte authority`)
    if (terminal.code !== 0 || terminal.signal !== null) {
      const detail = Buffer.concat(chunks).toString('utf8').trim()
      throw new Error(`${operation.operation} failed with code ${terminal.code ?? 'null'}${
        detail.length === 0 ? '' : `: ${detail}`
      }`)
    }
  } finally {
    if (timeout !== undefined) clearTimeout(timeout)
    if (reapTimeout !== undefined) clearTimeout(reapTimeout)
    stdout.off('data', append)
    stderr.off('data', append)
  }
}

async function validateBuiltHelper(operation) {
  const namedBefore = await lstat(operation.outputPath, { bigint: true })
  if (!namedBefore.isFile() || namedBefore.isSymbolicLink() || namedBefore.size < 1n ||
      namedBefore.size > BigInt(HELPER_MAXIMUM_BYTES) ||
      (operation.platform === 'linux' && (namedBefore.mode & 0o111n) === 0n)) {
    throw new Error(`${operation.operation} did not produce a bounded regular helper`)
  }
  const handle = await open(operation.outputPath, 'r')
  try {
    const openedBefore = await handle.stat({ bigint: true })
    if (!sameFileRevision(namedBefore, openedBefore)) {
      throw new Error(`${operation.operation} output changed while it was inspected`)
    }
    const namedAfter = await lstat(operation.outputPath, { bigint: true })
    if (
      !sameFileRevision(namedBefore, openedBefore) ||
      !sameFileRevision(namedBefore, namedAfter)
    ) throw new Error(`${operation.operation} output changed while it was inspected`)
    return Object.freeze({
      role: operation.role,
      path: operation.outputPath,
    })
  } finally {
    await handle.close()
  }
}

function sameFileRevision(left, right) {
  return left.dev === right.dev && left.ino === right.ino && left.size === right.size &&
    left.mtimeNs === right.mtimeNs && left.ctimeNs === right.ctimeNs
}

function requireGoArchitecture(value) {
  if (value === 'x64' || value === 'amd64') return 'amd64'
  if (value === 'arm64') return 'arm64'
  throw new Error(`browser network matrix helper builds are unsupported on ${value}`)
}

function samePlatformPath(left, right, platform) {
  return platform === 'win32'
    ? left.toLowerCase() === right.toLowerCase()
    : left === right
}

function errorMessage(cause) {
  return cause instanceof Error ? cause.message : String(cause)
}

function isErrno(cause, code) {
  return cause instanceof Error && 'code' in cause && cause.code === code
}

const invokedPath = process.argv[1] === undefined ? undefined : pathToFileURL(resolve(process.argv[1])).href
if (invokedPath === import.meta.url) {
  process.exitCode = await runBuildHelpersCli(process.argv.slice(2))
}
