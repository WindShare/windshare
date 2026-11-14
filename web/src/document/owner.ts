import type { ComponentType } from 'react'
import type { CapturedLocation } from '../ui/capability/location'

export interface DocumentRuntime {
  readonly View: ComponentType
  readonly Downloads: ComponentType<{
    readonly open: boolean
    readonly onOpenChange: (open: boolean) => void
    readonly entryLabel: string
    readonly entryDescription: string
  }>
  readonly surface: { subscribe(listener: () => void): () => void; getSnapshot(): boolean }
  openLocation(location: CapturedLocation): void
  ensureDownloads(): Promise<void>
  dispose(): Promise<void>
}

interface OpenIntent { readonly id: number; readonly location: CapturedLocation }
type RuntimeState =
  | { readonly kind: 'unloaded' }
  | { readonly kind: 'loading'; readonly attempt: number; readonly work: Promise<void>; readonly intent: OpenIntent | null }
  | { readonly kind: 'ready'; readonly runtime: DocumentRuntime; readonly stop: () => void }
  | { readonly kind: 'failed'; readonly attempt: number; readonly intent: OpenIntent | null }
  | { readonly kind: 'disposed' }

export interface DocumentSnapshot {
  readonly view: 'portal' | 'opening' | 'receiver'
  readonly loading: boolean
  readonly failed: boolean
  readonly downloadsOpen: boolean
  readonly runtime: DocumentRuntime | null
}

export interface DocumentOwnerOptions {
  readonly pageUrl: string
  readonly load: () => Promise<() => DocumentRuntime>
  readonly acceptDiagnostics: (location: CapturedLocation) => void
  readonly record: (event: Readonly<{ attempt: number; intentId: number | null; transition: 'loading' | 'ready' | 'failed' | 'disposed'; error?: unknown }>) => void
}

/** One document owns loading and pending intent across React remounts and bfcache. */
export class DocumentOwner {
  readonly #options: DocumentOwnerOptions
  readonly #listeners = new Set<() => void>()
  #state: RuntimeState = { kind: 'unloaded' }
  #attempt = 0
  #intentId = 0
  #downloadsOpen = false
  #snapshot: DocumentSnapshot = Object.freeze({
    view: 'portal', loading: false, failed: false, downloadsOpen: false, runtime: null,
  })

  constructor(options: DocumentOwnerOptions) { this.#options = options }
  readonly subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener)
    return () => { this.#listeners.delete(listener) }
  }
  readonly getSnapshot = (): DocumentSnapshot => this.#snapshot

  openLocation(location: CapturedLocation): void {
    if (this.#state.kind === 'disposed') return
    this.#options.acceptDiagnostics(location)
    if (location.capabilityInput === null) return
    const intent = Object.freeze({ id: ++this.#intentId, location })
    if (this.#state.kind === 'ready') this.#state.runtime.openLocation(location)
    else if (this.#state.kind === 'loading') {
      this.#state = { ...this.#state, intent }
      this.#publish()
    } else this.#start(intent)
  }

  submitKey(input: string): void {
    if (input.trim().length !== 0) this.openLocation({ capabilityInput: input.trim(), pageUrl: this.#options.pageUrl })
  }

  readonly setDownloadsOpen = (open: boolean): void => {
    if (this.#state.kind === 'disposed') return
    this.#downloadsOpen = open
    if (open) {
      if (this.#state.kind === 'ready') void this.#state.runtime.ensureDownloads()
      else if (this.#state.kind === 'unloaded') this.#start(null)
    }
    this.#publish()
  }

  retry(): void {
    if (this.#state.kind === 'failed') this.#start(this.#state.intent)
  }

  async dispose(): Promise<void> {
    const previous = this.#state
    if (previous.kind === 'disposed') return
    this.#state = { kind: 'disposed' }
    this.#listeners.clear()
    this.#record('disposed', null)
    if (previous.kind === 'ready') {
      previous.stop()
      await previous.runtime.dispose()
    }
  }

  #start(intent: OpenIntent | null): void {
    const attempt = ++this.#attempt
    // Install the ownership token before invoking fallible or reentrant loaders.
    const work = Promise.resolve().then(() => this.#options.load()).then(create => {
      const pending = this.#state
      if (pending.kind !== 'loading' || pending.attempt !== attempt) return
      const runtime = create()
      const current = this.#state
      if (current.kind !== 'loading' || current.attempt !== attempt) {
        void runtime.dispose().catch(() => undefined)
        return
      }
      let stop = () => undefined as void
      try {
        stop = runtime.surface.subscribe(() => this.#publish())
        if (current.intent !== null) runtime.openLocation(current.intent.location)
        this.#state = { kind: 'ready', runtime, stop }
      } catch (error) {
        stop()
        void runtime.dispose().catch(() => undefined)
        throw error
      }
      this.#record('ready', current.intent)
      if (this.#downloadsOpen) void runtime.ensureDownloads()
      this.#publish()
    }).catch((error: unknown) => {
      const pending = this.#state
      if (pending.kind !== 'loading' || pending.attempt !== attempt) return
      this.#state = { kind: 'failed', attempt, intent: pending.intent }
      this.#record('failed', pending.intent, error)
      this.#publish()
    })
    this.#state = { kind: 'loading', attempt, work, intent }
    this.#record('loading', intent)
    this.#publish()
  }

  #record(transition: Parameters<DocumentOwnerOptions['record']>[0]['transition'], intent: OpenIntent | null, error?: unknown): void {
    try { this.#options.record({ attempt: this.#attempt, intentId: intent?.id ?? null, transition, ...(error === undefined ? {} : { error }) }) } catch {
      // Diagnostic delivery cannot prevent a captured intent from reaching its owner.
    }
  }

  #publish(): void {
    const state = this.#state
    if (state.kind === 'disposed') return
    const runtime = state.kind === 'ready' ? state.runtime : null
    let view: DocumentSnapshot['view'] = 'portal'
    if ((state.kind === 'loading' || state.kind === 'failed') && state.intent !== null) view = 'opening'
    if (runtime?.surface.getSnapshot()) view = 'receiver'
    const next: DocumentSnapshot = Object.freeze({
      view, loading: state.kind === 'loading', failed: state.kind === 'failed',
      downloadsOpen: this.#downloadsOpen, runtime,
    })
    if (Object.keys(next).every(key => next[key as keyof DocumentSnapshot] === this.#snapshot[key as keyof DocumentSnapshot])) return
    this.#snapshot = next
    for (const listener of this.#listeners) listener()
  }
}
