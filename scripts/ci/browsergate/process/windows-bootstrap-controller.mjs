import { spawn } from 'node:child_process'
import { writeSync } from 'node:fs'
import { isAbsolute, resolve } from 'node:path'

const REQUEST_MAXIMUM_BYTES = 4 * 1024 * 1024
const STATUS_DESCRIPTOR = 3

const chunks = []
let requestBytes = 0
let rejected = false

process.stdin.on('data', (chunk) => {
  if (rejected) return
  requestBytes += chunk.byteLength
  if (requestBytes > REQUEST_MAXIMUM_BYTES) {
    rejected = true
    process.stderr.write('Windows bootstrap request exceeds its byte limit\n')
    process.exitCode = 2
    process.stdin.destroy()
    return
  }
  chunks.push(Buffer.from(chunk))
})

process.stdin.once('end', () => {
  if (rejected) return
  try {
    startTarget(parseRequest(Buffer.concat(chunks).toString('utf8')))
  } catch (cause) {
    process.stderr.write(`${errorMessage(cause)}\n`)
    process.exitCode = 2
  }
})

function startTarget(request) {
  const child = spawn(request.executable, request.arguments, {
    cwd: request.cwd,
    env: request.environment,
    shell: false,
    stdio: ['ignore', 'pipe', 'pipe'],
    windowsHide: true,
  })
  child.stdout.pipe(process.stdout)
  child.stderr.pipe(process.stderr)
  let settled = false
  const settle = (status) => {
    if (settled) return
    settled = true
    writeSync(STATUS_DESCRIPTOR, `${JSON.stringify(status)}\n`)
    // The controller deliberately remains alive after the target terminates.
    // Its parent can therefore retire one still-addressable /T process root and
    // prove that no compiler descendant escaped the bootstrap boundary.
    setInterval(() => undefined, 60_000)
  }
  child.once('error', (cause) => settle({
    schemaVersion: 1,
    terminal: 'spawn-failed',
    errorCode: cause.code ?? 'UNKNOWN',
    errorMessage: boundedText(cause.message),
  }))
  child.once('close', (code, signal) => settle(code === null
    ? { schemaVersion: 1, terminal: 'signaled', signal: signal ?? 'UNKNOWN' }
    : { schemaVersion: 1, terminal: 'exited', exitCode: code }))
}

function parseRequest(encoded) {
  let value
  try {
    value = JSON.parse(encoded)
  } catch (cause) {
    throw new Error('Windows bootstrap request is invalid JSON', { cause })
  }
  if (!isRecord(value) || JSON.stringify(value) !== encoded) {
    throw new Error('Windows bootstrap request must be canonical JSON')
  }
  exactKeys(
    value,
    ['schemaVersion', 'executable', 'arguments', 'cwd', 'environment'],
    'Windows bootstrap request',
  )
  if (value.schemaVersion !== 1) throw new Error('Windows bootstrap request schema is unsupported')
  requireCanonicalAbsolutePath(value.executable, 'Windows bootstrap executable')
  requireCanonicalAbsolutePath(value.cwd, 'Windows bootstrap working directory')
  if (!Array.isArray(value.arguments) || value.arguments.some((argument) => typeof argument !== 'string')) {
    throw new Error('Windows bootstrap arguments must be strings')
  }
  if (!isRecord(value.environment)) throw new Error('Windows bootstrap environment must be an object')
  for (const [name, environmentValue] of Object.entries(value.environment)) {
    if (
      name === '' || name.includes('=') || name.includes('\0') ||
      typeof environmentValue !== 'string' || environmentValue.includes('\0')
    ) {
      throw new Error('Windows bootstrap environment contains an invalid entry')
    }
  }
  return value
}

function exactKeys(value, keys, label) {
  const actual = Object.keys(value)
  if (actual.length !== keys.length || keys.some((key) => !Object.hasOwn(value, key))) {
    throw new Error(`${label} does not have exact keys`)
  }
}

function requireCanonicalAbsolutePath(value, label) {
  if (typeof value !== 'string' || !isAbsolute(value) || resolve(value) !== value) {
    throw new Error(`${label} must be absolute and canonical`)
  }
}

function isRecord(value) {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function boundedText(value) {
  return value.length <= 512 ? value : value.slice(0, 512)
}

function errorMessage(cause) {
  return cause instanceof Error ? cause.message : String(cause)
}
