import { encodeBase64Url } from '../crypto/bytes'
import { formatDiagnosticValue } from '../security/diagnostic-formatter'
import type { TraceActivationStore } from './activation'
import { browserBuildIdentity, type BuildIdentity, type BuildSnapshot } from './build-identity'
import { createDiagnosticFile, type DiagnosticFile } from './file'
import { DiagnosticRingBuffer } from './ring-buffer'

export const DIAGNOSTIC_BUNDLE_SCHEMA_VERSION = 3 as const
export const DEFAULT_TRACE_EXPIRY_MS = 30 * 60 * 1_000
export const MAX_TRACE_EVENT_COUNT = 4_096
export const MAX_TRACE_EVENT_BYTES = 16_384
export const MAX_TRACE_TOTAL_BYTES = 4 * 1024 * 1024
export const MAX_FAILURE_COUNT = 128
export const MAX_FAILURE_EVENT_BYTES = 16_384
export const MAX_FAILURE_TOTAL_BYTES = 512 * 1024

const RUNTIME_RUN_ID_BYTES = 16
const FAILURE_FORMAT_OPTIONS = Object.freeze({
  maxDepth: 6,
  maxEntries: 16,
  maxStringCharacters: 512,
})
const TEXT_ENCODER = new TextEncoder()

export type TraceCaptureState = 'idle' | 'recording' | 'sealed'
export type TraceSealReason = 'manual_disable' | 'expired'

export type TraceActivationSnapshot =
  | Readonly<{ kind: 'off' }>
  | Readonly<{ kind: 'active'; expiresAtMilliseconds: number }>

export interface TraceEntry {
  readonly event: string
  readonly correlation?: Readonly<Record<string, unknown>>
  readonly payload?: Readonly<Record<string, unknown>>
}

export type TraceObserver<Event = TraceEntry> = (event: Event) => void

/** Producers check `current` before allocating trace-only objects on hot paths. */
export interface TraceSource<Event = TraceEntry> {
  readonly current: TraceObserver<Event> | undefined
}

export interface DiagnosticFailureInput {
  readonly stage: string
  readonly operation?: string
  readonly error?: unknown
  readonly correlation?: Readonly<Record<string, unknown>>
  readonly context?: Readonly<Record<string, unknown>>
  readonly consoleReport?: boolean
}

export interface DiagnosticFailureRecord {
  readonly line_type: 'failure'
  readonly sequence: string
  readonly time: string
  readonly elapsed_ms: string
  readonly runtime_run_id: string
  readonly stage: string
  readonly operation?: string
  readonly correlation?: Readonly<Record<string, unknown>>
  readonly error?: unknown
  readonly context?: Readonly<Record<string, unknown>>
}

export interface DiagnosticsStatus {
  readonly schema_version: typeof DIAGNOSTIC_BUNDLE_SCHEMA_VERSION
  readonly state: TraceCaptureState
  readonly enabled: boolean
  readonly capture_generation: string
  readonly expires_at?: string
  readonly seal_reason?: TraceSealReason
  readonly failure_count: string
  readonly retained_event_count: string
  readonly retained_event_bytes: string
  readonly overwritten_count: string
  readonly dropped_count: string
}

export interface DiagnosticsSnapshot {
  readonly status: DiagnosticsStatus
  readonly activation: TraceActivationSnapshot
  readonly hasEvidence: boolean
}

export interface DiagnosticsClock {
  nowMilliseconds(): number
  captureTime(): string
}

export interface DiagnosticsScheduledTask {
  cancel(): void
}

export interface DiagnosticsScheduler {
  schedule(delayMilliseconds: number, callback: () => void): DiagnosticsScheduledTask
}

export interface DiagnosticsConsoleSink {
  error(record: DiagnosticFailureRecord): void
}

export interface DiagnosticsRuntimeOptions {
  readonly build?: BuildSnapshot
  readonly secureContext?: boolean
  readonly activationStore?: TraceActivationStore
  readonly consoleSink?: DiagnosticsConsoleSink
  readonly clock?: DiagnosticsClock
  readonly scheduler?: DiagnosticsScheduler
  readonly randomBytes?: (length: number) => Uint8Array
  readonly traceExpiryMs?: number
  readonly maxTraceEventCount?: number
  readonly maxTraceEventBytes?: number
  readonly maxTraceTotalBytes?: number
}

export const SYSTEM_DIAGNOSTICS_CLOCK: DiagnosticsClock = Object.freeze({
  nowMilliseconds: () => Date.now(),
  captureTime: () => new Date().toISOString(),
})

export const SYSTEM_DIAGNOSTICS_SCHEDULER: DiagnosticsScheduler = Object.freeze({
  schedule: (delayMilliseconds: number, callback: () => void): DiagnosticsScheduledTask => {
    const timer = globalThis.setTimeout(callback, delayMilliseconds)
    return Object.freeze({ cancel: () => globalThis.clearTimeout(timer) })
  },
})

interface StoredFailure {
  readonly record: DiagnosticFailureRecord
  readonly line: string
}

interface ActiveTrace {
  readonly generation: number
  readonly startedAtMs: number
  readonly ring: DiagnosticRingBuffer<string>
  readonly observer: TraceObserver<TraceEntry>
  state: 'recording' | 'sealed'
  sealReason?: TraceSealReason
  nextSequence: number
}

interface ActiveExpiry {
  readonly expiresAtMilliseconds: number
  task?: DiagnosticsScheduledTask
}

/**
 * In-memory page-local diagnostics with two tiers:
 * 1. An always-on bounded failure ring so unexpected errors remain exportable without reproduction.
 * 2. An opt-in O(1) trace ring enabled via UI, `?trace=1`, or `window.windshareDiagnostics`.
 */
export class DiagnosticsRuntime {
  readonly runtimeRunId: string
  readonly trace: TraceSource<TraceEntry>
  readonly #build: BuildIdentity
  readonly #secureContext: boolean
  readonly #clock: DiagnosticsClock
  readonly #scheduler: DiagnosticsScheduler
  readonly #activationStore: TraceActivationStore | undefined
  readonly #consoleSink: DiagnosticsConsoleSink | undefined
  readonly #traceExpiryMs: number
  readonly #traceCapacity: Readonly<{ maxCount: number; maxEventBytes: number; maxTotalBytes: number }>
  readonly #startedAtMs: number
  readonly #failures = new DiagnosticRingBuffer<StoredFailure>({
    maxCount: MAX_FAILURE_COUNT,
    maxEventBytes: MAX_FAILURE_EVENT_BYTES,
    maxTotalBytes: MAX_FAILURE_TOTAL_BYTES,
  })
  readonly #listeners = new Set<() => void>()
  #failureSequence = 0
  #captureGeneration = 0
  #active: ActiveTrace | undefined
  #expiry: ActiveExpiry | undefined
  #snapshot: DiagnosticsSnapshot

  constructor(options: DiagnosticsRuntimeOptions = {}) {
    this.#build = browserBuildIdentity(options.build)
    this.#secureContext = options.secureContext ?? globalThis.isSecureContext
    this.#clock = options.clock ?? SYSTEM_DIAGNOSTICS_CLOCK
    this.#scheduler = options.scheduler ?? SYSTEM_DIAGNOSTICS_SCHEDULER
    this.#activationStore = options.activationStore
    this.#consoleSink = options.consoleSink
    this.#traceExpiryMs = options.traceExpiryMs ?? DEFAULT_TRACE_EXPIRY_MS
    this.#traceCapacity = Object.freeze({
      maxCount: options.maxTraceEventCount ?? MAX_TRACE_EVENT_COUNT,
      maxEventBytes: options.maxTraceEventBytes ?? MAX_TRACE_EVENT_BYTES,
      maxTotalBytes: options.maxTraceTotalBytes ?? MAX_TRACE_TOTAL_BYTES,
    })
    this.runtimeRunId = encodeBase64Url(createRunIdBytes(options.randomBytes))
    this.#startedAtMs = this.#safeNow()
    const currentObserver = (): TraceObserver<TraceEntry> | undefined =>
      this.#active?.state === 'recording' ? this.#active.observer : undefined
    this.trace = Object.freeze({
      get current(): TraceObserver<TraceEntry> | undefined {
        return currentObserver()
      },
    })
    this.#restoreActivation()
    this.#snapshot = this.#buildSnapshot()
  }

  subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener)
    return () => { this.#listeners.delete(listener) }
  }

  getSnapshot = (): DiagnosticsSnapshot => this.#snapshot

  refresh(): void {
    this.#publishSnapshot()
  }

  activation(): TraceActivationSnapshot {
    return this.#expiry === undefined
      ? Object.freeze({ kind: 'off' })
      : Object.freeze({ kind: 'active', expiresAtMilliseconds: this.#expiry.expiresAtMilliseconds })
  }

  enable(): DiagnosticsStatus {
    const now = this.#safeNow()
    const expiresAt = Math.min(Number.MAX_SAFE_INTEGER, now + this.#traceExpiryMs)
    this.#startCapture(now, expiresAt)
    try {
      if (this.#active?.state === 'recording') this.#activationStore?.writeExpiry(expiresAt)
      else this.#activationStore?.clear()
    } catch {
      // Denied storage must not prevent in-memory recording in the current tab.
    }
    this.#publishSnapshot()
    return this.status()
  }

  enableFromLink(): void {
    // Repeated link intake must not discard evidence or extend an existing deadline.
    if (this.activation().kind === 'off') this.enable()
  }

  disable(): DiagnosticsStatus {
    this.#cancelExpiry()
    try { this.#activationStore?.clear() } catch {
      // Revocation of in-memory recording is independent of browser storage.
    }
    if (this.#active?.state === 'recording') {
      this.#active.state = 'sealed'
      this.#active.sealReason = 'manual_disable'
    }
    this.#publishSnapshot()
    return this.status()
  }

  clear(): void {
    this.#failures.clear()
    if (this.#active !== undefined) {
      if (this.#active.state === 'recording') this.#active.ring.clear()
      else this.#active = undefined
    }
    this.#publishSnapshot()
  }

  lastFailure(): DiagnosticFailureRecord | null {
    return this.#failures.last()?.record ?? null
  }

  recordFailure(input: DiagnosticFailureInput): DiagnosticFailureRecord | undefined {
    try {
      const now = this.#safeNow()
      const record: DiagnosticFailureRecord = Object.freeze({
        line_type: 'failure' as const,
        sequence: String(++this.#failureSequence),
        time: this.#safeTime(),
        elapsed_ms: String(Math.max(0, now - this.#startedAtMs)),
        runtime_run_id: this.runtimeRunId,
        stage: input.stage,
        ...(input.operation === undefined ? {} : { operation: input.operation }),
        ...(input.correlation === undefined ? {} : { correlation: input.correlation }),
        ...(input.error === undefined ? {} : { error: formatDiagnosticValue(input.error, FAILURE_FORMAT_OPTIONS) }),
        ...(input.context === undefined ? {} : {
          context: formatDiagnosticValue(input.context, FAILURE_FORMAT_OPTIONS) as Readonly<Record<string, unknown>>,
        }),
      })
      const line = encodeJsonLine(record)
      if (line !== undefined) this.#failures.push({ record, line: line.text }, line.bytes)
      if (input.consoleReport) {
        try { this.#consoleSink?.error(record) } catch { /* Console output is passive. */ }
      }
      this.#publishSnapshot()
      return record
    } catch {
      // Failure recording is strictly observational and must never throw into callers.
      return undefined
    }
  }

  status(): DiagnosticsStatus {
    const active = this.#active
    const recording = active?.state === 'recording'
    return Object.freeze({
      schema_version: DIAGNOSTIC_BUNDLE_SCHEMA_VERSION,
      state: active?.state ?? 'idle',
      enabled: recording,
      capture_generation: String(this.#captureGeneration),
      ...(recording && this.#expiry !== undefined
        ? { expires_at: new Date(this.#expiry.expiresAtMilliseconds).toISOString() }
        : {}),
      ...(active?.sealReason === undefined ? {} : { seal_reason: active.sealReason }),
      failure_count: String(this.#failures.size),
      retained_event_count: String(active?.ring.size ?? 0),
      retained_event_bytes: String(active?.ring.bytes ?? 0),
      overwritten_count: String(active?.ring.overwritten ?? 0),
      dropped_count: String(active?.ring.dropped ?? 0),
    })
  }

  export(): string {
    const status = this.status()
    const header = JSON.stringify({
      line_type: 'bundle_header',
      schema_version: DIAGNOSTIC_BUNDLE_SCHEMA_VERSION,
      build: this.#build,
      runtime: { kind: 'browser', secure_context: this.#secureContext },
      runtime_run_id: this.runtimeRunId,
      time: this.#safeTime(),
      status,
    })
    const lines: string[] = [header]
    for (const stored of this.#failures.entries()) lines.push(stored.line)
    if (this.#active !== undefined) {
      lines.push(JSON.stringify({ line_type: 'trace_capture', status }))
      for (const line of this.#active.ring.entries()) lines.push(line)
    }
    return lines.join('\n') + '\n'
  }

  exportFile(): DiagnosticFile {
    return createDiagnosticFile(this.export())
  }

  dispose(): void {
    this.#cancelExpiry()
    this.#listeners.clear()
  }

  #startCapture(now: number, expiresAt: number): void {
    this.#cancelExpiry()
    if (this.#active?.state === 'recording') {
      this.#active.state = 'sealed'
      this.#active.sealReason = 'manual_disable'
    }
    this.#captureGeneration += 1
    const ring = new DiagnosticRingBuffer<string>(this.#traceCapacity)
    const active: ActiveTrace = {
      generation: this.#captureGeneration,
      startedAtMs: now,
      ring,
      state: 'recording',
      nextSequence: 0,
      observer: entry => this.#recordTrace(active, entry),
    }
    this.#active = active
    const expiry: ActiveExpiry = { expiresAtMilliseconds: expiresAt }
    this.#expiry = expiry
    try {
      expiry.task = this.#scheduler.schedule(Math.max(0, expiresAt - now), () => this.#expire(expiry, active))
    } catch {
      this.#cancelExpiry()
      active.state = 'sealed'
      active.sealReason = 'expired'
    }
  }

  #recordTrace(active: ActiveTrace, entry: TraceEntry): void {
    if (active.state !== 'recording') return
    try {
      const hadEvidence = this.#snapshot.hasEvidence
      const now = this.#safeNow()
      const line = encodeJsonLine({
        line_type: 'trace_event',
        record: {
          sequence: String(++active.nextSequence),
          time: this.#safeTime(),
          elapsed_ms: String(Math.max(0, now - active.startedAtMs)),
          event: entry.event,
          runtime_run_id: this.runtimeRunId,
          ...(entry.correlation === undefined ? {} : { correlation: entry.correlation }),
          payload: entry.payload ?? {},
        },
      })
      if (line === undefined) return
      if (active.ring.push(line.text, line.bytes) && !hadEvidence) {
        // Notify React only when evidence transitions from empty to non-empty, not per hot-path event.
        this.#publishSnapshot()
      }
    } catch {
      // Trace recording cannot interfere with product execution.
    }
  }

  #expire(expiry: ActiveExpiry, active: ActiveTrace): void {
    if (this.#expiry !== expiry) return
    this.#cancelExpiry()
    try {
      if (this.#activationStore?.readExpiry() === expiry.expiresAtMilliseconds) {
        this.#activationStore.clear()
      }
    } catch {
      // Storage cleanup is best-effort.
    }
    if (active.state === 'recording') {
      active.state = 'sealed'
      active.sealReason = 'expired'
    }
    this.#publishSnapshot()
  }

  #restoreActivation(): void {
    try {
      const expiresAt = this.#activationStore?.readExpiry()
      if (expiresAt === undefined) return
      const now = this.#safeNow()
      if (!Number.isSafeInteger(expiresAt) || expiresAt <= now || expiresAt - now > this.#traceExpiryMs) {
        this.#activationStore?.clear()
        return
      }
      this.#startCapture(now, expiresAt)
      if (this.#active?.state !== 'recording') this.#activationStore?.clear()
    } catch {
      // Blocked or corrupted storage must never interfere with startup.
    }
  }

  #cancelExpiry(): void {
    const current = this.#expiry
    this.#expiry = undefined
    try { current?.task?.cancel() } catch { /* Ignore timer cancellation errors. */ }
  }

  #buildSnapshot(): DiagnosticsSnapshot {
    const status = this.status()
    return Object.freeze({
      status,
      activation: this.activation(),
      hasEvidence: this.#failures.size > 0 || (this.#active?.ring.size ?? 0) > 0,
    })
  }

  #publishSnapshot(): void {
    this.#snapshot = this.#buildSnapshot()
    for (const listener of this.#listeners) {
      try { listener() } catch { /* UI listener errors cannot affect diagnostics. */ }
    }
  }

  #safeNow(): number {
    try {
      const value = this.#clock.nowMilliseconds()
      return Number.isSafeInteger(value) && value >= 0 ? value : 0
    } catch {
      return 0
    }
  }

  #safeTime(): string {
    try { return this.#clock.captureTime() } catch { return new Date(0).toISOString() }
  }
}

export function createDiagnosticsRuntime(options?: DiagnosticsRuntimeOptions): DiagnosticsRuntime {
  return new DiagnosticsRuntime(options)
}

function encodeJsonLine(value: unknown): Readonly<{ text: string; bytes: number }> | undefined {
  const text = JSON.stringify(value, (_key, item: unknown) =>
    typeof item === 'bigint' ? item.toString(10) : item)
  if (text === undefined) return undefined
  return { text, bytes: TEXT_ENCODER.encode(text).byteLength + 1 }
}

function createRunIdBytes(randomBytes?: (length: number) => Uint8Array): Uint8Array {
  if (randomBytes !== undefined) {
    const bytes = randomBytes(RUNTIME_RUN_ID_BYTES).slice()
    if (bytes.byteLength !== RUNTIME_RUN_ID_BYTES) {
      throw new RangeError('Runtime run identity requires 16 bytes')
    }
    return bytes
  }
  const bytes = new Uint8Array(RUNTIME_RUN_ID_BYTES)
  globalThis.crypto.getRandomValues(bytes)
  return bytes
}
