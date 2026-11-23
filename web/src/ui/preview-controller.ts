import {
  AUTOMATIC_PHOTO_TIMEOUT_MILLISECONDS,
  AutomaticPhotoDeferredError,
  isAutomaticPhotoCandidate,
} from '../preview/automatic-photo'
import type { CatalogEntry } from '../catalog/records'
import { isPositionLocalPreviewFailure } from '../preview/preview'
import type { BrowseDirectory, JoinedBrowserShare } from '../application/browser-share/gateway'
import type { FailureReporter } from '../application/diagnostics/receive-trace'
import type { PreviewTraceEvent } from '../application/receive/contracts'
import type { TraceSource } from '../diagnostics/runtime'
import { EMPTY_PREVIEW, type ReceiverSnapshot } from './model'
import {
  isAbortError,
  previewSnapshot,
  type ActivePreview,
} from './controller-state'

const MEDIA_FAILURE_MESSAGE = 'The browser could not decode this bounded media preview.'

type ReadyPreviewSnapshot = Extract<ReceiverSnapshot['preview'], { state: 'image' | 'video' }>

interface ScopedActivePreview extends ActivePreview {
  readonly intent: 'manual' | 'automatic-photo'
  automaticDeadline?: ReturnType<typeof setTimeout>
  pendingSeekId?: number
  pendingMedia?: ReadyPreviewSnapshot
  // Retain the complete accepted presentation instead of independent flags and
  // positions that could disagree about what is actually on the canvas.
  presented?: ReadyPreviewSnapshot
  localFailureReported: boolean
}

export interface PreviewControllerHost {
  snapshot(): ReceiverSnapshot
  publish(snapshot: ReceiverSnapshot): void
  publicError(error: unknown): string
  readonly recordFailure?: FailureReporter
  readonly trace?: TraceSource<PreviewTraceEvent>
}

export interface ActivePreviewTarget {
  readonly entry: Extract<CatalogEntry, { kind: 'file' }>
  readonly directory: BrowseDirectory
  readonly imageBlob?: Blob
}

export class PreviewController {
  readonly #host: PreviewControllerHost
  #active: ScopedActivePreview | undefined
  #target: Readonly<{ entry: Extract<CatalogEntry, { kind: 'file' }>; directory: BrowseDirectory }> | undefined
  #nextId = 1

  constructor(host: PreviewControllerHost) {
    this.#host = host
  }

  open(
    joined: JoinedBrowserShare,
    entry: Extract<CatalogEntry, { kind: 'file' }>,
    directory: BrowseDirectory,
  ): void {
    this.#open(joined, entry, directory, 'manual')
  }

  openAutomaticPhoto(
    joined: JoinedBrowserShare,
    entry: Extract<CatalogEntry, { kind: 'file' }>,
    directory: BrowseDirectory,
  ): void {
    if (this.#active !== undefined || !isAutomaticPhotoCandidate(entry)) return
    try {
      this.#open(joined, entry, directory, 'automatic-photo')
    } catch {
      // Optional media admission must not turn an authenticated directory
      // publication into a browse failure if connectivity was replaced.
      try { this.cancel() } catch { /* The browse owner still publishes its page. */ }
    }
  }

  activeTarget(): ActivePreviewTarget | undefined {
    const target = this.#target
    if (target === undefined) return undefined
    const imageBlob = this.#active?.entry.idText === target.entry.idText
      ? this.#active.session?.imageBlob
      : undefined
    return Object.freeze({
      entry: target.entry,
      directory: target.directory,
      ...(imageBlob === undefined ? {} : { imageBlob }),
    })
  }

  yieldToReceiving(): void {
    const active = this.#active
    // A decoded still owns no network lease; retain its URL while receiving.
    // Pending speculation yields synchronously, without awaiting remote cleanup.
    if (active?.intent !== 'automatic-photo' || active.session !== undefined) return
    this.cancel()
  }

  cancel(): void {
    this.#target = undefined
    void this.#closeActive()
    this.#publishPreview(EMPTY_PREVIEW)
  }

  seek(seconds: number): void {
    const active = this.#active
    const preview = this.#host.snapshot().preview
    if (active?.session === undefined || preview.state !== 'video') return

    const seekId = ++active.seekId
    active.pendingSeekId = seekId
    try {
      this.#publishPreview(Object.freeze({ ...preview, requestedSeconds: seconds, seeking: true, failure: null }))
    } catch (error) {
      delete active.pendingSeekId
      throw error
    }
    let started: ReturnType<NonNullable<ActivePreview['session']>['seek']>
    try {
      started = active.session.seek(seconds, active.controller.signal)
    } catch (error) {
      delete active.pendingSeekId
      throw error
    }
    Promise.resolve(started).then(
      (presentation) => {
        if (this.#active !== active || active.pendingSeekId !== seekId) return
        const projected = previewSnapshot(active.entry, presentation, this.#nextId++)
        const next = projected.state === 'video'
          ? Object.freeze({ ...projected, requestedSeconds: seconds })
          : projected
        active.pendingMedia = next
        try {
          this.#publishPreview(next)
        } catch (error) {
          delete active.pendingMedia
          delete active.pendingSeekId
          throw error
        }
        delete active.pendingSeekId
      },
      (error: unknown) => {
        if (this.#active !== active || active.pendingSeekId !== seekId) return
        delete active.pendingSeekId
        if (active.controller.signal.aborted || isAbortError(error)) return
        if (active.presented?.state === 'video' && isPositionLocalPreviewFailure(error)) {
          // Only this position failed; the session, its previous frame and the
          // download stay available, so the preview remains open.
          this.#reportLocalFailure(active, 'preview_seek', error)
          this.#publishVideoFailure(active, this.#host.publicError(error))
          return
        }
        this.#reportFailure('preview_seek', error)
        const failure = Object.freeze({
          state: 'error' as const,
          fileId: active.entry.idText,
          name: active.entry.name,
          message: this.#host.publicError(error),
        })
        void this.#closeActive()
        this.#publishPreview(failure)
      },
    )
  }

  mediaPresented(presentationId: number): void {
    const active = this.#active
    const preview = this.#host.snapshot().preview
    const media = active?.pendingMedia
    if (
      active === undefined ||
      media === undefined ||
      (preview.state !== 'image' && preview.state !== 'video') ||
      preview.presentationId !== presentationId ||
      media.presentationId !== presentationId
    ) {
      return
    }
    delete active.pendingMedia
    active.presented = media
    if (preview.state === 'video' && media.state === 'video' && preview.failure !== null &&
        preview.requestedSeconds !== media.requestedSeconds) {
      // A prior frame can arrive after a newer request failed. Its accepted
      // snapshot owns the rollback position, without changing the latest error.
      this.#publishPreview(Object.freeze({ ...preview, requestedSeconds: media.requestedSeconds }))
    }
  }

  mediaFailed(presentationId: number): void {
    const active = this.#active
    const preview = this.#host.snapshot().preview
    const media = active?.pendingMedia
    if (
      active === undefined ||
      media === undefined ||
      (preview.state !== 'image' && preview.state !== 'video') ||
      preview.presentationId !== presentationId ||
      media.presentationId !== presentationId
    ) {
      return
    }
    delete active.pendingMedia
    if (preview.state === 'video' && active.presented?.state === 'video') {
      // A later seek's frame did not present: keep the earlier frame on screen.
      this.#reportLocalFailure(active, 'preview_media')
      // A newer seek is still reading its range; avoid clearing seeking or rolling back under it.
      if (active.pendingSeekId !== undefined) return
      this.#publishVideoFailure(active, MEDIA_FAILURE_MESSAGE)
      return
    }
    this.#reportFailure('preview_media')
    const failure = Object.freeze({
      state: 'error' as const,
      fileId: active.entry.idText,
      name: active.entry.name,
      message: MEDIA_FAILURE_MESSAGE,
    })
    void this.#closeActive()
    this.#publishPreview(failure)
  }

  close(): Promise<void> {
    this.#target = undefined
    return this.#closeActive()
  }

  #open(
    joined: JoinedBrowserShare,
    entry: Extract<CatalogEntry, { kind: 'file' }>,
    directory: BrowseDirectory,
    intent: 'manual' | 'automatic-photo',
  ): void {
    // Connectivity must be the first post-guard action so the user's click
    // remains the activation boundary even when it replaces another preview.
    const connectivity = joined.beginPreviewConnectivity()
    void this.#closeActive()
    const active: ScopedActivePreview = {
      id: this.#nextId++,
      entry,
      controller: new AbortController(),
      connectivity,
      seekId: 0,
      intent,
      localFailureReported: false,
    }
    this.#active = active
    this.#target = Object.freeze({ entry, directory })
    this.#publishPreview(Object.freeze({
      state: 'loading',
      fileId: entry.idText,
      name: entry.name,
    }))
    if (this.#active !== active) return
    if (intent === 'automatic-photo') {
      active.automaticDeadline = setTimeout(() => {
        if (this.#active === active) this.yieldToReceiving()
      }, AUTOMATIC_PHOTO_TIMEOUT_MILLISECONDS)
    }
    this.#run(joined, active).catch(() => undefined)
  }

  async #run(joined: JoinedBrowserShare, active: ScopedActivePreview): Promise<void> {
    try {
      const session = await (active.intent === 'automatic-photo'
        ? joined.automaticPhotoPreview(active.entry, active.connectivity, active.controller.signal)
        : joined.preview(active.entry, active.connectivity, active.controller.signal))
      clearTimeout(active.automaticDeadline)
      if (this.#active !== active || active.controller.signal.aborted) {
        try { await session.close() } catch (error) { this.#reportFailure('cleanup', error) }
        return
      }
      active.session = session
      const next = previewSnapshot(active.entry, session.current, this.#nextId++)
      active.pendingMedia = next
      this.#publishPreview(next)
    } catch (error) {
      if (this.#active !== active || active.controller.signal.aborted) return
      if (active.intent === 'automatic-photo' && error instanceof AutomaticPhotoDeferredError) {
        this.#traceAutomaticDeferral(joined, active, error)
        this.cancel()
        return
      }
      this.#reportFailure('preview_open', error)
      void this.#closeActive()
      this.#publishPreview(Object.freeze({
        state: 'error',
        fileId: active.entry.idText,
        name: active.entry.name,
        message: this.#host.publicError(error),
      }))
    }
  }

  #traceAutomaticDeferral(
    joined: JoinedBrowserShare,
    active: ScopedActivePreview,
    error: AutomaticPhotoDeferredError,
  ): void {
    try {
      this.#host.trace?.current?.({
        name: 'preview_transition',
        transition: 'deferred',
        previewId: active.id,
        shareInstanceId: joined.descriptor.shareInstanceId,
        fileId: active.entry.idText,
        error,
      })
    } catch {
      // A diagnostic sink cannot turn optional presentation into a visible failure.
    }
  }

  #reportLocalFailure(active: ScopedActivePreview, stage: 'preview_seek' | 'preview_media', error?: unknown): void {
    // Scrubbing a damaged region must not flood diagnostics.
    if (active.localFailureReported) return
    active.localFailureReported = true
    this.#reportFailure(stage, error)
  }

  #reportFailure(stage: 'preview_open' | 'preview_seek' | 'preview_media' | 'cleanup', error?: unknown): void {
    try {
      this.#host.recordFailure?.({ stage, ...(error === undefined ? {} : { error }) })
    } catch {
      // Diagnostic reporting cannot alter visible preview results.
    }
  }

  #publishVideoFailure(active: ScopedActivePreview, failure: string): void {
    const preview = this.#host.snapshot().preview
    if (preview.state !== 'video' || active.presented?.state !== 'video') return
    const requestedSeconds = active.presented.requestedSeconds
    this.#publishPreview(Object.freeze({ ...preview, requestedSeconds, seeking: false, failure }))
  }

  #closeActive(): Promise<void> {
    const active = this.#active
    if (active === undefined) return Promise.resolve()
    this.#active = undefined
    clearTimeout(active.automaticDeadline)
    active.controller.abort(new DOMException('Preview closed', 'AbortError'))

    try {
      active.connectivity.close()
    } catch (error) {
      this.#reportFailure('cleanup', error)
      throw error
    }

    let closing: void | PromiseLike<void>
    try {
      closing = active.session?.close()
    } catch (error) {
      this.#reportFailure('cleanup', error)
      throw error
    }
    return Promise.resolve(closing).then(
      () => undefined,
      (error: unknown) => { this.#reportFailure('cleanup', error) },
    )
  }

  #publishPreview(preview: ReceiverSnapshot['preview']): void {
    this.#host.publish({ ...this.#host.snapshot(), preview })
  }
}
