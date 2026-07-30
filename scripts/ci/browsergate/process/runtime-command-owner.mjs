import { createHash } from 'node:crypto'
import { lstatSync, readFileSync, realpathSync } from 'node:fs'
import { isAbsolute, delimiter, resolve } from 'node:path'

import { executeWindowsJob } from '../../../../web/scripts/browser-evidence/process/windows-job-client.ts'
import { executeLinuxProcessOwner } from '../../../../web/scripts/browser-evidence/process/linux-process-owner-client.ts'

const MAXIMUM_CAPTURE_BYTES = 16 * 1024 * 1024
const SHA256_PATTERN = /^[0-9a-f]{64}$/u

export async function executeOwnedRuntimeCommand({
  operationId,
  command,
  platform = process.platform,
  inheritedEnvironment,
  deadlineMs,
  terminationGraceMs,
  terminationSignal,
  windowsJobHelper,
  linuxProcessOwner,
  trace = () => undefined,
}) {
  requireOperationId(operationId)
  requireCommand(command)
  requirePositiveInteger(deadlineMs, 'runtime command deadline')
  requirePositiveInteger(terminationGraceMs, 'runtime command termination grace')
  if (terminationSignal !== undefined && !(terminationSignal instanceof AbortSignal)) {
    throw new Error('runtime command termination signal is invalid')
  }
  const environment = canonicalEnvironment(inheritedEnvironment)
  const stdout = boundedCollector('runtime command stdout')
  const stderr = boundedCollector('runtime command stderr')
  emitTrace(trace, { milestone: 'runtime-command-started', context: { operationId, platform } })
  let execution
  if (platform === 'win32') {
    if (windowsJobHelper === undefined) {
      throw new Error(
        'Windows runtime command requires a pre-authenticated Job helper; taskkill bootstrap is not settlement authority',
      )
    } else {
      assertRuntimeArtifactLive(windowsJobHelper, 'Windows Job helper')
      const owned = await executeWindowsJob({
        helperPath: windowsJobHelper.path,
        operationId,
        command: {
          executable: command.executable,
          ...(command.executableSha256 === undefined
            ? {}
            : { executableSha256: command.executableSha256 }),
          arguments: command.arguments,
          cwd: command.cwd,
          ...(command.stdin === undefined
            ? {}
            : { stdin: command.stdin, stdinAuthority: command.stdinAuthority }),
        },
        inheritedEnvironment: environment,
        injectedEnvironment: Object.freeze({}),
        deadlineMs,
        terminationGraceMs,
        ...(terminationSignal === undefined ? {} : { terminationSignal }),
        stdout: stdout.write,
        stderr: stderr.write,
      })
      assertRuntimeArtifactLive(windowsJobHelper, 'Windows Job helper')
      execution = owned
    }
  } else if (platform === 'linux') {
    if (linuxProcessOwner === undefined) {
      throw new Error(
        'Linux runtime command requires a pre-authenticated subreaper helper; process-group bootstrap is not settlement authority',
      )
    } else {
      assertRuntimeArtifactLive(linuxProcessOwner, 'Linux process owner helper')
      execution = await executeLinuxProcessOwner({
        helper: linuxProcessOwner,
        operationId,
        command,
        environment,
        deadlineMs,
        terminationGraceMs,
        ...(terminationSignal === undefined ? {} : { terminationSignal }),
        stdout: stdout.write,
        stderr: stderr.write,
        trace,
      })
      assertRuntimeArtifactLive(linuxProcessOwner, 'Linux process owner helper')
    }
  } else if (platform === 'darwin') {
    throw new Error('Darwin runtime process ownership is unsupported without a descendant authority')
  } else if (platform !== 'win32') {
    throw new Error(`unsupported runtime command platform ${JSON.stringify(platform)}`)
  }
  const result = Object.freeze({
    processEvidence: execution.processEvidence,
    timedOut: execution.timedOut,
    launched: execution.launched,
    treeEmpty: execution.treeEmpty,
    ...(execution.inputEvidence === undefined
      ? {}
      : { inputEvidence: execution.inputEvidence }),
    ...(execution.clientIoEvidence === undefined
      ? {}
      : { clientIoEvidence: execution.clientIoEvidence }),
    ...(execution.ownershipEvidence === undefined
      ? {}
      : { ownershipEvidence: execution.ownershipEvidence }),
    stdout: stdout.text(),
    stderr: stderr.text(),
  })
  emitTrace(trace, {
    milestone: 'runtime-command-tree-empty',
    context: { operationId, timedOut: result.timedOut, terminal: result.processEvidence.terminal },
  })
  return result
}

export function resolveHostExecutable(name, {
  platform = process.platform,
  environment = process.env,
} = {}) {
  if (typeof name !== 'string' || name === '' || /[\\/]/u.test(name)) {
    throw new Error('host executable name must be one path segment')
  }
  const pathValue = environmentEntry(environment, 'PATH')
  if (pathValue === undefined || pathValue === '') {
    throw new Error(`cannot resolve ${name}: PATH is unavailable`)
  }
  const names = platform === 'win32' && !name.toLowerCase().endsWith('.exe')
    ? [`${name}.exe`]
    : [name]
  for (const rawDirectory of pathValue.split(delimiter)) {
    const directory = rawDirectory.trim().replace(/^"|"$/gu, '')
    if (directory === '') continue
    for (const candidateName of names) {
      const candidate = resolve(directory, candidateName)
      try {
        const canonical = realpathSync(candidate)
        const metadata = lstatSync(canonical)
        if (metadata.isFile() && !metadata.isSymbolicLink()) return canonical
      } catch {
        // PATH entries are claims, not authority. Only a resolved regular file wins.
      }
    }
  }
  throw new Error(`cannot resolve host executable ${name}`)
}

function boundedCollector(label, maximumBytes = MAXIMUM_CAPTURE_BYTES) {
  const chunks = []
  let byteLength = 0
  return Object.freeze({
    write(chunk) {
      byteLength += chunk.byteLength
      if (byteLength > maximumBytes) throw new Error(`${label} exceeds its byte limit`)
      chunks.push(Buffer.from(chunk))
    },
    text() {
      const bytes = Buffer.concat(chunks)
      try {
        return new TextDecoder('utf-8', { fatal: true }).decode(bytes)
      } catch (cause) {
        throw new Error(`${label} is not valid UTF-8`, { cause })
      }
    },
  })
}

function assertRuntimeArtifactLive(artifact, label) {
  if (!isRecord(artifact) || typeof artifact.path !== 'string' || !SHA256_PATTERN.test(artifact.sha256)) {
    throw new Error(`${label} authority is invalid`)
  }
  const metadataBefore = lstatSync(artifact.path, { bigint: true })
  if (!metadataBefore.isFile() || metadataBefore.isSymbolicLink()) {
    throw new Error(`${label} is not a regular file`)
  }
  const bytes = readFileSync(artifact.path)
  const metadataAfter = lstatSync(artifact.path, { bigint: true })
  if (
    metadataBefore.dev !== metadataAfter.dev || metadataBefore.ino !== metadataAfter.ino ||
    metadataBefore.size !== metadataAfter.size || metadataBefore.mtimeNs !== metadataAfter.mtimeNs ||
    createHash('sha256').update(bytes).digest('hex') !== artifact.sha256
  ) throw new Error(`${label} changed while used`)
}

function canonicalEnvironment(value) {
  if (!isRecord(value)) throw new Error('runtime command inherited environment is required')
  const environment = {}
  for (const [name, entry] of Object.entries(value)) {
    if (entry === undefined) continue
    if (
      name === '' || name.includes('=') || name.includes('\0') ||
      typeof entry !== 'string' || entry.includes('\0')
    ) {
      throw new Error('runtime command environment contains an invalid entry')
    }
    environment[name] = entry
  }
  return Object.freeze(environment)
}

function requireCommand(command) {
  if (!isRecord(command)) throw new Error('runtime command is required')
  if (!isAbsolute(command.executable) || resolve(command.executable) !== command.executable) {
    throw new Error('runtime command executable must be absolute and canonical')
  }
  if (!isAbsolute(command.cwd) || resolve(command.cwd) !== command.cwd) {
    throw new Error('runtime command working directory must be absolute and canonical')
  }
  if (!Array.isArray(command.arguments) || command.arguments.some((value) => typeof value !== 'string')) {
    throw new Error('runtime command arguments must be strings')
  }
  if (
    command.executableSha256 !== undefined &&
    !SHA256_PATTERN.test(command.executableSha256)
  ) throw new Error('runtime command executable digest is invalid')
  if ((command.executableSha256 === undefined) !== (command.executableByteLength === undefined)) {
    throw new Error('runtime command executable digest and byte length must appear together')
  }
  if (
    command.executableByteLength !== undefined &&
    (!Number.isSafeInteger(command.executableByteLength) || command.executableByteLength < 1)
  ) throw new Error('runtime command executable byte length is invalid')
  if (
    command.stdin !== undefined &&
    (!(command.stdin instanceof Uint8Array) || command.stdin.byteLength < 1 ||
      command.stdin.byteLength > 1_048_576)
  ) throw new Error('runtime command stdin is invalid')
  if ((command.stdin === undefined) !== (command.stdinAuthority === undefined)) {
    throw new Error('runtime command stdin bytes and authority must appear together')
  }
}

function requireOperationId(value) {
  if (typeof value !== 'string' || !/^[A-Za-z0-9._-]{1,256}$/u.test(value)) {
    throw new Error('runtime operation ID is invalid')
  }
}

function requirePositiveInteger(value, label) {
  if (!Number.isSafeInteger(value) || value < 1) throw new Error(`${label} must be a positive integer`)
}

function environmentEntry(environment, expectedName) {
  return Object.entries(environment).find(([name]) => name.toUpperCase() === expectedName)?.[1]
}

function isRecord(value) {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function emitTrace(trace, event) {
  try {
    trace(Object.freeze({ ...event, context: Object.freeze(event.context) }))
  } catch {
    // Trace transport is non-authoritative and must not interrupt tree cleanup.
  }
}
