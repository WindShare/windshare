import { spawn, type ChildProcessWithoutNullStreams } from 'node:child_process'
import { EventEmitter } from 'node:events'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')
const PROCESS_READY_TIMEOUT_MILLISECONDS = 30_000
const PROCESS_STOP_TIMEOUT_MILLISECONDS = 10_000
const MAXIMUM_CAPTURED_OUTPUT_CHARACTERS = 1_000_000

interface ProcessOutcome {
  readonly code: number | null
  readonly signal: NodeJS.Signals | null
}

export interface ManagedProcessOptions {
  readonly redactDiagnostics?: boolean
  readonly redactStdout?: boolean
}

export class FixtureInfrastructureError extends Error {
  constructor(boundary: string, cause?: unknown) {
    super(infrastructureFailureMessage(boundary, cause), cause === undefined ? undefined : { cause })
    this.name = 'FixtureInfrastructureError'
  }
}

export function containsFixtureInfrastructureFailure(reason: unknown): boolean {
  return containsFixtureInfrastructureFailureInner(reason, new Set())
}

export async function settleCleanupTasks(
  tasks: readonly Promise<unknown>[],
  boundary: string,
): Promise<void> {
  const results = await Promise.allSettled(tasks)
  const failures = results.flatMap((result) =>
    result.status === 'rejected' ? [result.reason] : [],
  )
  if (failures.length === 1) throw failures[0]
  if (failures.length > 1) throw new AggregateError(failures, `${boundary} cleanup failed`)
}

/** Owns one diagnostic child without importing any sharing protocol fixture. */
export class ManagedProcess {
  readonly #child: ChildProcessWithoutNullStreams
  readonly #events = new EventEmitter()
  readonly #exit: Promise<ProcessOutcome>
  readonly #redactStdout: boolean
  readonly #redactStderr: boolean
  #stdout = ''
  #stderr = ''
  #spawnFailure: unknown
  #outcome: ProcessOutcome | undefined
  #prematureOutcome: ProcessOutcome | undefined
  #stopRequested = false

  constructor(
    command: string,
    args: readonly string[],
    options: ManagedProcessOptions = {},
  ) {
    this.#redactStdout = options.redactDiagnostics === true || options.redactStdout === true
    this.#redactStderr = options.redactDiagnostics === true
    this.#child = spawn(command, args, {
      cwd: REPOSITORY_ROOT,
      windowsHide: true,
      stdio: ['pipe', 'pipe', 'pipe'],
    })
    this.#child.stdin.end()
    this.#child.stdout.on('data', (chunk: Buffer) => {
      this.#stdout = boundedAppend(this.#stdout, chunk)
      this.#events.emit('output')
    })
    this.#child.stderr.on('data', (chunk: Buffer) => {
      this.#stderr = boundedAppend(this.#stderr, chunk)
      this.#events.emit('output')
    })
    // `exit` precedes stdio drain. Only `close` proves that readiness markers and
    // crash diagnostics inherited by descendants can no longer arrive.
    this.#exit = new Promise((resolveExit) => {
      this.#child.once('error', (error) => {
        this.#spawnFailure = error
        this.#events.emit('output')
      })
      this.#child.once('close', (code, signal) => {
        const outcome = { code, signal }
        this.#recordOutcome(outcome)
        resolveExit(outcome)
      })
    })
  }

  get stderr(): string {
    return this.#redactStderr ? this.#diagnostic('stderr') : this.#stderr
  }

  forgetCapturedStdout(): void {
    this.#stdout = ''
  }

  terminateForRunnerLoss(): void {
    const running = this.#child.exitCode === null && this.#child.signalCode === null
    if (!running) return
    this.#spawnFailure ??= new FixtureInfrastructureError(
      'The auditing runner guard disconnected',
    )
    this.#child.kill('SIGKILL')
  }

  async waitFor(
    stream: 'stdout' | 'stderr',
    expression: RegExp,
    timeoutMilliseconds = PROCESS_READY_TIMEOUT_MILLISECONDS,
    signal?: AbortSignal,
  ): Promise<RegExpMatchArray> {
    requirePositiveTimeout(timeoutMilliseconds, 'Process readiness')
    if (signal?.aborted === true) throw abortFailure(signal, 'Process readiness was aborted')
    const current = () => (stream === 'stdout' ? this.#stdout : this.#stderr)
    const match = () => current().match(expression)
    const immediate = match()
    if (immediate !== null) return immediate
    return await new Promise<RegExpMatchArray>((resolveMatch, rejectMatch) => {
      const cleanup = () => {
        clearTimeout(timeout)
        this.#events.off('output', inspect)
        signal?.removeEventListener('abort', aborted)
      }
      const aborted = () => {
        cleanup()
        rejectMatch(abortFailure(signal, 'Process readiness was aborted'))
      }
      const inspect = () => {
        const found = match()
        if (found !== null) {
          cleanup()
          resolveMatch(found)
          return
        }
        if (this.#spawnFailure !== undefined || this.#outcome !== undefined) {
          cleanup()
          rejectMatch(this.#prematureExitError(stream, expression))
        }
      }
      const timeout = setTimeout(() => {
        cleanup()
        rejectMatch(new FixtureInfrastructureError(
          `Timed out waiting for ${expression} in ${stream}. ` +
          `stdout=${this.#diagnostic('stdout')} stderr=${this.#diagnostic('stderr')}`,
        ))
      }, timeoutMilliseconds)
      this.#events.on('output', inspect)
      signal?.addEventListener('abort', aborted, { once: true })
      inspect()
    })
  }

  async waitForExit(): Promise<void> {
    await this.#exit
  }

  async stop(
    timeoutMilliseconds = PROCESS_STOP_TIMEOUT_MILLISECONDS,
    signal?: AbortSignal,
  ): Promise<void> {
    requirePositiveTimeout(timeoutMilliseconds, 'Process stop')
    const stopAlreadyRequested = this.#stopRequested
    const running = this.#child.exitCode === null && this.#child.signalCode === null
    const settledBeforeStop = !stopAlreadyRequested && (this.#outcome !== undefined || !running)
    this.#stopRequested = true
    // Cancellation must never suppress the local kill. The runner guard may be
    // gone precisely when bounded teardown needs this ownership action most.
    if (running) this.#child.kill('SIGKILL')
    let timeout: ReturnType<typeof setTimeout> | undefined
    let aborted: (() => void) | undefined
    try {
      const outcome = await Promise.race([
        this.#exit,
        new Promise<never>((_, rejectStop) => {
          timeout = setTimeout(
            () => rejectStop(new FixtureInfrastructureError(
              'Timed out stopping an E2E child process',
            )),
            timeoutMilliseconds,
          )
        }),
        new Promise<never>((_, rejectStop) => {
          aborted = () => rejectStop(abortFailure(signal, 'Process stop was aborted'))
          if (signal?.aborted === true) aborted()
          else signal?.addEventListener('abort', aborted, { once: true })
        }),
      ])
      if (this.#spawnFailure !== undefined) {
        throw new FixtureInfrastructureError(
          'E2E child process could not be started',
          this.#spawnFailure,
        )
      }
      if (this.#prematureOutcome !== undefined || settledBeforeStop) {
        const premature = this.#prematureOutcome ?? outcome
        throw new FixtureInfrastructureError(
          `E2E child exited before cleanup (code=${String(premature.code)}, ` +
          `signal=${String(premature.signal)})`,
        )
      }
    } finally {
      if (timeout !== undefined) clearTimeout(timeout)
      if (aborted !== undefined) signal?.removeEventListener('abort', aborted)
    }
  }

  async stopAndDrain(timeoutMilliseconds = PROCESS_STOP_TIMEOUT_MILLISECONDS): Promise<void> {
    let stopFailure: unknown
    try {
      await this.stop(timeoutMilliseconds)
    } catch (error) {
      stopFailure = error
    }
    // The deadline wrapper may already have released its caller, but local
    // directory ownership cannot move on until Windows has closed child handles
    // and inherited diagnostic streams have drained.
    await this.#exit
    if (stopFailure !== undefined) throw stopFailure
  }

  #recordOutcome(outcome: ProcessOutcome): void {
    this.#outcome = outcome
    if (!this.#stopRequested) this.#prematureOutcome = outcome
    this.#events.emit('output')
  }

  #prematureExitError(stream: 'stdout' | 'stderr', expression: RegExp): Error {
    return new FixtureInfrastructureError(
      `E2E child exited before ${expression} appeared in ${stream} ` +
      `(code=${String(this.#outcome?.code)}, signal=${String(this.#outcome?.signal)}). ` +
      `stdout=${this.#diagnostic('stdout')} stderr=${this.#diagnostic('stderr')}`,
      this.#spawnFailure,
    )
  }

  #diagnostic(stream: 'stdout' | 'stderr'): string {
    const captured = stream === 'stdout' ? this.#stdout : this.#stderr
    if (stream === 'stdout' ? this.#redactStdout : this.#redactStderr) {
      return `<redacted capability ${stream}; ${captured.length} characters captured>`
    }
    return JSON.stringify(captured)
  }
}

function containsFixtureInfrastructureFailureInner(
  reason: unknown,
  visited: Set<object>,
): boolean {
  if (reason instanceof FixtureInfrastructureError) return true
  if (reason === null || typeof reason !== 'object' || visited.has(reason)) return false
  visited.add(reason)
  if (reason instanceof AggregateError) {
    return reason.errors.some((error) => containsFixtureInfrastructureFailureInner(error, visited)) ||
      containsFixtureInfrastructureFailureInner(reason.cause, visited)
  }
  return reason instanceof Error &&
    containsFixtureInfrastructureFailureInner(reason.cause, visited)
}

function infrastructureFailureMessage(boundary: string, cause: unknown): string {
  if (cause === undefined) return boundary
  const detail = cause instanceof Error ? cause.message : String(cause)
  return `${boundary}: ${detail}`
}

function abortFailure(signal: AbortSignal | undefined, boundary: string): unknown {
  return signal?.reason ?? new FixtureInfrastructureError(boundary)
}

function requirePositiveTimeout(timeoutMilliseconds: number, boundary: string): void {
  if (!Number.isSafeInteger(timeoutMilliseconds) || timeoutMilliseconds <= 0) {
    throw new RangeError(`${boundary} timeout must be a positive integer`)
  }
}

function boundedAppend(current: string, chunk: Buffer): string {
  const next = current + chunk.toString('utf8')
  return next.length <= MAXIMUM_CAPTURED_OUTPUT_CHARACTERS
    ? next
    : next.slice(next.length - MAXIMUM_CAPTURED_OUTPUT_CHARACTERS)
}
