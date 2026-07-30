import { spawn } from 'node:child_process'
import { posix, win32 } from 'node:path'

import { childEvidenceEnvironment } from '../../scripts/browser-evidence/child-evidence.ts'
import type {
  BrowserSampleContainmentBackend,
  BrowserSampleContainmentRequest,
  ContainedSampleCommand,
} from '../../scripts/browser-evidence/process/containment.ts'
import type { RunnerProcessEvidence } from '../../scripts/browser-evidence/execution-evidence.ts'

/** Process-backed semantics belong exclusively to the browser process gate. */
export function createProcessTestContainmentBackend(): BrowserSampleContainmentBackend {
  return Object.freeze({
    kind: 'test' as const,
    preflight: async () => undefined,
    execute: executeProcessChild,
  })
}

async function executeProcessChild(request: BrowserSampleContainmentRequest) {
  const command = mapAttachmentPaths(request)
  const environment = {
    ...process.env,
    ...command.environment,
    ...childEvidenceEnvironment(request.childContext),
  }
  let child
  try {
    child = spawn(command.executable, [...command.arguments], {
      cwd: command.cwd,
      env: environment,
      shell: false,
      stdio: ['ignore', 'pipe', 'pipe'],
      windowsHide: true,
    })
  } catch (cause) {
    return Object.freeze({ processEvidence: spawnFailure(cause), timedOut: false })
  }
  child.stdout.on('data', (chunk: Buffer) => request.stdout(chunk))
  child.stderr.on('data', (chunk: Buffer) => request.stderr(chunk))
  let spawnError: unknown
  child.once('error', (cause) => { spawnError = cause })
  const terminal = new Promise<{ readonly code: number | null; readonly signal: NodeJS.Signals | null }>(
    (resolveTerminal) => child.once('close', (code, signal) => resolveTerminal({ code, signal })),
  )
  let timer: ReturnType<typeof setTimeout> | undefined
  const completion = await Promise.race([
    terminal.then((value) => Object.freeze({ kind: 'terminal' as const, value })),
    new Promise<{ readonly kind: 'deadline' }>((resolveDeadline) => {
      timer = setTimeout(() => resolveDeadline(Object.freeze({ kind: 'deadline' })), request.deadlineMs)
      timer.unref()
    }),
  ])
  if (timer !== undefined) clearTimeout(timer)
  if (completion.kind === 'terminal') {
    return Object.freeze({
      processEvidence: terminalEvidence(completion.value, spawnError),
      timedOut: false,
    })
  }
  const intervened = child.kill('SIGKILL')
  const value = await terminal
  return Object.freeze({
    processEvidence: terminalEvidence(value, spawnError),
    timedOut: intervened,
  })
}

function mapAttachmentPaths(request: BrowserSampleContainmentRequest): ContainedSampleCommand {
  const pathApi = process.platform === 'win32' ? win32 : posix
  const finalRoot = pathApi.join(request.sampleDirectory, 'attachments')
  const mapValue = (value: string): string => {
    if (!pathApi.isAbsolute(value)) return value
    const relative = pathApi.relative(finalRoot, value)
    if (
      relative === '..' || relative.startsWith(`..${pathApi.sep}`) || pathApi.isAbsolute(relative)
    ) return value
    return relative === ''
      ? request.childAttachmentStagingRoot
      : pathApi.join(request.childAttachmentStagingRoot, relative)
  }
  return Object.freeze({
    executable: request.command.executable,
    arguments: Object.freeze(request.command.arguments.map(mapValue)),
    ...(request.command.cwd === undefined ? {} : { cwd: request.command.cwd }),
    ...(request.command.environment === undefined
      ? {}
      : {
          environment: Object.freeze(Object.fromEntries(
            Object.entries(request.command.environment).map(([name, value]) => [name, mapValue(value)]),
          )),
        }),
  })
}

function terminalEvidence(
  terminal: { readonly code: number | null; readonly signal: NodeJS.Signals | null },
  spawnError: unknown,
): RunnerProcessEvidence {
  if (spawnError !== undefined) return spawnFailure(spawnError)
  if (terminal.signal !== null) return Object.freeze({ terminal: 'signaled', signal: terminal.signal })
  if (terminal.code !== null) return Object.freeze({ terminal: 'exited', exitCode: terminal.code >>> 0 })
  return Object.freeze({
    terminal: 'spawn-failed',
    errorCode: 'NO_TERMINAL',
    errorMessage: 'process test child closed without a terminal status',
  })
}

function spawnFailure(cause: unknown): RunnerProcessEvidence {
  const code = typeof cause === 'object' && cause !== null && 'code' in cause
    ? String((cause as NodeJS.ErrnoException).code ?? 'SPAWN_ERROR')
    : 'SPAWN_ERROR'
  return Object.freeze({
    terminal: 'spawn-failed',
    errorCode: code.replace(/[^A-Za-z0-9._-]/gu, '_').slice(0, 128) || 'SPAWN_ERROR',
    errorMessage: cause instanceof Error ? cause.message.slice(0, 512) : String(cause).slice(0, 512),
  })
}
