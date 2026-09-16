import type { DiagnosticsStatusV2 } from '../export/diagnostic-bundle-v2'
import type { DiagnosticsRuntimePort } from '../runtime'
import type { DiagnosticsEvidenceReadPort, DiagnosticsEvidenceSnapshot } from '../evidence'
import type { TraceActivationSnapshot } from '../trace/switch'
import { SYSTEM_TRACE_SCHEDULER, type TraceScheduledTask, type TraceScheduler } from '../trace/ports'
import {
  DIAGNOSTICS_ARCHIVE_MAX_AGE_MS,
  retainDiagnosticCaptures,
  type DiagnosticsArchiveStore,
  type SavedDiagnosticCapture,
  type DiagnosticCaptureSummary,
} from './archive'
import { createDiagnosticFile, type DiagnosticFile } from './file'

export const DIAGNOSTICS_CHECKPOINT_INTERVAL_MS = 5_000

export interface BrowserDiagnosticsSnapshot {
  readonly capture: DiagnosticsStatusV2
  readonly activation: TraceActivationSnapshot
  readonly hasCurrentEvidence: boolean
  readonly previous: DiagnosticCaptureSummary | null
  readonly savedAt: number | null
  readonly archiveUnavailable: boolean
}

export interface BrowserDiagnosticsSessionOptions {
  readonly runtime: DiagnosticsRuntimePort & DiagnosticsEvidenceReadPort
  readonly observeCapture: (listener: () => void) => () => void
  readonly archive: DiagnosticsArchiveStore
  readonly pageUrl: string
  readonly scheduler?: TraceScheduler
  readonly now?: () => number
}

interface PendingDiagnosticCapture {
  readonly capture: SavedDiagnosticCapture
  readonly evidence: DiagnosticsEvidenceSnapshot
}

/** Owns diagnostic evidence delivery independently of receiver/connection lifetime. */
export class BrowserDiagnosticsSession implements DiagnosticsRuntimePort {
  readonly #options: BrowserDiagnosticsSessionOptions
  readonly #scope: string
  readonly #runId: string
  readonly #now: () => number
  readonly #scheduler: TraceScheduler
  readonly #listeners = new Set<() => void>()
  readonly #stopObservation: () => void
  readonly #writes = new Map<string, PendingDiagnosticCapture | null>()
  #queuedEvidence: DiagnosticsEvidenceSnapshot | undefined
  #saved: readonly DiagnosticCaptureSummary[] = []
  #archiveRevision = 0
  #snapshot: BrowserDiagnosticsSnapshot
  #archiveUnavailable = false
  #draining: Promise<void> | undefined
  #timer: TraceScheduledTask | undefined
  #notificationQueued = false
  #disposed = false

  constructor(options: BrowserDiagnosticsSessionOptions) {
    this.#options = options
    this.#scope = new URL(options.pageUrl).pathname
    this.#now = options.now ?? Date.now
    this.#scheduler = options.scheduler ?? SYSTEM_TRACE_SCHEDULER
    this.#runId = options.runtime.runtimeRunId
    this.#snapshot = this.#readSnapshot()
    this.#stopObservation = options.observeCapture(() => this.#captureChanged())
    this.#loadArchive().catch(() => undefined)
    this.#schedule()
  }

  subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener)
    return () => { this.#listeners.delete(listener) }
  }

  getSnapshot = (): BrowserDiagnosticsSnapshot => this.#snapshot

  enable(): DiagnosticsStatusV2 {
    this.checkpoint().catch(() => undefined)
    const status = this.#options.runtime.enable()
    this.refresh()
    return status
  }

  enableFromLink(): void {
    // Repeated intake must not discard evidence or renew a restored deadline.
    if (this.activation().kind === 'off') this.enable()
  }

  disable(): DiagnosticsStatusV2 {
    const status = this.#options.runtime.disable()
    this.refresh()
    return status
  }

  get runtimeRunId(): string { return this.#runId }
  activation(): TraceActivationSnapshot { return this.#options.runtime.activation() }
  status(): DiagnosticsStatusV2 { return this.#options.runtime.status() }
  inspectLastFailure() { return this.#options.runtime.inspectLastFailure() }
  export(): string { return this.#options.runtime.export() }
  exportFile(): DiagnosticFile { return createDiagnosticFile(this.export()) }
  readSavedFile = (id: string): Promise<DiagnosticFile | null> => this.#options.archive.readFile(id)

  clear(): void {
    const id = this.#captureId(this.status())
    this.#queuedEvidence = undefined
    this.#options.runtime.clear()
    this.#archiveRevision++
    this.#saved = this.#saved.filter(capture => capture.id !== id)
    // A tombstone follows any in-flight write, so clear cannot resurrect old evidence.
    this.#writes.set(id, null)
    this.#drain().catch(() => undefined)
    this.refresh()
  }

  checkpoint(): Promise<void> {
    try {
      const evidence = this.#options.runtime.readEvidence()
      if (evidence.status.state !== 'idle' && evidence.hasEvidence && evidence !== this.#queuedEvidence) {
        const capture: SavedDiagnosticCapture = Object.freeze({
          id: this.#captureId(evidence.status),
          scope: this.#scope,
          savedAt: this.#now(),
          file: createDiagnosticFile(evidence.export()),
        })
        // Coalesce changed snapshots; identical evidence does not renew retention
        // or rebuild the file on a timer, visibility change, or repeated pagehide.
        this.#writes.set(capture.id, { capture, evidence })
        this.#queuedEvidence = evidence
      }
    } catch {
      this.#archiveUnavailable = true
      this.refresh()
    }
    return this.#drain()
  }

  dispose(): void {
    this.checkpoint().catch(() => undefined)
    this.#disposed = true
    this.#stopObservation()
    this.#timer?.cancel()
    this.#listeners.clear()
  }

  #captureId(status: DiagnosticsStatusV2): string {
    return `${this.#runId}.${status.capture_generation}`
  }

  async #loadArchive(): Promise<void> {
    try {
      const revision = this.#archiveRevision
      const saved = await this.#options.archive.list()
      if (revision === this.#archiveRevision) this.#saved = retainDiagnosticCaptures(saved, this.#now())
    } catch {
      this.#archiveUnavailable = true
    }
    this.refresh()
  }

  #drain(): Promise<void> {
    if (this.#draining !== undefined) return this.#draining
    if (this.#writes.size === 0) return Promise.resolve()
    this.#draining = this.#writePending().finally(() => {
      this.#draining = undefined
      if (this.#writes.size > 0) return this.#drain()
    })
    return this.#draining
  }

  async #writePending(): Promise<void> {
    for (const [id, pending] of this.#writes) {
      this.#writes.delete(id)
      try {
        const saved = pending === null
          ? await this.#options.archive.remove(id)
          : await this.#options.archive.save(pending.capture)
        this.#archiveRevision++
        this.#saved = retainDiagnosticCaptures(saved, this.#now())
        this.#archiveUnavailable = false
      } catch {
        // A failed write must stay retryable even when no new events arrive.
        if (pending?.evidence === this.#queuedEvidence) this.#queuedEvidence = undefined
        this.#archiveUnavailable = true
      }
      this.refresh()
    }
  }

  #captureChanged(): void {
    if (this.#notificationQueued || this.#disposed) return
    this.#notificationQueued = true
    queueMicrotask(() => {
      this.#notificationQueued = false
      if (this.#disposed) return
      this.refresh()
      // Seal can occur inside incident publication; snapshot after that turn finishes.
      this.checkpoint().catch(() => undefined)
    })
  }

  #readSnapshot(): BrowserDiagnosticsSnapshot {
    const saved = retainDiagnosticCaptures(this.#saved, this.#now())
    const evidence = this.#options.runtime.readEvidence()
    const id = this.#captureId(evidence.status)
    return Object.freeze({
      capture: evidence.status,
      activation: this.activation(),
      hasCurrentEvidence: evidence.hasEvidence,
      previous: saved.find(capture => capture.scope === this.#scope && capture.id !== id) ?? null,
      savedAt: saved.find(capture => capture.id === id)?.savedAt ?? null,
      archiveUnavailable: this.#archiveUnavailable,
    })
  }

  refresh(): void {
    this.#snapshot = this.#readSnapshot()
    for (const listener of this.#listeners) listener()
    this.#timer?.cancel()
    this.#timer = undefined
    this.#schedule()
  }

  #schedule(): void {
    if (this.#disposed || this.#timer !== undefined) return
    const previous = this.#snapshot.previous
    let delay: number | undefined
    if (this.#snapshot.capture.enabled) delay = DIAGNOSTICS_CHECKPOINT_INTERVAL_MS
    else if (previous !== null) delay = Math.max(1, previous.savedAt + DIAGNOSTICS_ARCHIVE_MAX_AGE_MS - this.#now())
    if (delay === undefined) return
    this.#timer = this.#scheduler.schedule(delay, () => {
      this.#timer = undefined
      this.refresh()
      if (this.status().enabled) this.checkpoint().catch(() => undefined)
    })
  }
}

export function observeDiagnosticsPage(
  windowPort: Pick<Window, 'addEventListener' | 'removeEventListener' | 'document'>,
  session: BrowserDiagnosticsSession,
): () => void {
  const hidden = () => {
    if (windowPort.document.visibilityState === 'hidden') session.checkpoint().catch(() => undefined)
  }
  const leaving = () => { session.checkpoint().catch(() => undefined) }
  windowPort.document.addEventListener('visibilitychange', hidden)
  windowPort.addEventListener('pagehide', leaving)
  return () => {
    windowPort.document.removeEventListener('visibilitychange', hidden)
    windowPort.removeEventListener('pagehide', leaving)
  }
}
