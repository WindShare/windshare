import { createHash } from 'node:crypto'
import { spawn } from 'node:child_process'
import { lstatSync, readFileSync } from 'node:fs'
import { isAbsolute, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const EXECUTABLE_ENV = 'WINDSHARE_PION_SERVER_EXECUTABLE'
const EXECUTABLE_SHA256_ENV = 'WINDSHARE_PION_SERVER_EXECUTABLE_SHA256'
const SHA256_PATTERN = /^[0-9a-f]{64}$/u

export function verifiedPionServerCommand(environment = process.env) {
  const path = environment[EXECUTABLE_ENV]
  const expectedSha256 = environment[EXECUTABLE_SHA256_ENV]
  if (typeof path !== 'string' || !isAbsolute(path) || resolve(path) !== path) {
    throw new Error(`${EXECUTABLE_ENV} must be an absolute canonical path`)
  }
  if (typeof expectedSha256 !== 'string' || !SHA256_PATTERN.test(expectedSha256)) {
    throw new Error(`${EXECUTABLE_SHA256_ENV} must be lowercase 64-hex`)
  }
  const before = lstatSync(path, { bigint: true })
  if (!before.isFile() || before.isSymbolicLink()) {
    throw new Error('Pion server executable must be a regular non-symbolic file')
  }
  const bytes = readFileSync(path)
  const after = lstatSync(path, { bigint: true })
  if (
    before.dev !== after.dev || before.ino !== after.ino || before.size !== after.size ||
    before.mtimeNs !== after.mtimeNs || before.ctimeNs !== after.ctimeNs
  ) throw new Error('Pion server executable changed while authenticated')
  const sha256 = createHash('sha256').update(bytes).digest('hex')
  if (sha256 !== expectedSha256) {
    throw new Error('Pion server executable differs from its runtime manifest')
  }
  return Object.freeze({ executable: path, arguments: Object.freeze([]) })
}

export async function runVerifiedPionServer(environment = process.env) {
  const command = verifiedPionServerCommand(environment)
  const child = spawn(command.executable, command.arguments, {
    env: environment,
    shell: false,
    stdio: 'inherit',
    windowsHide: true,
  })
  const forwardSignal = (signal) => {
    try {
      child.kill(signal)
    } catch {
      // The enclosing process group or Windows Job remains cleanup authority.
    }
  }
  const forwardInterrupt = () => forwardSignal('SIGINT')
  const forwardTermination = () => forwardSignal('SIGTERM')
  process.once('SIGINT', forwardInterrupt)
  process.once('SIGTERM', forwardTermination)
  try {
    return await new Promise((resolveExit, rejectExit) => {
      child.once('error', rejectExit)
      child.once('close', (code, signal) => {
        if (code !== null) resolveExit(code)
        else rejectExit(new Error(`Pion server terminated by ${signal ?? 'unknown signal'}`))
      })
    })
  } finally {
    process.off('SIGINT', forwardInterrupt)
    process.off('SIGTERM', forwardTermination)
  }
}

if (process.argv[1] !== undefined && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  runVerifiedPionServer().then(
    (exitCode) => { process.exitCode = exitCode },
    (cause) => {
      process.stderr.write(`${cause instanceof Error ? cause.message : String(cause)}\n`)
      process.exitCode = 1
    },
  )
}
