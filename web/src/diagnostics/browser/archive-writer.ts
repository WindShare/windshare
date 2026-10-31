import type { TraceScheduledTask, TraceScheduler } from '../trace/ports'
import {
  retainDiagnosticCaptures,
  summarizeDiagnosticCapture,
  type DiagnosticCaptureSummary,
  type DiagnosticsArchiveStore,
  type SavedDiagnosticCapture,
} from './archive'

export const DIAGNOSTICS_SAVE_MAX_ATTEMPTS = 3
const DIAGNOSTICS_SAVE_RETRY_MS = 1_000

export type DiagnosticArchiveWriteEvent =
  | Readonly<{ event: 'diagnostic_archive_write'; captureId: string; operation: 'save' | 'remove';
      attempt: number; outcome: 'saved' | 'failed'; error?: unknown }>
  | Readonly<{ event: 'diagnostic_archive_evicted'; captureId: string; savedAt: number; byteLength: number }>

interface DiagnosticsArchiveWriterOptions {
  readonly archive: Pick<DiagnosticsArchiveStore, 'save' | 'remove'>
  readonly now: () => number
  readonly scheduler: TraceScheduler
  readonly onSaved: (saved: readonly DiagnosticCaptureSummary[]) => void
  readonly onFailure: () => void
  readonly trace?: (event: DiagnosticArchiveWriteEvent) => void
}

interface PendingCaptureSave {
  readonly kind: 'save'
  readonly capture: SavedDiagnosticCapture
  readonly summary: DiagnosticCaptureSummary
  attempts: number
  retryTimer: TraceScheduledTask | undefined
}

type PendingArchiveWrite = PendingCaptureSave | Readonly<{ kind: 'remove'; id: string }>

/** Coalescing and retry ownership follow capture identity, independently of live recording. */
export class DiagnosticsArchiveWriter {
  readonly #options: DiagnosticsArchiveWriterOptions
  readonly #writes = new Map<string, PendingArchiveWrite>()
  #draining: Promise<void> | undefined
  #disposed = false

  constructor(options: DiagnosticsArchiveWriterOptions) { this.#options = options }

  save(capture: SavedDiagnosticCapture): void {
    const summary = summarizeDiagnosticCapture(capture)
    this.#replace(capture.id, { kind: 'save', capture, summary, attempts: 0, retryTimer: undefined })
    this.#prune()
  }

  remove(id: string): void {
    // Keep deletion ordered after an in-flight save, even when other captures are pruned.
    this.#replace(id, { kind: 'remove', id })
  }

  flush(): Promise<void> {
    // An explicit checkpoint can retry immediately without renewing any attempt budget.
    for (const write of this.#writes.values()) {
      if (write.kind === 'save') {
        write.retryTimer?.cancel()
        write.retryTimer = undefined
      }
    }
    return this.#drain()
  }

  dispose(): void {
    this.#disposed = true
    for (const [id, write] of this.#writes) {
      if (write.kind === 'save' && write.retryTimer !== undefined) {
        write.retryTimer.cancel()
        this.#writes.delete(id)
      }
    }
  }

  #replace(id: string, write: PendingArchiveWrite): void {
    const previous = this.#writes.get(id)
    if (previous?.kind === 'save') previous.retryTimer?.cancel()
    // Updating insertion order makes equal-timestamp admission prefer the newest snapshot.
    this.#writes.delete(id)
    this.#writes.set(id, write)
  }

  #prune(): void {
    const candidates = [...this.#writes.values()].filter(write => write.kind === 'save')
    // Pending bodies use the archive's count, byte, and age budgets. An already
    // executing write may keep one additional body alive until its I/O settles.
    const retained = new Set(retainDiagnosticCaptures(
      candidates.map(write => write.summary).reverse(), this.#options.now(),
    ).map(summary => summary.id))
    for (const write of candidates) {
      if (retained.has(write.capture.id)) continue
      write.retryTimer?.cancel()
      this.#writes.delete(write.capture.id)
      this.#trace({ event: 'diagnostic_archive_evicted', captureId: write.capture.id,
        savedAt: write.capture.savedAt, byteLength: write.summary.byteLength })
    }
  }

  #next(): PendingArchiveWrite | undefined {
    return [...this.#writes.values()].find(write => write.kind === 'remove' || write.retryTimer === undefined)
  }

  #drain(): Promise<void> {
    if (this.#draining !== undefined) return this.#draining
    if (this.#next() === undefined) return Promise.resolve()
    this.#draining = this.#writePending().finally(() => {
      this.#draining = undefined
      if (this.#next() !== undefined) return this.#drain()
    })
    return this.#draining
  }

  async #writePending(): Promise<void> {
    for (;;) {
      this.#prune()
      const write = this.#next()
      if (write === undefined) return
      await this.#write(write)
    }
  }

  async #write(write: PendingArchiveWrite): Promise<void> {
    const id = write.kind === 'save' ? write.capture.id : write.id
    if (write.kind === 'save') write.attempts++
    const attempt = write.kind === 'save' ? write.attempts : 1
    let saved: readonly DiagnosticCaptureSummary[]
    try {
      saved = write.kind === 'save'
        ? await this.#options.archive.save(write.capture)
        : await this.#options.archive.remove(id)
    } catch (error) {
      this.#retry(id, write)
      this.#trace({ event: 'diagnostic_archive_write', captureId: id,
        operation: write.kind, attempt, outcome: 'failed', error })
      this.#options.onFailure()
      return
    }
    if (this.#writes.get(id) === write) this.#writes.delete(id)
    this.#trace({ event: 'diagnostic_archive_write', captureId: id,
      operation: write.kind, attempt, outcome: 'saved' })
    this.#options.onSaved(saved)
  }

  #retry(id: string, write: PendingArchiveWrite): void {
    // Replacement, clear, or retention eviction revokes this exact snapshot's retry authority.
    if (this.#writes.get(id) !== write) return
    if (write.kind === 'remove' || this.#disposed || write.attempts >= DIAGNOSTICS_SAVE_MAX_ATTEMPTS) {
      this.#writes.delete(id)
      return
    }
    write.retryTimer = this.#options.scheduler.schedule(DIAGNOSTICS_SAVE_RETRY_MS * write.attempts, () => {
      if (this.#disposed || this.#writes.get(id) !== write) return
      write.retryTimer = undefined
      this.#drain().catch(() => undefined)
    })
  }

  #trace(event: DiagnosticArchiveWriteEvent): void {
    try {
      if (this.#options.trace !== undefined) this.#options.trace(event)
      else console.debug('[windshare:diagnostics:archive]', event)
    } catch {
      // Reporting archive failures must never interrupt writes or recursively create evidence.
    }
  }
}
