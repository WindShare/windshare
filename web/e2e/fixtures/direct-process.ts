import { spawn, type ChildProcessByStdio } from 'node:child_process'
import { EventEmitter } from 'node:events'
import type { Readable } from 'node:stream'

import { formatDiagnosticValue } from '../../src/security/diagnostic-formatter'

const MAXIMUM_CAPTURED_OUTPUT_CHARACTERS = 256 * 1024
const DEFAULT_READINESS_TIMEOUT_MILLISECONDS = 20_000
const DEFAULT_STOP_TIMEOUT_MILLISECONDS = 5_000
const FORCE_STOP_TIMEOUT_MILLISECONDS = 2_000
// Readiness matches are retained only long enough to redact an error that was
// produced before a capability stream is consumed. Bounding both the number
// and aggregate size keeps a noisy child from turning that diagnostic aid into
// an unbounded secret buffer; overflow fails closed in the formatter.
const MAXIMUM_READINESS_VALUES = 32
const MAXIMUM_READINESS_VALUE_CHARACTERS = 16 * 1024

export type DirectProcessStream = 'stdout' | 'stderr'

/**
 * `capability` permits raw bytes only to the readiness matcher. Once the
 * caller consumes that readiness record, the bytes are erased and all public
 * snapshots contain a length marker. `safe` is explicit because stderr is
 * useful for owned listener diagnostics but must never be inferred by default.
 * `private` keeps a stream out of public diagnostics while retaining it for
 * readiness matching when needed.
 */
export type DirectProcessDisclosure = 'capability' | 'safe' | 'private'

export interface DirectProcessDisclosurePolicy {
  readonly stdout: DirectProcessDisclosure
  readonly stderr: DirectProcessDisclosure
}

export interface DirectProcessOptions {
  readonly cwd: string
  readonly environment?: NodeJS.ProcessEnv
  readonly operationId: string
  /** Required at every process construction site so a new process cannot opt into publication accidentally. */
  readonly disclosure: DirectProcessDisclosurePolicy
}

export interface DirectProcessDiagnosticSnapshot {
  readonly operationId: string
  readonly code: number | null | undefined
  readonly signal: NodeJS.Signals | null | undefined
  readonly spawnError: unknown
  readonly stdout: string
  readonly stderr: string
}

export interface DirectProcessDiagnosticInput {
  readonly operationId: string
  readonly code: number | null | undefined
  readonly signal: NodeJS.Signals | null | undefined
  readonly spawnError: unknown
  readonly stdout: string
  readonly stderr: string
  readonly stdoutCapturedCharacters: number
  readonly stderrCapturedCharacters: number
  readonly redactText?: (value: string) => string
}

interface DirectProcessOutcome {
  readonly code: number | null
  readonly signal: NodeJS.Signals | null
  readonly spawnError?: unknown
}

export class DirectProcess {
  readonly #child: ChildProcessByStdio<null, Readable, Readable>
  readonly #events = new EventEmitter()
  readonly #operationId: string
  readonly #disclosure: DirectProcessDisclosurePolicy
  readonly #completion: Promise<DirectProcessOutcome>
  #stdout = ''
  #stderr = ''
  #stdoutCapturedCharacters = 0
  #stderrCapturedCharacters = 0
  readonly #readinessValues: Record<DirectProcessStream, string[]> = {
    stdout: [],
    stderr: [],
  }
  readonly #readinessValueCharacters: Record<DirectProcessStream, number> = {
    stdout: 0,
    stderr: 0,
  }
  readonly #readinessCaptureOverflowed: Record<DirectProcessStream, boolean> = {
    stdout: false,
    stderr: false,
  }
  #stdoutReadinessConsumed = false
  #stderrReadinessConsumed = false
  #capabilityCaptureErased = false
  #spawnErrorSnapshot: unknown = null
  #outcome: DirectProcessOutcome | undefined
  #stopRequested = false

  constructor(command: string, arguments_: readonly string[], options: DirectProcessOptions) {
    this.#operationId = options.operationId
    this.#disclosure = validateDisclosurePolicy(options.disclosure)
    this.#child = spawn(command, arguments_, {
      cwd: options.cwd,
      env: options.environment ?? process.env,
      stdio: ['ignore', 'pipe', 'pipe'],
      windowsHide: true,
    })
    this.#child.stdout.on('data', (chunk: Buffer) => this.#append('stdout', chunk))
    this.#child.stderr.on('data', (chunk: Buffer) => this.#append('stderr', chunk))
    this.#completion = new Promise((resolveOutcome) => {
      let spawnErrorObserved = false
      this.#child.once('error', (error) => {
        spawnErrorObserved = true
        // Detach and redact the error at the event boundary. Keeping the raw
        // Error graph until a later cleanup call would let a post-readiness
        // consume erase the only values available to the redactor.
        this.#spawnErrorSnapshot = formatDiagnosticValue(error, {
          redactText: this.#diagnosticRedactor(),
        })
        this.#events.emit('changed')
      })
      this.#child.once('close', (code, signal) => {
        const outcome: DirectProcessOutcome = Object.freeze({
          code,
          signal,
          ...(!spawnErrorObserved
            ? {}
            : { spawnError: this.#spawnErrorSnapshot }),
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

  /**
   * Waits against the private bounded capture. A readiness match is the only
   * operation allowed to observe capability-bearing output.
   */
  async waitFor(
    stream: DirectProcessStream,
    expression: RegExp,
    options: { readonly signal?: AbortSignal; readonly timeoutMilliseconds?: number } = {},
  ): Promise<RegExpMatchArray> {
    const timeoutMilliseconds = options.timeoutMilliseconds ??
      DEFAULT_READINESS_TIMEOUT_MILLISECONDS
    requirePositiveTimeout(timeoutMilliseconds, 'process readiness')
    options.signal?.throwIfAborted()
    const match = () => this.#captured(stream).match(expression)
    const immediate = match()
    if (immediate !== null) {
      this.#rememberReadiness(stream, immediate)
      return immediate
    }
    return await new Promise<RegExpMatchArray>((resolveMatch, rejectMatch) => {
      const timeout = setTimeout(() => reject(new Error(
        `${this.#diagnosticLabel(this.#operationId)} timed out waiting for ` +
          `${this.#diagnosticLabel(String(expression))} in ${stream}; ` +
          this.diagnostic(),
      )), timeoutMilliseconds)
      const cleanup = () => {
        clearTimeout(timeout)
        this.#events.off('changed', inspect)
        options.signal?.removeEventListener('abort', aborted)
      }
      const reject = (reason: unknown) => {
        cleanup()
        // A timeout/early exit is terminal for readiness. Erasing here closes
        // the raw-capture window even when the caller never reaches `consume`.
        this.#eraseCapabilityCapture()
        rejectMatch(reason)
      }
      const aborted = () => reject(options.signal?.reason ?? new Error('process readiness aborted'))
      const inspect = () => {
        const found = match()
        if (found !== null) {
          this.#rememberReadiness(stream, found)
          cleanup()
          resolveMatch(found)
        } else if (this.#outcome !== undefined) {
          reject(new Error(
            `${this.#diagnosticLabel(this.#operationId)} exited before ` +
              `${this.#diagnosticLabel(String(expression))} appeared in ${stream}; ` +
              this.diagnostic(),
          ))
        }
      }
      this.#events.on('changed', inspect)
      options.signal?.addEventListener('abort', aborted, { once: true })
      inspect()
    })
  }

  /**
   * Explicitly ends readiness ownership for a stream. Capability streams are
   * erased rather than merely hidden, so later cleanup and attachment paths
   * cannot recover the matched bytes.
   */
  consumeReadiness(stream: DirectProcessStream = 'stdout'): void {
    if (this.#disclosure[stream] !== 'capability') {
      throw new Error(`${stream} is not configured as a capability readiness stream`)
    }
    this.#erase(stream)
  }

  /** Alias used by fixture callers that describe the operation as capture consumption. */
  consumeCapturedOutput(stream: DirectProcessStream = 'stdout'): void {
    this.consumeReadiness(stream)
  }

  /** Returns a detached, deeply immutable public diagnostic snapshot. */
  diagnosticSnapshot(): Readonly<DirectProcessDiagnosticSnapshot> {
    const outcome = this.#outcome
    // Copy the mutable capture state before handing it to the pure formatter.
    // A frozen input boundary makes the snapshot deterministic even if a child
    // emits another chunk immediately after this call returns.
    const input = Object.freeze({
      operationId: this.#operationId,
      code: outcome?.code,
      signal: outcome?.signal,
      spawnError: outcome?.spawnError ?? null,
      stdout: this.#stdout,
      stderr: this.#stderr,
      stdoutCapturedCharacters: this.#stdoutCapturedCharacters,
      stderrCapturedCharacters: this.#stderrCapturedCharacters,
      redactText: this.#diagnosticRedactor(),
    })
    return formatDirectProcessDiagnostic({
      ...input,
    }, this.#disclosure)
  }

  /** Serializes only the immutable snapshot; raw buffers never cross this boundary. */
  diagnostic(): string {
    return JSON.stringify(this.diagnosticSnapshot())
  }

  async stop(timeoutMilliseconds = DEFAULT_STOP_TIMEOUT_MILLISECONDS): Promise<void> {
    requirePositiveTimeout(timeoutMilliseconds, 'process stop')
    if (this.#stopRequested) {
      await this.#completion
      return
    }
    this.#stopRequested = true
    if (this.#outcome !== undefined) {
      throw new Error(`${this.#diagnosticLabel(this.#operationId)} exited before cleanup; ${this.diagnostic()}`)
    }
    this.#child.kill()
    if (await settlesWithin(this.#completion, timeoutMilliseconds)) return
    this.#child.kill('SIGKILL')
    if (!await settlesWithin(this.#completion, FORCE_STOP_TIMEOUT_MILLISECONDS)) {
      throw new Error(`${this.#diagnosticLabel(this.#operationId)} did not stop; ${this.diagnostic()}`)
    }
  }

  #captured(stream: DirectProcessStream): string {
    return stream === 'stdout' ? this.#stdout : this.#stderr
  }

  #diagnosticLabel(value: string): string {
    return this.#diagnosticRedactor()(value)
  }

  #append(stream: DirectProcessStream, chunk: Buffer): void {
    const text = chunk.toString('utf8')
    if (stream === 'stdout' && this.#stdoutReadinessConsumed) return
    if (stream === 'stderr' && this.#stderrReadinessConsumed) return
    const current = this.#captured(stream)
    const appended = current + text
    const bounded = appended.length <= MAXIMUM_CAPTURED_OUTPUT_CHARACTERS
      ? appended
      : appended.slice(-MAXIMUM_CAPTURED_OUTPUT_CHARACTERS)
    if (stream === 'stdout') {
      this.#stdout = bounded
      this.#stdoutCapturedCharacters = bounded.length
    } else {
      this.#stderr = bounded
      this.#stderrCapturedCharacters = bounded.length
    }
    this.#events.emit('changed')
  }

  #erase(stream: DirectProcessStream): void {
    if (stream === 'stdout') {
      this.#stdout = ''
      this.#stdoutReadinessConsumed = true
    } else {
      this.#stderr = ''
      this.#stderrReadinessConsumed = true
    }
    this.#readinessValues[stream].length = 0
    this.#readinessValueCharacters[stream] = 0
    this.#readinessCaptureOverflowed[stream] = false
    if (this.#disclosure[stream] === 'capability') this.#capabilityCaptureErased = true
  }

  #eraseCapabilityCapture(): void {
    if (this.#disclosure.stdout === 'capability') this.#erase('stdout')
    if (this.#disclosure.stderr === 'capability') this.#erase('stderr')
  }

  #rememberReadiness(stream: DirectProcessStream, match: RegExpMatchArray): void {
    if (this.#disclosure[stream] !== 'capability') return
    const values = [match[0] ?? '', ...match.slice(1).filter(
      (capture): capture is string => capture !== undefined,
    )]
    for (const value of values) {
      if (value.length === 0) continue
      if (
        value.length > MAXIMUM_READINESS_VALUE_CHARACTERS ||
        values.length > MAXIMUM_READINESS_VALUES ||
        this.#readinessValues[stream].length >= MAXIMUM_READINESS_VALUES ||
        this.#readinessValueCharacters[stream] + value.length > MAXIMUM_READINESS_VALUE_CHARACTERS
      ) {
        // Once the bounded redaction index is incomplete, suppress nested
        // process errors rather than risk publishing a value that fell out of
        // the index. The readiness matcher itself still returns its raw match
        // to the caller, which owns the explicit consume boundary.
        this.#readinessCaptureOverflowed[stream] = true
        continue
      }
      this.#readinessValues[stream].push(value)
      this.#readinessValueCharacters[stream] += value.length
    }
  }

  #diagnosticRedactor(): (value: string) => string {
    const capabilityStreams = (['stdout', 'stderr'] as const).filter(
      (stream) => this.#disclosure[stream] === 'capability',
    )
    if (capabilityStreams.length === 0) return (value) => value
    if (this.#capabilityCaptureErased || capabilityStreams.some(
      (stream) => this.#readinessCaptureOverflowed[stream],
    )) {
      return () => '[redacted capability diagnostic]'
    }
    const candidates = capabilityStreams.flatMap((stream) => [
      this.#captured(stream),
      ...this.#readinessValues[stream],
    ]).filter((candidate) => candidate.length > 0)
    return (value: string): string => {
      let result = value
      for (const candidate of candidates) {
        result = result.split(candidate).join('[redacted capability output]')
      }
      return result
    }
  }
}

/**
 * Purely formats a detached process snapshot. Keeping this outside the child
 * lifecycle makes recursive-error and disclosure behavior independently
 * testable and prevents a diagnostic consumer from reaching raw buffers.
 */
export function formatDirectProcessDiagnostic(
  input: DirectProcessDiagnosticInput,
  disclosure: DirectProcessDisclosurePolicy,
): Readonly<DirectProcessDiagnosticSnapshot> {
  const safeDisclosure = validateDisclosurePolicy(disclosure)
  // Detach the caller-owned record before reading it. In particular, a caller
  // may reuse a mutable diagnostic object while a nested Error is being walked;
  // the formatter must never observe a moving policy or capture buffer.
  const snapshotInput = Object.freeze({
    operationId: input.operationId,
    code: input.code,
    signal: input.signal,
    spawnError: input.spawnError,
    stdout: input.stdout,
    stderr: input.stderr,
    stdoutCapturedCharacters: input.stdoutCapturedCharacters,
    stderrCapturedCharacters: input.stderrCapturedCharacters,
    redactText: input.redactText,
  })
  let detachedSpawnError: unknown = null
  if (snapshotInput.spawnError !== null && snapshotInput.spawnError !== undefined) {
    detachedSpawnError = formatDiagnosticValue(
      snapshotInput.spawnError,
      snapshotInput.redactText === undefined
        ? {}
        : { redactText: snapshotInput.redactText },
    )
  }
  const snapshot: DirectProcessDiagnosticSnapshot = {
    operationId: snapshotInput.redactText?.(snapshotInput.operationId) ?? snapshotInput.operationId,
    code: snapshotInput.code,
    signal: snapshotInput.signal,
    spawnError: detachedSpawnError,
    stdout: disclosureText(
      safeDisclosure.stdout,
      snapshotInput.stdout,
      snapshotInput.stdoutCapturedCharacters,
      'stdout',
    ),
    stderr: disclosureText(
      safeDisclosure.stderr,
      snapshotInput.stderr,
      snapshotInput.stderrCapturedCharacters,
      'stderr',
    ),
  }
  return Object.freeze(snapshot)
}

function validateDisclosurePolicy(
  policy: DirectProcessDisclosurePolicy | undefined,
): DirectProcessDisclosurePolicy {
  if (policy === undefined || typeof policy !== 'object') {
    throw new TypeError('DirectProcess requires an explicit stdout/stderr disclosure policy')
  }
  if (!isDisclosure(policy.stdout) || !isDisclosure(policy.stderr)) {
    throw new TypeError('DirectProcess disclosure policy contains an unsupported stream mode')
  }
  return Object.freeze({ stdout: policy.stdout, stderr: policy.stderr })
}

function isDisclosure(value: unknown): value is DirectProcessDisclosure {
  return value === 'capability' || value === 'safe' || value === 'private'
}

function disclosureText(
  disclosure: DirectProcessDisclosure,
  captured: string,
  capturedCharacters: number,
  stream: DirectProcessStream,
): string {
  if (disclosure === 'safe') return captured
  if (disclosure === 'capability') {
    return `<redacted capability ${stream} output; ${capturedCharacters} characters captured>`
  }
  return `<private ${stream} output omitted; ${capturedCharacters} characters captured>`
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
