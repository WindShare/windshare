import { spawn, type ChildProcessByStdio } from 'node:child_process'
import { EventEmitter } from 'node:events'
import type { Readable } from 'node:stream'

const MAXIMUM_CAPTURED_OUTPUT_CHARACTERS = 256 * 1024
const DEFAULT_READINESS_TIMEOUT_MILLISECONDS = 20_000
const DEFAULT_STOP_TIMEOUT_MILLISECONDS = 5_000
const FORCE_STOP_TIMEOUT_MILLISECONDS = 2_000

interface DirectProcessOutcome {
  readonly code: number | null
  readonly signal: NodeJS.Signals | null
  readonly spawnError?: Error
}

export interface DirectProcessOptions {
  readonly cwd: string
  readonly environment?: NodeJS.ProcessEnv
  readonly operationId: string
  readonly redactStdout?: boolean
}

export class DirectProcess {
  readonly #child: ChildProcessByStdio<null, Readable, Readable>
  readonly #events = new EventEmitter()
  readonly #operationId: string
  readonly #redactStdout: boolean
  readonly #completion: Promise<DirectProcessOutcome>
  #stdout = ''
  #stderr = ''
  #outcome: DirectProcessOutcome | undefined
  #stopRequested = false

  constructor(command: string, arguments_: readonly string[], options: DirectProcessOptions) {
    this.#operationId = options.operationId
    this.#redactStdout = options.redactStdout === true
    this.#child = spawn(command, arguments_, {
      cwd: options.cwd,
      env: options.environment ?? process.env,
      stdio: ['ignore', 'pipe', 'pipe'],
      windowsHide: true,
    })
    this.#child.stdout.on('data', (chunk: Buffer) => this.#append('stdout', chunk))
    this.#child.stderr.on('data', (chunk: Buffer) => this.#append('stderr', chunk))
    this.#completion = new Promise((resolveOutcome) => {
      let spawnError: Error | undefined
      this.#child.once('error', (error) => {
        spawnError = error
        this.#events.emit('changed')
      })
      this.#child.once('close', (code, signal) => {
        const outcome: DirectProcessOutcome = Object.freeze({
          code,
          signal,
          ...(spawnError === undefined ? {} : { spawnError }),
        })
        this.#outcome = outcome
        this.#events.emit('changed')
        resolveOutcome(outcome)
      })
    })
  }

  get operationId(): string {
    return this.#operationId
  }

  async waitFor(
    stream: 'stdout' | 'stderr',
    expression: RegExp,
    options: { readonly signal?: AbortSignal; readonly timeoutMilliseconds?: number } = {},
  ): Promise<RegExpMatchArray> {
    const timeoutMilliseconds = options.timeoutMilliseconds ??
      DEFAULT_READINESS_TIMEOUT_MILLISECONDS
    requirePositiveTimeout(timeoutMilliseconds, 'process readiness')
    options.signal?.throwIfAborted()
    const match = () => this.#captured(stream).match(expression)
    const immediate = match()
    if (immediate !== null) return immediate
    return await new Promise<RegExpMatchArray>((resolveMatch, rejectMatch) => {
      const cleanup = () => {
        clearTimeout(timeout)
        this.#events.off('changed', inspect)
        options.signal?.removeEventListener('abort', aborted)
      }
      const reject = (reason: unknown) => {
        cleanup()
        rejectMatch(reason)
      }
      const aborted = () => reject(options.signal?.reason ?? new Error('process readiness aborted'))
      const inspect = () => {
        const found = match()
        if (found !== null) {
          cleanup()
          resolveMatch(found)
        } else if (this.#outcome !== undefined) {
          reject(new Error(
            `${this.#operationId} exited before ${String(expression)} appeared in ${stream}; ` +
            this.diagnostic(),
          ))
        }
      }
      const timeout = setTimeout(() => reject(new Error(
        `${this.#operationId} timed out waiting for ${String(expression)} in ${stream}; ` +
        this.diagnostic(),
      )), timeoutMilliseconds)
      this.#events.on('changed', inspect)
      options.signal?.addEventListener('abort', aborted, { once: true })
      inspect()
    })
  }

  async stop(timeoutMilliseconds = DEFAULT_STOP_TIMEOUT_MILLISECONDS): Promise<void> {
    requirePositiveTimeout(timeoutMilliseconds, 'process stop')
    if (this.#stopRequested) {
      await this.#completion
      return
    }
    this.#stopRequested = true
    if (this.#outcome !== undefined) {
      throw new Error(`${this.#operationId} exited before cleanup; ${this.diagnostic()}`)
    }
    this.#child.kill()
    if (await settlesWithin(this.#completion, timeoutMilliseconds)) return
    this.#child.kill('SIGKILL')
    if (!await settlesWithin(this.#completion, FORCE_STOP_TIMEOUT_MILLISECONDS)) {
      throw new Error(`${this.#operationId} did not stop; ${this.diagnostic()}`)
    }
  }

  diagnostic(): string {
    const outcome = this.#outcome
    return JSON.stringify({
      operationId: this.#operationId,
      code: outcome?.code,
      signal: outcome?.signal,
      spawnError: outcome?.spawnError?.message,
      stdout: this.#redactStdout
        ? `<redacted capability output; ${this.#stdout.length} characters captured>`
        : this.#stdout,
      stderr: this.#stderr,
    })
  }

  #captured(stream: 'stdout' | 'stderr'): string {
    return stream === 'stdout' ? this.#stdout : this.#stderr
  }

  #append(stream: 'stdout' | 'stderr', chunk: Buffer): void {
    const current = this.#captured(stream)
    const appended = current + chunk.toString('utf8')
    const bounded = appended.length <= MAXIMUM_CAPTURED_OUTPUT_CHARACTERS
      ? appended
      : appended.slice(-MAXIMUM_CAPTURED_OUTPUT_CHARACTERS)
    if (stream === 'stdout') this.#stdout = bounded
    else this.#stderr = bounded
    this.#events.emit('changed')
  }
}

function requirePositiveTimeout(timeoutMilliseconds: number, label: string): void {
  if (!Number.isSafeInteger(timeoutMilliseconds) || timeoutMilliseconds <= 0) {
    throw new RangeError(`${label} timeout must be a positive safe integer`)
  }
}

async function settlesWithin(task: Promise<unknown>, timeoutMilliseconds: number): Promise<boolean> {
  let timeout: ReturnType<typeof setTimeout> | undefined
  try {
    return await Promise.race([
      task.then(() => true),
      new Promise<false>((resolveTimeout) => {
        timeout = setTimeout(() => resolveTimeout(false), timeoutMilliseconds)
      }),
    ])
  } finally {
    if (timeout !== undefined) clearTimeout(timeout)
  }
}
