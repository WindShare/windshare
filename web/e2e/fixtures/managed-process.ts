import { EventEmitter } from 'node:events'

import type {
  InheritedChildProcessBackend,
  InheritedChildSession,
  InheritedChildTerminal,
} from '../../scripts/browser-evidence/process/inherited-child-process.mjs'
import type { TestEvent } from '../../scripts/browser-evidence/process/test-event-channel.mjs'
import type { TestIdentity } from '../../scripts/browser-evidence/process/test-identity.mjs'

const PROCESS_READY_TIMEOUT_MILLISECONDS = 30_000
const PROCESS_STOP_TIMEOUT_MILLISECONDS = 10_000
const MAXIMUM_CAPTURED_OUTPUT_CHARACTERS = 1_000_000
const MAXIMUM_CAPTURED_OUTPUT_BYTES = 4_194_304
const MAXIMUM_CAPTURED_PROCESS_EVENTS = 1_024

interface ProcessOutcome {
  readonly code: number | null
  readonly signal: string | null
}

export interface ManagedProcessOptions {
  readonly redactDiagnostics?: boolean
  readonly redactStdout?: boolean
  readonly backend: InheritedChildProcessBackend
  readonly identity: TestIdentity
  readonly environment: Readonly<Record<string, string>>
  readonly events?: {
    readonly minimumEvents: number
    readonly maximumEvents: number
  }
}

export interface ManagedProcessEventExpectation {
  readonly component: string
  readonly milestone: string
  readonly outcome: string
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

/**
 * Manages one direct descendant while the outer process owner remains the only
 * authority for hard tree retirement. Stop never creates a second Job/pgroup.
 */
export class ManagedProcess {
  readonly #events = new EventEmitter()
  readonly #exit: Promise<ProcessOutcome>
  readonly #redactStdout: boolean
  readonly #redactStderr: boolean
  #stdout = ''
  #stderr = ''
  readonly #processEvents: TestEvent[] = []
  #session: InheritedChildSession | undefined
  #spawnFailure: unknown
  #backendFailure: unknown
  #outcome: ProcessOutcome | undefined
  #prematureOutcome: ProcessOutcome | undefined
  #stopRequested = false

  constructor(
    command: string,
    args: readonly string[],
    options: ManagedProcessOptions,
  ) {
    if (options?.backend?.kind !== 'inherited-descendant' ||
        typeof options.backend.launch !== 'function') {
      throw new Error('ManagedProcess requires an injected inherited-descendant backend')
    }
    this.#redactStdout = options.redactDiagnostics === true || options.redactStdout === true
    this.#redactStderr = options.redactDiagnostics === true
    this.#exit = this.#execute(command, args, options)
  }

  get stderr(): string {
    return this.#redactStderr ? this.#diagnostic('stderr') : this.#stderr
  }

  capturedOutputContains(value: string): boolean {
    if (value.length === 0) throw new TypeError('Captured-output probe cannot be empty')
    return this.#stdout.includes(value) || this.#stderr.includes(value)
  }

  forgetCapturedStdout(): void {
    this.#stdout = ''
  }

  terminateForRunnerLoss(): void {
    if (this.#outcome !== undefined) return
    this.#backendFailure ??= new FixtureInfrastructureError(
      'The auditing runner guard disconnected',
    )
    this.#stopRequested = true
    this.#requestStop()
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
        if (this.#spawnFailure !== undefined || this.#backendFailure !== undefined ||
            this.#outcome !== undefined) {
          cleanup()
          rejectMatch(this.#prematureExitError(stream, expression))
        }
      }
      const timeout = setTimeout(() => {
        cleanup()
        this.#rejectAfterDeadline(
          `Timed out waiting for ${expression} in ${stream}`,
          rejectMatch,
        )
      }, timeoutMilliseconds)
      this.#events.on('output', inspect)
      signal?.addEventListener('abort', aborted, { once: true })
      inspect()
    })
  }

  async waitForEvent(
    expectation: ManagedProcessEventExpectation,
    timeoutMilliseconds = PROCESS_READY_TIMEOUT_MILLISECONDS,
    signal?: AbortSignal,
  ): Promise<TestEvent> {
    requirePositiveTimeout(timeoutMilliseconds, 'Process event readiness')
    if (signal?.aborted === true) throw abortFailure(signal, 'Process event readiness was aborted')
    const match = () => this.#processEvents.find((event) =>
      event.component === expectation.component &&
      event.milestone === expectation.milestone &&
      event.outcome === expectation.outcome)
    const immediate = match()
    if (immediate !== undefined) return immediate
    return await new Promise<TestEvent>((resolveEvent, rejectEvent) => {
      const cleanup = () => {
        clearTimeout(timeout)
        this.#events.off('event', inspect)
        signal?.removeEventListener('abort', aborted)
      }
      const aborted = () => {
        cleanup()
        rejectEvent(abortFailure(signal, 'Process event readiness was aborted'))
      }
      const inspect = () => {
        const found = match()
        if (found !== undefined) {
          cleanup()
          resolveEvent(found)
          return
        }
        if (this.#spawnFailure !== undefined || this.#backendFailure !== undefined ||
            this.#outcome !== undefined) {
          cleanup()
          rejectEvent(new FixtureInfrastructureError(
            `E2E child failed before private event ${JSON.stringify(expectation)}. ` +
            this.#structuredDiagnostic(),
            this.#spawnFailure ?? this.#backendFailure,
          ))
        }
      }
      const timeout = setTimeout(() => {
        cleanup()
        this.#rejectAfterDeadline(
          `Timed out waiting for private event ${JSON.stringify(expectation)}`,
          rejectEvent,
        )
      }, timeoutMilliseconds)
      this.#events.on('event', inspect)
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
    const settledBeforeStop = !stopAlreadyRequested && this.#outcome !== undefined
    this.#stopRequested = true
    this.#requestStop()
    let timeout: ReturnType<typeof setTimeout> | undefined
    let aborted: (() => void) | undefined
    try {
      const outcome = await Promise.race([
        this.#exit,
        new Promise<never>((_, rejectStop) => {
          timeout = setTimeout(
            () => rejectStop(new FixtureInfrastructureError(
              'Timed out joining an E2E direct child; outer owner retirement is required',
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
        throw new FixtureInfrastructureError('E2E child process could not be started', this.#spawnFailure)
      }
      if (this.#backendFailure !== undefined) {
        throw new FixtureInfrastructureError(
          `E2E direct-child backend did not settle cleanly. ${this.#structuredDiagnostic()}`,
          this.#backendFailure,
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
    await this.stop(timeoutMilliseconds)
  }

  #requestStop(): void {
    if (this.#session === undefined || this.#outcome !== undefined) return
    try {
      this.#session.requestStop()
    } catch (cause) {
      this.#recordBackendFailure(new Error('E2E direct child stop request failed', { cause }))
    }
  }

  #recordOutcome(outcome: ProcessOutcome): void {
    this.#outcome = outcome
    if (!this.#stopRequested) this.#prematureOutcome = outcome
    this.#events.emit('output')
    this.#events.emit('event')
  }

  #appendOutput(stream: 'stdout' | 'stderr', chunk: Uint8Array): void {
    if (stream === 'stdout') this.#stdout = boundedAppend(this.#stdout, chunk)
    else this.#stderr = boundedAppend(this.#stderr, chunk)
    this.#events.emit('output')
  }

  #appendProcessEvent(event: TestEvent): void {
    if (this.#processEvents.length >= MAXIMUM_CAPTURED_PROCESS_EVENTS) {
      this.#recordBackendFailure(new Error('direct child exceeded its bounded event history'))
      return
    }
    this.#processEvents.push(event)
    this.#events.emit('event')
  }

  #recordBackendFailure(cause: unknown): void {
    this.#backendFailure ??= cause
    this.#events.emit('output')
    this.#events.emit('event')
  }

  async #consumeOutput(
    stream: 'stdout' | 'stderr',
    source: AsyncIterable<Uint8Array>,
  ): Promise<void> {
    for await (const chunk of source) this.#appendOutput(stream, chunk)
  }

  async #consumeProcessEvents(source: AsyncIterable<TestEvent>): Promise<void> {
    for await (const event of source) this.#appendProcessEvent(event)
  }

  #observeChannel(task: Promise<void>): Promise<void> {
    return task.catch((cause) => {
      this.#recordBackendFailure(cause)
      throw cause
    })
  }

  async #execute(
    command: string,
    args: readonly string[],
    options: ManagedProcessOptions,
  ): Promise<ProcessOutcome> {
    let terminal: InheritedChildTerminal | undefined
    try {
      this.#session = options.backend.launch({
        identity: options.identity,
        command: { executable: command, arguments: args, cwd: process.cwd() },
        environment: options.environment,
        capture: Object.freeze({
          stdoutBytes: MAXIMUM_CAPTURED_OUTPUT_BYTES,
          stderrBytes: MAXIMUM_CAPTURED_OUTPUT_BYTES,
        }),
        ...(options.events === undefined
          ? {}
          : {
              events: options.events,
            }),
      })
      const channelDrains = [
        this.#observeChannel(this.#consumeOutput('stdout', this.#session.stdout)),
        this.#observeChannel(this.#consumeOutput('stderr', this.#session.stderr)),
        this.#observeChannel(this.#consumeProcessEvents(this.#session.events)),
      ]
      for (const drain of channelDrains) drain.catch(() => undefined)
      if (this.#stopRequested) this.#requestStop()
      terminal = await this.#session.terminal
      if (!this.#stopRequested) this.#prematureOutcome = processOutcome(terminal)
      if (terminal.terminal === 'spawn-failed') {
        this.#spawnFailure = new Error(
          `direct child spawn failed (${terminal.errorCode}): ${terminal.errorMessage}`,
        )
      }
      let completionFailure: unknown
      try {
        await this.#session.completion
      } catch (cause) {
        completionFailure = cause
      }
      const drainOutcomes = await Promise.allSettled(channelDrains)
      const drainFailures = drainOutcomes.flatMap((outcome) =>
        outcome.status === 'rejected' ? [outcome.reason] : [],
      )
      if (completionFailure !== undefined || drainFailures.length !== 0) {
        const failures = [
          ...(completionFailure === undefined ? [] : [completionFailure]),
          ...drainFailures,
        ]
        throw failures.length === 1
          ? failures[0]
          : new AggregateError(failures, 'E2E direct child channels did not settle cleanly')
      }
    } catch (cause) {
      this.#recordBackendFailure(cause)
    }
    const outcome = processOutcome(terminal)
    this.#recordOutcome(outcome)
    return outcome
  }

  #prematureExitError(stream: 'stdout' | 'stderr', expression: RegExp): Error {
    return new FixtureInfrastructureError(
      `E2E child failed before ${expression} appeared in ${stream} ` +
      `(code=${String(this.#outcome?.code)}, signal=${String(this.#outcome?.signal)}). ` +
      this.#structuredDiagnostic(),
      this.#spawnFailure ?? this.#backendFailure,
    )
  }

  #rejectAfterDeadline(boundary: string, reject: (reason: unknown) => void): void {
    this.#stopRequested = true
    this.#requestStop()
    boundedSettlement(this.#exit, PROCESS_STOP_TIMEOUT_MILLISECONDS).then(
      (joined) => {
        reject(new FixtureInfrastructureError(
          `${boundary}. direct_child_joined=${String(joined)} ${this.#structuredDiagnostic()}`,
          this.#backendFailure ?? this.#spawnFailure,
        ))
      },
      (cause) => reject(new FixtureInfrastructureError(`${boundary}. direct child join failed`, cause)),
    )
  }

  #structuredDiagnostic(): string {
    return `backend=inherited-descendant stdout=${this.#diagnostic('stdout')} ` +
      `stderr=${this.#diagnostic('stderr')}`
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
  return reason instanceof Error && containsFixtureInfrastructureFailureInner(reason.cause, visited)
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

function processOutcome(terminal: InheritedChildTerminal | undefined): ProcessOutcome {
  if (terminal?.terminal === 'exited') return { code: terminal.exitCode, signal: null }
  if (terminal?.terminal === 'signaled') return { code: null, signal: terminal.signal }
  return { code: null, signal: null }
}

async function boundedSettlement(task: Promise<unknown>, timeoutMilliseconds: number): Promise<boolean> {
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

function boundedAppend(current: string, chunk: Uint8Array): string {
  const next = current + Buffer.from(chunk).toString('utf8')
  return next.length <= MAXIMUM_CAPTURED_OUTPUT_CHARACTERS
    ? next
    : next.slice(next.length - MAXIMUM_CAPTURED_OUTPUT_CHARACTERS)
}
