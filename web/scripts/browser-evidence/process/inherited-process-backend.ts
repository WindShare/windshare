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
} from './containment.ts'
import { sampleProcessEnvironment } from './sample-environment.ts'

/**
 * This backend deliberately owns only the direct leaf. Its caller must itself
 * be the sole child of a manifest-pinned OS tree owner. Keeping this module
 * incapable of claiming tree quiescence prevents a nested process group or Job
 * from hiding Playwright descendants from the settlement authority.
 */
export function createInheritedProcessContainmentBackend(): BrowserSampleContainmentBackend {
  return Object.freeze({
    kind: 'inherited' as const,
    preflight: preflightInheritedProcess,
    execute: executeInheritedProcess,
  })
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
): Promise<BrowserSampleContainmentExecution> {
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
  const child = spawn(request.command.executable, [...request.command.arguments], {
    cwd: request.command.cwd,
    detached: false,
    env: environment,
    shell: false,
    stdio: [request.command.stdin === undefined ? 'ignore' : 'pipe', 'pipe', 'pipe'],
    windowsHide: true,
  })
  if (child.stdout === null || child.stderr === null) {
    child.kill('SIGKILL')
    throw new Error('inherited leaf output pipes were not created')
  }
  let sinkFailure: Error | undefined
  const forwardStdout = nonAuthoritativeSink('stdout', request.stdout, (failure) => {
    sinkFailure ??= failure
  })
  const forwardStderr = nonAuthoritativeSink('stderr', request.stderr, (failure) => {
    sinkFailure ??= failure
  })
  child.stdout.on('data', forwardStdout)
  child.stderr.on('data', forwardStderr)
  const stdinDelivery = request.command.stdin === undefined
    ? Promise.resolve<unknown>(undefined)
    : deliverAnonymousStdin(child, request.command.stdin).then(
        () => undefined,
        (cause: unknown) => cause,
      )
  const terminal = childTerminal(child)
  let timedOut = false
  let timer: ReturnType<typeof setTimeout> | undefined
  let removeAbortListener = () => {}
  const termination = new Promise<void>((resolveTermination) => {
    timer = setTimeout(() => {
      timedOut = true
      resolveTermination()
    }, request.deadlineMs)
    timer.ref()
    const signal = request.terminationSignal
    if (signal === undefined) return
    const abort = () => resolveTermination()
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
      emitTrace(request, 'inherited-leaf-termination-requested', { timedOut })
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
  if (sinkFailure !== undefined) throw sinkFailure
  emitTrace(request, 'inherited-leaf-root-terminal', {
    terminal: processEvidence.terminal,
    timedOut,
  })
  return Object.freeze({ processEvidence, timedOut })
}

async function terminateDirectLeaf(
  child: ChildProcess,
  terminal: Promise<RunnerProcessEvidence>,
  graceMs: number,
): Promise<RunnerProcessEvidence> {
  child.kill('SIGTERM')
  const graceful = await boundedWait(terminal, graceMs)
  if (graceful !== undefined) return graceful
  child.kill('SIGKILL')
  const killed = await boundedWait(terminal, graceMs)
  if (killed === undefined) {
    throw new Error('inherited leaf root remained alive after SIGKILL')
  }
  return killed
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
  await new Promise<void>((resolveDelivery, rejectDelivery) => {
    const failed = (cause: Error) => rejectDelivery(cause)
    stdin.once('error', failed)
    stdin.end(bytes, () => {
      stdin.off('error', failed)
      resolveDelivery()
    })
  })
}

function nonAuthoritativeSink(
  stream: string,
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
      recordFailure(new Error(`inherited leaf ${stream} sink failed`, { cause }))
    }
  }
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

function emitTrace(
  request: BrowserSampleContainmentRequest,
  milestone: string,
  context: Readonly<Record<string, unknown>>,
): void {
  try {
    request.trace(Object.freeze({ milestone, context: Object.freeze(context) }))
  } catch {
    // The outer owner, not observability transport, owns retirement authority.
  }
}
