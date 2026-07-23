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
  readonly #redactDiagnostics: boolean
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
    this.#redactDiagnostics = options.redactDiagnostics === true
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
    this.#exit = new Promise((resolveExit) => {
      this.#child.once('error', (error) => {
        this.#spawnFailure = error
        const outcome = { code: null, signal: null }
        this.#recordOutcome(outcome)
        resolveExit(outcome)
      })
      this.#child.once('exit', (code, signal) => {
        const outcome = { code, signal }
        this.#recordOutcome(outcome)
        resolveExit(outcome)
      })
    })
  }

  get stderr(): string {
    return this.#redactDiagnostics ? this.#diagnostic('stderr') : this.#stderr
  }

  forgetCapturedStdout(): void {
    this.#stdout = ''
  }

  terminateForRunnerLoss(): void {
    const running = this.#child.exitCode === null && this.#child.signalCode === null
    if (!running) return
    this.#spawnFailure ??= new Error('The auditing runner guard disconnected')
    this.#child.kill('SIGKILL')
  }

  async waitFor(
    stream: 'stdout' | 'stderr',
    expression: RegExp,
    timeoutMilliseconds = PROCESS_READY_TIMEOUT_MILLISECONDS,
  ): Promise<RegExpMatchArray> {
    const current = () => (stream === 'stdout' ? this.#stdout : this.#stderr)
    const match = () => current().match(expression)
    const immediate = match()
    if (immediate !== null) return immediate
    return await new Promise<RegExpMatchArray>((resolveMatch, rejectMatch) => {
      const cleanup = () => {
        clearTimeout(timeout)
        this.#events.off('output', inspect)
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
        rejectMatch(new Error(
          `Timed out waiting for ${expression} in ${stream}. ` +
          `stdout=${this.#diagnostic('stdout')} stderr=${this.#diagnostic('stderr')}`,
        ))
      }, timeoutMilliseconds)
      this.#events.on('output', inspect)
      inspect()
    })
  }

  async waitForExit(): Promise<void> {
    await this.#exit
  }

  async stop(): Promise<void> {
    const stopAlreadyRequested = this.#stopRequested
    const running = this.#child.exitCode === null && this.#child.signalCode === null
    const settledBeforeStop = !stopAlreadyRequested && (this.#outcome !== undefined || !running)
    this.#stopRequested = true
    if (running) this.#child.kill('SIGKILL')
    let timeout: ReturnType<typeof setTimeout> | undefined
    try {
      const outcome = await Promise.race([
        this.#exit,
        new Promise<never>((_, rejectStop) => {
          timeout = setTimeout(
            () => rejectStop(new Error('Timed out stopping an E2E child process')),
            PROCESS_STOP_TIMEOUT_MILLISECONDS,
          )
        }),
      ])
      if (this.#spawnFailure !== undefined) {
        throw new Error('E2E child process could not be started', { cause: this.#spawnFailure })
      }
      if (this.#prematureOutcome !== undefined || settledBeforeStop) {
        const premature = this.#prematureOutcome ?? outcome
        throw new Error(
          `E2E child exited before cleanup (code=${String(premature.code)}, ` +
          `signal=${String(premature.signal)})`,
        )
      }
    } finally {
      if (timeout !== undefined) clearTimeout(timeout)
    }
  }

  #recordOutcome(outcome: ProcessOutcome): void {
    this.#outcome = outcome
    if (!this.#stopRequested) this.#prematureOutcome = outcome
    this.#events.emit('output')
  }

  #prematureExitError(stream: 'stdout' | 'stderr', expression: RegExp): Error {
    return new Error(
      `E2E child exited before ${expression} appeared in ${stream} ` +
      `(code=${String(this.#outcome?.code)}, signal=${String(this.#outcome?.signal)}). ` +
      `stdout=${this.#diagnostic('stdout')} stderr=${this.#diagnostic('stderr')}`,
      { cause: this.#spawnFailure },
    )
  }

  #diagnostic(stream: 'stdout' | 'stderr'): string {
    const captured = stream === 'stdout' ? this.#stdout : this.#stderr
    if (this.#redactDiagnostics) {
      return `<redacted capability ${stream}; ${captured.length} characters captured>`
    }
    return JSON.stringify(captured)
  }
}

function boundedAppend(current: string, chunk: Buffer): string {
  const next = current + chunk.toString('utf8')
  return next.length <= MAXIMUM_CAPTURED_OUTPUT_CHARACTERS
    ? next
    : next.slice(next.length - MAXIMUM_CAPTURED_OUTPUT_CHARACTERS)
}
