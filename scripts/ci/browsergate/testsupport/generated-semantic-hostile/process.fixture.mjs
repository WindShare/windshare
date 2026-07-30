import { spawn } from 'node:child_process'
import { copyFile, mkdir, readFile } from 'node:fs/promises'
import { isAbsolute, join, resolve } from 'node:path'
import { pathToFileURL } from 'node:url'

const FIXTURE_ROOT = import.meta.dirname
const HOSTILE_PROJECT_FILES = Object.freeze([
  ['vite-config.fixture.mjs', 'vite.config.mjs'],
  ['production-env.fixture', '.env.production'],
  ['tsconfig.fixture.json', 'tsconfig.json'],
])
const KILL_SETTLEMENT_DEADLINE_MILLISECONDS = 10_000
const WINDOWS_TREE_KILL_DEADLINE_MILLISECONDS = 5_000
const PRELOAD_RECORD_SCHEMA = 'windshare.generated-semantic-hostile-preload/v1'

export const HOSTILE_PRELOAD_PATH = join(FIXTURE_ROOT, 'preload.fixture.mjs')

export async function materializeHostileGeneratedSemanticProject(destination) {
  requireCanonicalAbsolutePath(destination, 'hostile generated semantic project root')
  await mkdir(destination, { recursive: false })
  await Promise.all(HOSTILE_PROJECT_FILES.map(([fixtureName, discoveredName]) =>
    copyFile(join(FIXTURE_ROOT, fixtureName), join(destination, discoveredName))))
  return destination
}

export function createHostileNodeOptions(recordPath) {
  requireCanonicalAbsolutePath(recordPath, 'hostile preload record path')
  const preload = new URL(pathToFileURL(HOSTILE_PRELOAD_PATH))
  preload.searchParams.set('recordPath', recordPath)
  // Encoding the destination into the import URL proves that NODE_OPTIONS itself
  // did not cross the worker boundary; the recorder needs no second environment variable.
  return `--import=${preload.href}`
}

export async function readExactlyOneHostilePreloadRecord(path) {
  const encoded = decodeUtf8(
    await readFile(path),
    'hostile generated semantic preload record',
  )
  if (
    encoded.length < 3 || !encoded.endsWith('\n') ||
    encoded.indexOf('\n') !== encoded.length - 1 || encoded.includes('\r')
  ) throw new Error('hostile preload must emit exactly one LF-terminated record')

  const line = encoded.slice(0, -1)
  let record
  try {
    record = JSON.parse(line)
  } catch (cause) {
    throw new Error('hostile preload record is malformed JSON', { cause })
  }
  if (JSON.stringify(record) !== line) throw new Error('hostile preload record is not canonical JSON')
  requireExactKeys(
    record,
    ['schemaVersion', 'pid', 'parentPid', 'entryPoint', 'workingDirectory'],
    'hostile preload record',
  )
  if (record.schemaVersion !== PRELOAD_RECORD_SCHEMA) {
    throw new Error('hostile preload record schema is unsupported')
  }
  if (!Number.isSafeInteger(record.pid) || record.pid < 1) {
    throw new Error('hostile preload process ID is invalid')
  }
  if (!Number.isSafeInteger(record.parentPid) || record.parentPid < 0) {
    throw new Error('hostile preload parent process ID is invalid')
  }
  requireCanonicalAbsolutePath(record.entryPoint, 'hostile preload entry point')
  requireCanonicalAbsolutePath(record.workingDirectory, 'hostile preload working directory')
  return Object.freeze(record)
}

export async function runStrictBoundedProcess({
  executable,
  arguments: arguments_,
  workingDirectory,
  environment,
  deadlineMilliseconds,
  maximumStdoutBytes,
  maximumStderrBytes,
  label,
}) {
  if (typeof executable !== 'string' || executable.length === 0) {
    throw new TypeError(`${label} executable is required`)
  }
  if (!Array.isArray(arguments_) || arguments_.some((argument) => typeof argument !== 'string')) {
    throw new TypeError(`${label} arguments must be strings`)
  }
  requireCanonicalAbsolutePath(workingDirectory, `${label} working directory`)
  if (environment === null || typeof environment !== 'object' || Array.isArray(environment)) {
    throw new TypeError(`${label} environment must be an object`)
  }
  for (const [name, value] of [
    ['deadline', deadlineMilliseconds],
    ['stdout limit', maximumStdoutBytes],
    ['stderr limit', maximumStderrBytes],
  ]) {
    if (!Number.isSafeInteger(value) || value < 1) throw new TypeError(`${label} ${name} is invalid`)
  }

  let child
  try {
    child = spawn(executable, arguments_, {
      cwd: workingDirectory,
      env: environment,
      detached: process.platform !== 'win32',
      shell: false,
      windowsHide: true,
      stdio: ['ignore', 'pipe', 'pipe'],
    })
  } catch (cause) {
    throw new Error(`${label} failed to start`, { cause })
  }

  const stdout = boundedBytes(maximumStdoutBytes, `${label} stdout`)
  const stderr = boundedBytes(maximumStderrBytes, `${label} stderr`)
  let terminalFailure = null
  let terminationPromise = null
  let killSettlementTimer = null

  const terminal = await new Promise((resolvePromise) => {
    const deadlineTimer = setTimeout(() => {
      failAndTerminate(new Error(`${label} exceeded its ${deadlineMilliseconds}ms deadline`))
    }, deadlineMilliseconds)

    child.stdout.on('data', (chunk) => collect(stdout, chunk))
    child.stderr.on('data', (chunk) => collect(stderr, chunk))
    child.stdout.once('error', (cause) => failAndTerminate(
      new Error(`${label} stdout collection failed`, { cause }),
    ))
    child.stderr.once('error', (cause) => failAndTerminate(
      new Error(`${label} stderr collection failed`, { cause }),
    ))
    child.once('error', (cause) => failAndTerminate(
      new Error(`${label} process failed`, { cause }),
    ))
    child.once('close', (exitCode, signal) => settle({ exitCode, signal, unsettled: false }))

    function collect(collector, chunk) {
      if (terminalFailure !== null) return
      try {
        collector.write(chunk)
      } catch (cause) {
        failAndTerminate(cause)
      }
    }

    function failAndTerminate(failure) {
      terminalFailure ??= failure
      if (terminationPromise !== null) return
      terminationPromise = terminateProcessTree(child)
      killSettlementTimer = setTimeout(
        () => settle({ exitCode: null, signal: null, unsettled: true }),
        KILL_SETTLEMENT_DEADLINE_MILLISECONDS,
      )
    }

    function settle(outcome) {
      clearTimeout(deadlineTimer)
      if (killSettlementTimer !== null) clearTimeout(killSettlementTimer)
      resolvePromise(outcome)
    }
  })

  if (terminationPromise !== null) await terminationPromise
  if (terminal.unsettled) {
    throw new Error(`${label} did not settle after process-tree termination`, {
      cause: terminalFailure ?? undefined,
    })
  }
  if (terminalFailure !== null) throw terminalFailure
  const stdoutBytes = stdout.bytes()
  const stdoutText = decodeUtf8(stdoutBytes, `${label} stdout`)
  const stderrText = stderr.text()
  if (terminal.signal !== null) throw new Error(`${label} ended from signal ${terminal.signal}`)
  if (stderrText !== '') throw new Error(`${label} emitted unexpected stderr: ${diagnostic(stderrText)}`)
  if (terminal.exitCode !== 0) {
    throw new Error(`${label} exited with code ${terminal.exitCode}: ${diagnostic(stdoutText)}`)
  }
  return Object.freeze({ pid: child.pid, stdout: stdoutText, stdoutBytes })
}

function boundedBytes(maximumBytes, label) {
  const chunks = []
  let byteLength = 0
  return Object.freeze({
    write(chunk) {
      const bytes = Buffer.from(chunk)
      byteLength += bytes.byteLength
      if (byteLength > maximumBytes) throw new Error(`${label} exceeded its byte limit`)
      chunks.push(bytes)
    },
    text() {
      return decodeUtf8(Buffer.concat(chunks), label)
    },
    bytes() {
      return Buffer.concat(chunks)
    },
  })
}

async function terminateProcessTree(child) {
  const pid = child.pid
  if (!Number.isSafeInteger(pid) || pid < 1) return
  if (process.platform === 'win32') {
    await terminateWindowsProcessTree(pid)
  } else {
    try {
      process.kill(-pid, 'SIGKILL')
    } catch (cause) {
      if (cause?.code !== 'ESRCH') child.kill('SIGKILL')
    }
  }
  if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL')
}

async function terminateWindowsProcessTree(pid) {
  const systemRoot = windowsSystemRoot()
  if (systemRoot === null) return
  let killer
  try {
    killer = spawn(join(systemRoot, 'System32', 'taskkill.exe'), [
      '/PID',
      String(pid),
      '/T',
      '/F',
    ], {
      shell: false,
      windowsHide: true,
      stdio: 'ignore',
    })
  } catch {
    return
  }
  await new Promise((resolvePromise) => {
    let settled = false
    const timer = setTimeout(() => {
      killer.kill('SIGKILL')
      settle()
    }, WINDOWS_TREE_KILL_DEADLINE_MILLISECONDS)
    killer.once('error', settle)
    killer.once('close', settle)

    function settle() {
      if (settled) return
      settled = true
      clearTimeout(timer)
      resolvePromise()
    }
  })
}

function windowsSystemRoot() {
  for (const name of ['SystemRoot', 'SYSTEMROOT', 'WINDIR']) {
    const value = process.env[name]
    if (typeof value === 'string' && isAbsolute(value)) return resolve(value)
  }
  return null
}

function decodeUtf8(bytes, label) {
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch (cause) {
    throw new Error(`${label} is not valid UTF-8`, { cause })
  }
}

function diagnostic(value) {
  const maximumCharacters = 512
  return JSON.stringify(value.slice(0, maximumCharacters))
}

function requireExactKeys(value, expected, label) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new TypeError(`${label} must be an object`)
  }
  const actual = Object.keys(value)
  if (actual.length !== expected.length || expected.some((key) => !Object.hasOwn(value, key))) {
    throw new Error(`${label} does not have exact keys`)
  }
}

function requireCanonicalAbsolutePath(value, label) {
  if (typeof value !== 'string' || !isAbsolute(value) || resolve(value) !== value) {
    throw new TypeError(`${label} must be absolute and canonical`)
  }
}
