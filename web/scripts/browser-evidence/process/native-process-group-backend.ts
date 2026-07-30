import { spawn, type ChildProcess } from 'node:child_process'
import { lstat } from 'node:fs/promises'
import { isAbsolute, resolve } from 'node:path'

import { childEvidenceEnvironment } from '../child-evidence.ts'
import type { RunnerProcessEvidence } from '../execution-evidence.ts'
import type {
  BrowserSampleContainmentBackend,
  BrowserSampleContainmentExecution,
  BrowserSampleContainmentPreflight,
  BrowserSampleContainmentRequest,
  BrowserSampleContainmentTraceSink,
  ContainedSampleCommand,
} from './containment.ts'
import { sampleProcessEnvironment } from './sample-environment.ts'

const GROUP_POLL_INTERVAL_MS = 25

export interface NativeProcessGroupCommandRequest {
  readonly command: ContainedSampleCommand
  readonly environment: Readonly<Record<string, string>>
  readonly deadlineMs: number
  readonly terminationGraceMs: number
  readonly terminationSignal?: AbortSignal
  readonly stdout: (chunk: Uint8Array) => void
  readonly stderr: (chunk: Uint8Array) => void
  readonly trace: BrowserSampleContainmentTraceSink
}

export function createNativeProcessGroupContainmentBackend(
  platform = process.platform,
): BrowserSampleContainmentBackend {
  if (platform === 'win32') {
    throw new Error('native process groups are unavailable on Windows')
  }
  return Object.freeze({
    kind: 'native-process-group' as const,
    preflight: preflightNativeProcessGroup,
    execute: executeNativeProcessGroup,
  })
}

async function preflightNativeProcessGroup(request: BrowserSampleContainmentPreflight): Promise<void> {
  await Promise.all([
    requireRegularNoFollowPath(request.topologyProfilePath, 'topology profile'),
    requireRegularNoFollowPath(request.topologyResolutionPath, 'topology resolution'),
    ...request.readOnlyInputRoots.map((path, index) =>
      requireExistingNoFollowPath(path, `read-only input ${index}`)),
  ])
}

async function executeNativeProcessGroup(
  request: BrowserSampleContainmentRequest,
): Promise<BrowserSampleContainmentExecution> {
  const environment = sampleProcessEnvironment(
    request.command.environment,
    childEvidenceEnvironment(request.childContext),
  )
  return executeNativeProcessGroupCommand({
    command: request.command,
    environment,
    deadlineMs: request.deadlineMs,
    terminationGraceMs: request.terminationGraceMs,
    ...(request.terminationSignal === undefined
      ? {}
      : { terminationSignal: request.terminationSignal }),
    stdout: request.stdout,
    stderr: request.stderr,
    trace: request.trace,
  })
}

export async function executeNativeProcessGroupCommand(
  request: NativeProcessGroupCommandRequest,
): Promise<BrowserSampleContainmentExecution> {
  validateNativeProcessGroupRequest(request)
  const child = spawn(request.command.executable, [...request.command.arguments], {
    cwd: request.command.cwd,
    detached: true,
    env: request.environment,
    shell: false,
    stdio: [request.command.stdin === undefined ? 'ignore' : 'pipe', 'pipe', 'pipe'],
  })
  const stdout = child.stdout
  const stderr = child.stderr
  if (stdout === null || stderr === null) {
    throw new Error('native process output pipes were not created')
  }
  const stdinDelivery = request.command.stdin === undefined
    ? Promise.resolve<unknown>(undefined)
    : deliverAnonymousStdin(child, request.command.stdin).then(
        () => undefined,
        (cause: unknown) => cause,
      )
  let outputSinkFailure: Error | undefined
  const forwardStdout = nonAuthoritativeByteSink('stdout', request.stdout, (failure) => {
    outputSinkFailure ??= failure
  })
  const forwardStderr = nonAuthoritativeByteSink('stderr', request.stderr, (failure) => {
    outputSinkFailure ??= failure
  })
  stdout.on('data', forwardStdout)
  stderr.on('data', forwardStderr)

  const terminal = childTerminal(child)
  let timedOut = false
  let terminationRequested = false
  let deadline: ReturnType<typeof setTimeout> | undefined
  let removeAbortListener: () => void = () => {}
  const termination = new Promise<'deadline' | 'parent-request'>((resolveTermination) => {
    deadline = setTimeout(() => {
      timedOut = true
      terminationRequested = true
      resolveTermination('deadline')
    }, request.deadlineMs)
    deadline.ref()
    const signal = request.terminationSignal
    if (signal === undefined) return
    const abort = () => {
      terminationRequested = true
      resolveTermination('parent-request')
    }
    if (signal.aborted) abort()
    else {
      signal.addEventListener('abort', abort, { once: true })
      removeAbortListener = () => signal.removeEventListener('abort', abort)
    }
  })

  let processEvidence: RunnerProcessEvidence | undefined
  let ownershipFailure: unknown
  try {
    const first = await Promise.race([
      terminal.then((evidence) => ({ kind: 'terminal' as const, evidence })),
      termination.then((reason) => ({ kind: 'termination' as const, reason })),
    ])
    if (first.kind === 'termination') {
      emitTrace(request, Object.freeze({
        milestone: 'native-process-group-termination-requested',
        context: Object.freeze({ reason: first.reason }),
      }))
      await retireProcessGroup(child.pid, request.terminationGraceMs)
      processEvidence = await terminal
    } else {
      processEvidence = first.evidence
      await retireResidualProcessGroup(child.pid, request.terminationGraceMs)
    }
    emitTrace(request, Object.freeze({
      milestone: 'native-process-group-tree-empty',
      context: Object.freeze({ timedOut, terminationRequested }),
    }))
  } catch (cause) {
    ownershipFailure = cause
  } finally {
    if (deadline !== undefined) clearTimeout(deadline)
    removeAbortListener()
    stdout.off('data', forwardStdout)
    stderr.off('data', forwardStderr)
    child.stdin?.destroy()
  }
  const stdinFailure = await stdinDelivery
  assertNativeProcessGroupSuccess(ownershipFailure, outputSinkFailure, stdinFailure)
  if (processEvidence === undefined) {
    throw new Error('native process group ended without terminal evidence')
  }
  return Object.freeze({ processEvidence, timedOut })
}

function validateNativeProcessGroupRequest(request: NativeProcessGroupCommandRequest): void {
  requirePositiveInteger(request.deadlineMs, 'native process deadline')
  requirePositiveInteger(request.terminationGraceMs, 'native process termination grace')
  if (!isAbsolute(request.command.executable)) {
    throw new Error('native process executable must be absolute')
  }
  if (request.command.cwd !== undefined && !isAbsolute(request.command.cwd)) {
    throw new Error('native process working directory must be absolute')
  }
}

function assertNativeProcessGroupSuccess(
  ownershipFailure: unknown,
  outputSinkFailure: Error | undefined,
  stdinFailure: unknown,
): void {
  if (stdinFailure !== undefined) {
    const deliveryFailure = new Error('native process anonymous stdin was not delivered exactly once')
    if (ownershipFailure !== undefined) {
      throw new AggregateError(
        [ownershipFailure, deliveryFailure],
        'native process retirement and anonymous stdin delivery both failed',
      )
    }
    ownershipFailure = deliveryFailure
  }
  if (ownershipFailure !== undefined && outputSinkFailure !== undefined) {
    throw new AggregateError(
      [ownershipFailure, outputSinkFailure],
      'native process-group retirement and output forwarding both failed',
      { cause: ownershipFailure },
    )
  }
  if (ownershipFailure !== undefined) throw ownershipFailure
  if (outputSinkFailure !== undefined) throw outputSinkFailure
}

async function deliverAnonymousStdin(child: ChildProcess, bytes: Uint8Array): Promise<void> {
  if (bytes.byteLength === 0 || bytes.byteLength > 1_048_576 || child.stdin === null) {
    throw new Error('native process anonymous stdin authority is invalid')
  }
  const stdin = child.stdin
  await new Promise<void>((resolve, reject) => {
    const failed = (cause: Error): void => reject(cause)
    stdin.once('error', failed)
    stdin.end(bytes, () => {
      stdin.off('error', failed)
      resolve()
    })
  })
}

function nonAuthoritativeByteSink(
  stream: 'stdout' | 'stderr',
  sink: (chunk: Uint8Array) => void,
  recordFailure: (failure: Error) => void,
): (chunk: Uint8Array) => void {
  let failed = false
  return (chunk) => {
    if (failed) return
    try {
      sink(chunk)
    } catch (cause) {
      failed = true
      recordFailure(new Error(`native process ${stream} sink failed`, { cause }))
    }
  }
}

function childTerminal(child: ChildProcess): Promise<RunnerProcessEvidence> {
  return new Promise((resolveTerminal) => {
    let settled = false
    const settle = (evidence: RunnerProcessEvidence) => {
      if (settled) return
      settled = true
      resolveTerminal(Object.freeze(evidence))
    }
    child.once('error', (cause: NodeJS.ErrnoException) => settle({
      terminal: 'spawn-failed',
      errorCode: cause.code ?? 'UNKNOWN',
      errorMessage: boundedMessage(cause.message),
    }))
    child.once('close', (code, signal) => {
      if (code !== null) settle({ terminal: 'exited', exitCode: code })
      else settle({ terminal: 'signaled', signal: signal ?? 'UNKNOWN' })
    })
  })
}

async function retireResidualProcessGroup(pid: number | undefined, graceMs: number): Promise<void> {
  if (pid === undefined || !processGroupExists(pid)) return
  await retireProcessGroup(pid, graceMs)
}

async function retireProcessGroup(pid: number | undefined, graceMs: number): Promise<void> {
  if (pid === undefined) return
  signalProcessGroup(pid, 'SIGTERM')
  if (await waitForProcessGroupExit(pid, graceMs)) return
  signalProcessGroup(pid, 'SIGKILL')
  if (await waitForProcessGroupExit(pid, graceMs)) return
  throw new Error('native process group remained alive after SIGKILL')
}

function signalProcessGroup(pid: number, signal: NodeJS.Signals): void {
  try {
    process.kill(-pid, signal)
  } catch (cause) {
    if ((cause as NodeJS.ErrnoException).code !== 'ESRCH') throw cause
  }
}

function processGroupExists(pid: number): boolean {
  try {
    process.kill(-pid, 0)
    return true
  } catch (cause) {
    if ((cause as NodeJS.ErrnoException).code === 'ESRCH') return false
    if ((cause as NodeJS.ErrnoException).code === 'EPERM') return true
    throw cause
  }
}

async function waitForProcessGroupExit(pid: number, maximumWaitMs: number): Promise<boolean> {
  const deadline = Date.now() + maximumWaitMs
  while (processGroupExists(pid)) {
    const remaining = deadline - Date.now()
    if (remaining <= 0) return false
    await new Promise<void>((resolveWait) => {
      const timer = setTimeout(resolveWait, Math.min(GROUP_POLL_INTERVAL_MS, remaining))
      timer.ref()
    })
  }
  return true
}

async function requireRegularNoFollowPath(path: string, label: string): Promise<void> {
  const status = await requireExistingNoFollowPath(path, label)
  if (!status.isFile()) throw new Error(`${label} must be a regular file`)
}

async function requireExistingNoFollowPath(path: string, label: string) {
  if (!isAbsolute(path) || resolve(path) !== path) {
    throw new Error(`${label} path must be absolute and canonical`)
  }
  const status = await lstat(path)
  if (status.isSymbolicLink()) throw new Error(`${label} must not be a symbolic link`)
  return status
}

function requirePositiveInteger(value: number, label: string): void {
  if (!Number.isSafeInteger(value) || value < 1) throw new Error(`${label} must be a positive integer`)
}

function boundedMessage(value: string): string {
  return value.length <= 512 ? value : value.slice(0, 512)
}

function emitTrace(
  request: Pick<NativeProcessGroupCommandRequest, 'trace'>,
  event: Parameters<BrowserSampleContainmentTraceSink>[0],
): void {
  try {
    request.trace(event)
  } catch {
    // Observability is deliberately non-authoritative: a hostile or broken
    // sink must never interrupt the process-group retirement transaction.
  }
}
