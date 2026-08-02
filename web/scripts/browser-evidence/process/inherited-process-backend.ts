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
  BrowserSampleContainmentTrace,
  InheritedProcessAuthority,
} from './containment.ts'
import { BrowserSampleContainmentError } from './containment.ts'
import {
  createOwnedByteChannel,
  createOwnedEventChannel,
  waitForExactWritableCompletion,
} from './owned-process-channel.mjs'
import { sampleProcessEnvironment } from './sample-environment.ts'

const MAXIMUM_CONTAINMENT_TRACE_RECORDS = 16

/**
 * This backend deliberately owns only the direct leaf. Its caller must itself
 * be the sole child of the outer common tree owner. Its signed settlement is
 * the only tree-quiescence proof, so a nested process group or Job here could
 * hide Playwright descendants from the scenario's cleanup oracle.
 */
export function createInheritedProcessContainmentBackend(
  outerAuthority: InheritedProcessAuthority,
  spawnProcess: typeof spawn = spawn,
): BrowserSampleContainmentBackend {
  requireOuterAuthority(outerAuthority)
  return Object.freeze({
    kind: 'inherited' as const,
    outerAuthority,
    preflight: preflightInheritedProcess,
    execute: (request: BrowserSampleContainmentRequest) =>
      executeInheritedProcess(request, spawnProcess),
  })
}

function requireOuterAuthority(value: InheritedProcessAuthority): void {
  if (
    value === null || typeof value !== 'object' || value.kind !== 'test-process-owner' ||
    !['windows_job', 'linux_subreaper'].includes(value.backend) ||
    typeof value.operationId !== 'string' || !/^[A-Za-z0-9._-]{1,256}$/u.test(value.operationId)
  ) throw new Error('inherited process containment requires an explicit outer tree authority')
}

async function preflightInheritedProcess(
  request: BrowserSampleContainmentPreflight,
): Promise<void> {
  await Promise.all([
    requireRegularNoFollowPath(request.topologyProfilePath, 'topology profile'),
    requireRegularNoFollowPath(request.topologyResolutionPath, 'topology resolution'),
    ...request.readOnlyInputRoots.map((path, index) =>
      requireExistingNoFollowPath(path, `read-only input ${index}`)),
  ])
}

async function executeInheritedProcess(
  request: BrowserSampleContainmentRequest,
  spawnProcess: typeof spawn,
): Promise<BrowserSampleContainmentExecution> {
  const stdout = createOwnedByteChannel(request.capture.stdoutBytes, 'inherited leaf stdout')
  const stderr = createOwnedByteChannel(request.capture.stderrBytes, 'inherited leaf stderr')
  const traces = createOwnedEventChannel<BrowserSampleContainmentTrace>(
    MAXIMUM_CONTAINMENT_TRACE_RECORDS,
    'inherited leaf containment traces',
  )
  try {
    const execution = await executeInheritedProcessBody(request, stdout, stderr, traces, spawnProcess)
    const captureFailure = aggregateFailures(
      'inherited leaf owned channels failed',
      [stdout.failure(), stderr.failure(), traces.failure()],
    )
    if (captureFailure !== undefined) throw captureFailure
    stdout.finish()
    stderr.finish()
    traces.finish()
    return Object.freeze({
      ...execution,
      output: Object.freeze({ stdout: stdout.view.snapshot(), stderr: stderr.view.snapshot() }),
      traces: traces.view.snapshot(),
    })
  } catch (cause) {
    traces.append(Object.freeze({
      milestone: 'inherited-leaf-failed',
      outcome: 'failed',
      context: Object.freeze({ failure: boundedMessage(cause instanceof Error ? cause.message : String(cause)) }),
    }))
    const failure = aggregateFailures(
      'inherited leaf containment failed',
      [cause, stdout.failure(), stderr.failure(), traces.failure()],
    ) ?? new Error('inherited leaf containment failed')
    stdout.finish()
    stderr.finish()
    traces.finish()
    throw new BrowserSampleContainmentError(
      'inherited leaf containment failed',
      traces.view.snapshot(),
      Object.freeze({ stdout: stdout.view.snapshot(), stderr: stderr.view.snapshot() }),
      failure,
    )
  }
}

async function executeInheritedProcessBody(
  request: BrowserSampleContainmentRequest,
  stdout: ReturnType<typeof createOwnedByteChannel>,
  stderr: ReturnType<typeof createOwnedByteChannel>,
  traces: ReturnType<typeof createOwnedEventChannel<BrowserSampleContainmentTrace>>,
  spawnProcess: typeof spawn,
): Promise<Omit<BrowserSampleContainmentExecution, 'output' | 'traces'>> {
  requirePositiveInteger(request.deadlineMs, 'inherited leaf deadline')
  requirePositiveInteger(request.terminationGraceMs, 'inherited leaf termination grace')
  if (!isAbsolute(request.command.executable)) {
    throw new Error('inherited leaf executable must be absolute')
  }
  if (request.command.cwd !== undefined && !isAbsolute(request.command.cwd)) {
    throw new Error('inherited leaf working directory must be absolute')
  }
  const environment = sampleProcessEnvironment(
    request.command.environment,
    childEvidenceEnvironment(request.childContext),
  )
  const child = spawnProcess(request.command.executable, [...request.command.arguments], {
    cwd: request.command.cwd,
    detached: false,
    env: environment,
    shell: false,
    stdio: [request.command.stdin === undefined ? 'ignore' : 'pipe', 'pipe', 'pipe'],
    windowsHide: true,
  })
  if (child.stdout === null || child.stderr === null) {
    child.kill('SIGTERM')
    throw new Error('inherited leaf output pipes were not created')
  }
  const forwardStdout = (chunk: Uint8Array) => stdout.append(chunk)
  const forwardStderr = (chunk: Uint8Array) => stderr.append(chunk)
  child.stdout.on('data', forwardStdout)
  child.stderr.on('data', forwardStderr)
  const stdinDelivery = request.command.stdin === undefined
    ? Promise.resolve<unknown>(undefined)
    : deliverAnonymousStdin(child, request.command.stdin).then(
        () => undefined,
        (cause: unknown) => cause,
      )
  const terminal = childTerminal(child)
  let terminationReason: BrowserSampleContainmentExecution['terminationReason'] = 'natural'
  let terminationRequested = false
  let timer: ReturnType<typeof setTimeout> | undefined
  let removeAbortListener = () => {}
  const termination = new Promise<void>((resolveTermination) => {
    const requestTermination = (reason: BrowserSampleContainmentExecution['terminationReason']) => {
      if (terminationRequested) return
      terminationRequested = true
      terminationReason = reason
      resolveTermination()
    }
    timer = setTimeout(() => {
      requestTermination('deadline')
    }, request.deadlineMs)
    timer.ref()
    const signal = request.terminationSignal
    if (signal === undefined) return
    const abort = () => requestTermination('stop')
    if (signal.aborted) abort()
    else {
      signal.addEventListener('abort', abort, { once: true })
      removeAbortListener = () => signal.removeEventListener('abort', abort)
    }
  })
  let processEvidence: RunnerProcessEvidence
  try {
    const first = await Promise.race([
      terminal.then((evidence) => ({ kind: 'terminal' as const, evidence })),
      termination.then(() => ({ kind: 'termination' as const })),
    ])
    if (first.kind === 'terminal') processEvidence = first.evidence
    else {
      traces.append(Object.freeze({
        milestone: 'inherited-leaf-termination-requested',
        outcome: 'started',
        context: Object.freeze({ terminationReason }),
      }))
      processEvidence = await terminateDirectLeaf(child, terminal, request.terminationGraceMs)
    }
  } finally {
    if (timer !== undefined) clearTimeout(timer)
    removeAbortListener()
    child.stdout.off('data', forwardStdout)
    child.stderr.off('data', forwardStderr)
    // Detached descendants can inherit these descriptors. Closing our read end
    // lets the driver finish so the outer subreaper/Job can retire that tree.
    child.stdout.destroy()
    child.stderr.destroy()
    child.stdin?.destroy()
  }
  const stdinFailure = await stdinDelivery
  if (stdinFailure !== undefined) {
    throw new Error('inherited leaf anonymous stdin was not delivered exactly once', {
      cause: stdinFailure,
    })
  }
  if (processEvidence.terminal === 'spawn-failed' && terminationReason === 'natural') {
    terminationReason = 'initialization_failed'
  }
  traces.append(Object.freeze({
    milestone: 'inherited-leaf-root-terminal',
    outcome: terminationReason === 'natural' ? 'succeeded' : 'failed',
    context: Object.freeze({ terminal: processEvidence.terminal, terminationReason }),
  }))
  return Object.freeze({ processEvidence, terminationReason })
}

async function terminateDirectLeaf(
  child: ChildProcess,
  terminal: Promise<RunnerProcessEvidence>,
  graceMs: number,
): Promise<RunnerProcessEvidence> {
  if (child.kill('SIGTERM') !== true) {
    throw new Error('inherited leaf root rejected its direct stop request')
  }
  const graceful = await boundedWait(terminal, graceMs)
  if (graceful !== undefined) return graceful
  throw new Error('inherited leaf root did not stop gracefully; outer owner retirement is required')
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
    // `exit`, unlike `close`, is not held open by a detached descendant that
    // inherited the leaf's stdout/stderr descriptors.
    child.once('exit', (code, signal) => {
      if (code !== null) settle({ terminal: 'exited', exitCode: code })
      else settle({ terminal: 'signaled', signal: signal ?? 'UNKNOWN' })
    })
  })
}

async function deliverAnonymousStdin(child: ChildProcess, bytes: Uint8Array): Promise<void> {
  const stdin = child.stdin
  if (bytes.byteLength === 0 || bytes.byteLength > 1_048_576 || stdin === null) {
    throw new Error('inherited leaf anonymous stdin authority is invalid')
  }
  const completion = waitForExactWritableCompletion(stdin, 'inherited leaf anonymous stdin')
  try {
    stdin.end(Buffer.from(bytes))
  } catch (cause) {
    stdin.destroy(cause instanceof Error ? cause : new Error(String(cause)))
  }
  await completion
}

function aggregateFailures(message: string, values: readonly unknown[]): Error | undefined {
  const failures: Error[] = []
  const observed = new Set<unknown>()
  for (const value of values) {
    if (value === undefined || observed.has(value)) continue
    observed.add(value)
    failures.push(value instanceof Error ? value : new Error(String(value)))
  }
  if (failures.length === 0) return undefined
  if (failures.length === 1) return failures[0]
  return new AggregateError(failures, message)
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

async function boundedWait<T>(promise: Promise<T>, milliseconds: number): Promise<T | undefined> {
  let timer: ReturnType<typeof setTimeout> | undefined
  const timeout = new Promise<undefined>((resolveTimeout) => {
    timer = setTimeout(() => resolveTimeout(undefined), milliseconds)
    timer.ref()
  })
  try {
    return await Promise.race([promise, timeout])
  } finally {
    if (timer !== undefined) clearTimeout(timer)
  }
}
