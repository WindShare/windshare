import { spawn } from 'node:child_process'
import { isAbsolute, resolve } from 'node:path'

import {
  GeneratedSemanticFailureError,
  generatedSemanticCauseMessage,
} from './failure.mjs'
import {
  encodeGeneratedSemanticWorkerRequest,
  parseGeneratedSemanticWorkerResult,
} from './worker-protocol.mjs'

const MAXIMUM_WORKER_STDOUT_BYTES = 4 * 1_024 * 1_024
const MAXIMUM_WORKER_STDERR_BYTES = 256 * 1_024

export function createGeneratedSemanticWorkerProcessSpec({
  nodeExecutable,
  workerPath,
  request,
  environment,
  workingDirectory,
}) {
  requireAbsolutePath(nodeExecutable, 'generated semantic Node executable')
  requireAbsolutePath(workerPath, 'generated semantic worker module')
  requireAbsolutePath(workingDirectory, 'generated semantic worker directory')
  if (!isRecord(environment)) throw new Error('generated semantic worker environment is required')
  return Object.freeze({
    executable: nodeExecutable,
    // A direct executable invocation cannot inherit the parent's process.execArgv;
    // only these explicit worker arguments cross the process boundary.
    arguments: Object.freeze([
      workerPath,
      encodeGeneratedSemanticWorkerRequest(request),
    ]),
    environment: Object.freeze({ ...environment }),
    workingDirectory,
  })
}

export function assertGeneratedSemanticParentProcessIsolation(
  permissionModel = process.permission,
) {
  // Node propagates an active permission model to spawned Node processes through
  // NODE_OPTIONS even when the caller supplies an otherwise empty environment.
  if (permissionModel !== undefined) {
    throw new GeneratedSemanticFailureError(
      'build',
      'parent-permission-model-active',
      'generated semantic worker requires a parent without the Node permission model',
    )
  }
}

export async function launchGeneratedSemanticWorker(
  options,
  { spawnProcess = spawn } = {},
) {
  assertGeneratedSemanticParentProcessIsolation()
  const spec = createGeneratedSemanticWorkerProcessSpec(options)
  const spawnEnvironment = createGeneratedSemanticSpawnEnvironment(spec.environment)
  return new Promise((resolvePromise, rejectPromise) => {
    let child
    try {
      child = spawnProcess(spec.executable, spec.arguments, {
        cwd: spec.workingDirectory,
        env: spawnEnvironment,
        shell: false,
        windowsHide: true,
        stdio: ['ignore', 'pipe', 'pipe'],
      })
    } catch (cause) {
      rejectPromise(new GeneratedSemanticFailureError(
        'build',
        'worker-spawn-failed',
        generatedSemanticCauseMessage(cause, 'generated semantic worker failed to start'),
        { cause },
      ))
      return
    }

    const stdout = boundedBytes('stdout', MAXIMUM_WORKER_STDOUT_BYTES)
    const stderr = boundedBytes('stderr', MAXIMUM_WORKER_STDERR_BYTES)
    let terminal = false
    let collectionFailure = null

    child.stdout.on('data', (chunk) => collect(stdout, chunk))
    child.stderr.on('data', (chunk) => collect(stderr, chunk))
    child.once('error', (cause) => settle(() => rejectPromise(new GeneratedSemanticFailureError(
      'build',
      'worker-spawn-failed',
      generatedSemanticCauseMessage(cause, 'generated semantic worker failed to start'),
      { cause },
    ))))
    child.once('close', (exitCode, signal) => settle(() => {
      if (collectionFailure !== null) {
        rejectPromise(collectionFailure)
        return
      }
      try {
        const stdoutText = stdout.text()
        const stderrText = stderr.text()
        resolvePromise(Object.freeze({
          terminal: signal === null ? 'exited' : 'signaled',
          exitCode,
          signal,
          stdout: stdoutText,
          stderr: stderrText,
        }))
      } catch (cause) {
        rejectPromise(cause)
      }
    }))

    function collect(collector, chunk) {
      if (collectionFailure !== null) return
      try {
        collector.write(chunk)
      } catch (cause) {
        collectionFailure = cause
        child.kill()
      }
    }

    function settle(action) {
      if (terminal) return
      terminal = true
      action()
    }
  })
}

export function createGeneratedSemanticSpawnEnvironment(environment) {
  if (!isRecord(environment)) throw new Error('generated semantic worker environment is required')
  const spawnEnvironment = Object.assign(Object.create(null), environment)
  // Node may otherwise inject its own coverage directory into a caller-supplied
  // environment. An owned undefined entry prevents mutation without reaching
  // the child process environment.
  Object.defineProperty(spawnEnvironment, 'NODE_V8_COVERAGE', {
    value: undefined,
    writable: true,
    enumerable: true,
    configurable: true,
  })
  return spawnEnvironment
}

export function requireSuccessfulGeneratedSemanticWorker(execution) {
  if (!isRecord(execution) || execution.terminal !== 'exited' || execution.signal !== null) {
    throw new GeneratedSemanticFailureError(
      'build',
      'worker-did-not-exit',
      'generated semantic worker did not exit normally',
    )
  }
  if (execution.stderr !== '') {
    throw new GeneratedSemanticFailureError(
      'build',
      'worker-stderr-not-empty',
      'generated semantic worker emitted unexpected stderr',
    )
  }
  let result
  try {
    result = parseGeneratedSemanticWorkerResult(execution.stdout)
  } catch (cause) {
    throw new GeneratedSemanticFailureError(
      'build',
      'worker-result-invalid',
      generatedSemanticCauseMessage(cause, 'generated semantic worker result is invalid'),
      { cause },
    )
  }
  if (result.outcome === 'failed') {
    if (execution.exitCode === 0) {
      throw new GeneratedSemanticFailureError(
        'build',
        'worker-exit-result-mismatch',
        'generated semantic worker exit code contradicts its failure result',
      )
    }
    throw new GeneratedSemanticFailureError(
      result.failure.kind,
      result.failure.code,
      result.failure.message,
    )
  }
  if (execution.exitCode !== 0) {
    throw new GeneratedSemanticFailureError(
      'build',
      'worker-exit-result-mismatch',
      'generated semantic worker exit code contradicts its build result',
    )
  }
  return result
}

function boundedBytes(name, maximumBytes) {
  const chunks = []
  let byteLength = 0
  return Object.freeze({
    write(chunk) {
      const bytes = Buffer.from(chunk)
      byteLength += bytes.byteLength
      if (byteLength > maximumBytes) {
        throw new GeneratedSemanticFailureError(
          'build',
          `worker-${name}-too-large`,
          `generated semantic worker ${name} exceeds its byte limit`,
        )
      }
      chunks.push(bytes)
    },
    text() {
      try {
        return new TextDecoder('utf-8', { fatal: true }).decode(Buffer.concat(chunks))
      } catch (cause) {
        throw new GeneratedSemanticFailureError(
          'build',
          `worker-${name}-invalid-utf8`,
          `generated semantic worker ${name} is not valid UTF-8`,
          { cause },
        )
      }
    },
  })
}

function requireAbsolutePath(value, label) {
  if (typeof value !== 'string' || !isAbsolute(value) || resolve(value) !== value) {
    throw new Error(`${label} must be absolute and canonical`)
  }
}

function isRecord(value) {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}
