import { isAutomaticPhotoCandidate, AutomaticPhotoDeferredError, AUTOMATIC_PHOTO_TIMEOUT_MILLISECONDS } from '../preview/automatic-photo'
import type { CatalogEntry } from '../catalog/records'
import { RemoteOperationError } from '../content/session-operations'
import { RemoteRevisionError } from '../content/session-services'
import { VideoPreviewError } from '../preview/mp4-range'
import type { PreviewPresentation } from '../preview/preview'
import { unclassifiedFailureFact, type FailureFact } from '../diagnostics/incident/fact'
import {
  excludedPresentationDecision,
  incidentPresentationDecision,
  type PresentationDecision,
  type PresentationExclusionReason,
} from '../diagnostics/incident/presentation'
import type {
  IncidentScopeHandle,
  IncidentScopeOwner,
} from '../diagnostics/incident/scope'
import type { JoinedBrowserShare } from '../application/browser-share/gateway'
import { EMPTY_PREVIEW, type ReceiverSnapshot } from './model'
import {
  isAbortError,
  previewSnapshot,
  type ActivePreview,
} from './controller-state'

type PreviewScopeKind = 'preview_open' | 'preview_seek' | 'preview_media'

const MEDIA_FAILURE_MESSAGE = 'The browser could not decode this bounded media preview.'

interface PreviewIncidentPort {
  openScope(kind: PreviewScopeKind): IncidentScopeOwner
  submitDecision(scope: IncidentScopeHandle, decision: PresentationDecision): void
}

interface PreviewAttempt {
  readonly scope?: IncidentScopeOwner
  decisionSettled: boolean
}

interface PendingPreviewSeek {
  readonly id: number
  readonly attempt: PreviewAttempt
}

interface PendingMediaPresentation {
  readonly id: number
  readonly url: string
  readonly positionSeconds: number | undefined
  readonly attempt: PreviewAttempt
}

interface ScopedActivePreview extends ActivePreview {
  readonly intent: 'manual' | 'automatic-photo'
  readonly openAttempt: PreviewAttempt
  automaticDeadline?: ReturnType<typeof setTimeout>
  seekAttempt?: PendingPreviewSeek
  mediaAttempt?: PendingMediaPresentation
  incidentAttempt?: PreviewAttempt
  framePresented?: boolean
  // The slider position of the frame currently on the canvas.
  presentedPositionSeconds?: number
}

export interface PreviewControllerHost {
  snapshot(): ReceiverSnapshot
  publish(snapshot: ReceiverSnapshot): void
  publicError(error: unknown): string
  readonly incidents?: PreviewIncidentPort
}

export class PreviewController {
  readonly #host: PreviewControllerHost
  #active: ScopedActivePreview | undefined
  #nextId = 1

  constructor(host: PreviewControllerHost) {
    this.#host = host
  }

  open(
    joined: JoinedBrowserShare,
    entry: Extract<CatalogEntry, { kind: 'file' }>,
  ): void {
    this.#open(joined, entry, 'manual')
  }

  openAutomaticPhoto(
    joined: JoinedBrowserShare,
    entry: Extract<CatalogEntry, { kind: 'file' }>,
  ): void {
    if (this.#active !== undefined || !isAutomaticPhotoCandidate(entry)) return
    try {
      this.#open(joined, entry, 'automatic-photo')
    } catch {
      // Optional media admission must not turn an authenticated directory
      // publication into a browse failure if connectivity was replaced.
      try { this.cancel() } catch { /* The browse owner still publishes its page. */ }
    }
  }

  yieldToReceiving(): void {
    const active = this.#active
    // A decoded still owns no network lease; retain its URL while receiving.
    // Pending speculation yields synchronously, without awaiting remote cleanup.
    if (active?.intent !== 'automatic-photo' || active.session !== undefined) return
    this.cancel()
  }

  #open(
    joined: JoinedBrowserShare,
    entry: Extract<CatalogEntry, { kind: 'file' }>,
    intent: 'manual' | 'automatic-photo',
  ): void {
    // Connectivity must be the first post-guard action so the user's click
    // remains the activation boundary even when it replaces another preview.
    const connectivity = joined.beginPreviewConnectivity()
    this.#closeActive('stale_replacement')
    const active: ScopedActivePreview = {
      id: this.#nextId++,
      entry,
      controller: new AbortController(),
      connectivity,
      seekId: 0,
      intent,
      openAttempt: this.#newAttempt('preview_open'),
    }
    this.#active = active
    try {
      this.#publishPreview(Object.freeze({
        state: 'loading',
        fileId: entry.idText,
        name: entry.name,
      }))
    } catch (error) {
      this.#exclude(active.openAttempt, 'not_user_visible')
      this.#closeAllAttempts(active)
      throw error
    }
    if (this.#active !== active) return
    if (intent === 'automatic-photo') {
      active.automaticDeadline = setTimeout(() => {
        if (this.#active === active) this.yieldToReceiving()
      }, AUTOMATIC_PHOTO_TIMEOUT_MILLISECONDS)
    }
    this.#run(joined, active).catch(() => undefined)
  }

  cancel(): void {
    this.#closeActive('cancelled')
    this.#publishPreview(EMPTY_PREVIEW)
  }

  seek(seconds: number): void {
    const active = this.#active
    const preview = this.#host.snapshot().preview
    if (active?.session === undefined || preview.state !== 'video') return

    const replaced = active.seekAttempt
    if (replaced !== undefined) {
      this.#exclude(replaced.attempt, 'stale_replacement')
      this.#closeAttempt(replaced.attempt)
    }

    const pending: PendingPreviewSeek = {
      id: ++active.seekId,
      attempt: this.#newAttempt('preview_seek'),
    }
    active.seekAttempt = pending
    try {
      this.#publishPreview(Object.freeze({ ...preview, seeking: true, failure: null }))
    } catch (error) {
      this.#exclude(pending.attempt, 'not_user_visible')
      this.#closeAttempt(pending.attempt)
      delete active.seekAttempt
      throw error
    }
    let started: ReturnType<NonNullable<ActivePreview['session']>['seek']>
    try {
      started = active.session.seek(seconds, active.controller.signal)
    } catch (error) {
      this.#exclude(pending.attempt, 'not_user_visible')
      this.#closeAttempt(pending.attempt)
      delete active.seekAttempt
      throw error
    }
    Promise.resolve(started).then(
      (presentation) => {
        if (this.#active !== active || active.seekAttempt !== pending) {
          this.#exclude(pending.attempt, 'stale_replacement')
          this.#closeAttempt(pending.attempt)
          return
        }
        const media = this.#startMediaAttempt(active, presentation.url, videoPosition(presentation))
        const projected = previewSnapshot(active.entry, presentation, false, media.id)
        try {
          this.#publishPreview(projected)
        } catch (error) {
          this.#exclude(pending.attempt, 'not_user_visible')
          this.#closeAttempt(pending.attempt)
          this.#exclude(media.attempt, 'not_user_visible')
          this.#closeAttempt(media.attempt)
          delete active.mediaAttempt
          delete active.seekAttempt
          throw error
        }
        this.#exclude(pending.attempt, 'success')
        this.#closeAttempt(pending.attempt)
        delete active.seekAttempt
      },
      (error: unknown) => {
        if (this.#active !== active || active.seekAttempt !== pending) {
          this.#exclude(pending.attempt, 'stale_replacement')
          this.#closeAttempt(pending.attempt)
          return
        }
        if (active.controller.signal.aborted) {
          this.#exclude(pending.attempt, 'cancelled')
          this.#closeAttempt(pending.attempt)
          delete active.seekAttempt
          return
        }
        if (isAbortError(error)) {
          // Product behavior currently treats this as invisible; diagnostics
          // records that presentation decision without calling it cancellation.
          this.#exclude(pending.attempt, 'not_user_visible')
          this.#closeAttempt(pending.attempt)
          delete active.seekAttempt
          return
        }
        this.#recordFailure(pending.attempt, () => previewFailureFact('preview_seek', error))
        this.#retainIncidentAttempt(active, pending.attempt)
        delete active.seekAttempt
        if (active.framePresented === true && isLocalSeekFailure(error)) {
          // Only this position failed; the session, its previous frame and the
          // download stay available, so the preview remains open.
          this.#publishVideoFailure(active, this.#host.publicError(error))
          return
        }
        const failure = Object.freeze({
          state: 'error' as const,
          fileId: active.entry.idText,
          name: active.entry.name,
          message: this.#host.publicError(error),
        })
        this.#closeActive('stale_replacement')
        this.#publishPreview(failure)
      },
    )
  }

  mediaPresented(presentationId: number): void {
    const active = this.#active
    const preview = this.#host.snapshot().preview
    const media = active?.mediaAttempt
    if (
      active === undefined ||
      media === undefined ||
      (preview.state !== 'image' && preview.state !== 'video') ||
      preview.presentationId !== presentationId ||
      media.id !== presentationId
    ) {
      return
    }
    this.#exclude(media.attempt, 'success')
    this.#closeAttempt(media.attempt)
    delete active.mediaAttempt
    active.framePresented = true
    if (media.positionSeconds === undefined) return
    active.presentedPositionSeconds = media.positionSeconds
    if (preview.state === 'video' && preview.failure !== null &&
        preview.positionSeconds !== media.positionSeconds) {
      // An earlier seek's frame presented after a later seek failed and rolled
      // the slider back: point the slider at the frame now on screen.
      this.#publishPreview(Object.freeze({ ...preview, positionSeconds: media.positionSeconds }))
    }
  }

  mediaFailed(presentationId: number): void {
    const active = this.#active
    const preview = this.#host.snapshot().preview
    const media = active?.mediaAttempt
    if (
      active === undefined ||
      media === undefined ||
      (preview.state !== 'image' && preview.state !== 'video') ||
      preview.presentationId !== presentationId ||
      media.id !== presentationId
    ) {
      return
    }
    this.#recordFailure(media.attempt, () => unclassifiedFailureFact({
      stage: 'preview_media',
      recoveryDisposition: 'none',
    }))
    this.#retainIncidentAttempt(active, media.attempt)
    delete active.mediaAttempt
    if (preview.state === 'video' && active.framePresented === true) {
      // A later seek's frame did not present: keep the earlier frame on screen.
      this.#publishVideoFailure(active, MEDIA_FAILURE_MESSAGE)
      return
    }
    const failure = Object.freeze({
      state: 'error' as const,
      fileId: active.entry.idText,
      name: active.entry.name,
      message: MEDIA_FAILURE_MESSAGE,
    })
    this.#closeActive('stale_replacement')
    this.#publishPreview(failure)
  }

  close(): Promise<void> {
    return this.#closeActive('cancelled')
  }

  async #run(
    joined: JoinedBrowserShare,
    active: ScopedActivePreview,
  ): Promise<void> {
    try {
      const session = await (active.intent === 'automatic-photo'
        ? joined.automaticPhotoPreview(active.entry, active.connectivity, active.controller.signal)
        : joined.preview(active.entry, active.connectivity, active.controller.signal))
      clearTimeout(active.automaticDeadline)
      if (this.#active !== active || active.controller.signal.aborted) {
        try {
          await session.close()
        } catch (error) {
          this.#recordCleanupFailure(active, error)
        } finally {
          this.#closeAllAttempts(active)
        }
        return
      }
      active.session = session
      const media = this.#startMediaAttempt(active, session.current.url, videoPosition(session.current))
      const projected = previewSnapshot(active.entry, session.current, false, media.id)
      this.#publishPreview(projected)
      this.#exclude(active.openAttempt, 'success')
      this.#closeAttempt(active.openAttempt)
    } catch (error) {
      if (this.#active !== active || active.controller.signal.aborted) {
        this.#exclude(
          active.openAttempt,
          active.controller.signal.aborted ? 'cancelled' : 'stale_replacement',
        )
        this.#closeAttempt(active.openAttempt)
        return
      }
      if (active.intent === 'automatic-photo' && error instanceof AutomaticPhotoDeferredError) {
        this.#exclude(active.openAttempt, 'not_user_visible')
        this.cancel()
        return
      }
      this.#recordFailure(active.openAttempt, () => previewFailureFact('preview_open', error))
      active.incidentAttempt = active.openAttempt
      this.#closeActive('stale_replacement')
      this.#publishPreview(Object.freeze({
        state: 'error',
        fileId: active.entry.idText,
        name: active.entry.name,
        message: this.#host.publicError(error),
      }))
    }
  }

  #retainIncidentAttempt(active: ScopedActivePreview, attempt: PreviewAttempt): void {
    const previous = active.incidentAttempt
    active.incidentAttempt = attempt
    if (previous !== undefined && previous !== attempt) this.#closeAttempt(previous)
  }

  #publishVideoFailure(active: ScopedActivePreview, failure: string): void {
    const preview = this.#host.snapshot().preview
    if (preview.state !== 'video') return
    // The canvas still shows the last presented frame, so the slider, the
    // time label and the next seek's baseline return to its position.
    const positionSeconds = active.presentedPositionSeconds ?? preview.positionSeconds
    this.#publishPreview(Object.freeze({ ...preview, positionSeconds, seeking: false, failure }))
  }

  #startMediaAttempt(
    active: ScopedActivePreview,
    url: string,
    positionSeconds: number | undefined,
  ): PendingMediaPresentation {
    const replaced = active.mediaAttempt
    if (replaced !== undefined) {
      this.#exclude(replaced.attempt, 'stale_replacement')
      this.#closeAttempt(replaced.attempt)
      delete active.mediaAttempt
    }
    const media: PendingMediaPresentation = {
      id: this.#nextId++,
      url,
      positionSeconds,
      attempt: this.#newAttempt('preview_media'),
    }
    active.mediaAttempt = media
    return media
  }

  #closeActive(reason: PresentationExclusionReason): Promise<void> {
    const active = this.#active
    if (active === undefined) return Promise.resolve()
    this.#active = undefined
    clearTimeout(active.automaticDeadline)
    active.controller.abort(new DOMException('Preview closed', 'AbortError'))
    this.#excludeUndecided(active, reason)

    try {
      active.connectivity.close()
    } catch (error) {
      this.#recordCleanupFailure(active, error)
      this.#closeAllAttempts(active)
      throw error
    }

    let closing: void | PromiseLike<void>
    try {
      closing = active.session?.close()
    } catch (error) {
      this.#recordCleanupFailure(active, error)
      this.#closeAllAttempts(active)
      throw error
    }
    return Promise.resolve(closing).then(
      () => undefined,
      (error: unknown) => {
        this.#recordCleanupFailure(active, error)
      },
    ).finally(() => this.#closeAllAttempts(active))
  }

  #excludeUndecided(
    active: ScopedActivePreview,
    reason: PresentationExclusionReason,
  ): void {
    this.#exclude(active.openAttempt, reason)
    if (active.seekAttempt !== undefined) {
      this.#exclude(active.seekAttempt.attempt, reason)
    }
    if (active.mediaAttempt !== undefined) {
      this.#exclude(active.mediaAttempt.attempt, reason)
    }
  }

  #closeAllAttempts(active: ScopedActivePreview): void {
    const attempts = new Set<PreviewAttempt>([
      active.openAttempt,
      ...(active.seekAttempt === undefined ? [] : [active.seekAttempt.attempt]),
      ...(active.mediaAttempt === undefined ? [] : [active.mediaAttempt.attempt]),
      ...(active.incidentAttempt === undefined ? [] : [active.incidentAttempt]),
    ])
    for (const attempt of attempts) this.#closeAttempt(attempt)
  }

  #recordCleanupFailure(active: ScopedActivePreview, error: unknown): void {
    const attempt =
      active.incidentAttempt ??
      active.seekAttempt?.attempt ??
      active.mediaAttempt?.attempt ??
      active.openAttempt
    try {
      attempt.scope?.facts.record(
        previewFailureFact('cleanup', error),
        'consequence',
      )
    } catch {
      // Cleanup evidence cannot change preview cleanup or its primary outcome.
    }
  }

  #newAttempt(kind: PreviewScopeKind): PreviewAttempt {
    let scope: IncidentScopeOwner | undefined
    try {
      scope = this.#host.incidents?.openScope(kind)
    } catch {
      // Diagnostics cannot acquire browser-media authority.
    }
    return {
      ...(scope === undefined ? {} : { scope }),
      decisionSettled: false,
    }
  }

  #recordFailure(
    attempt: PreviewAttempt,
    failure: () => FailureFact,
  ): void {
    const scope = attempt.scope
    if (scope === undefined || attempt.decisionSettled) return
    try {
      const trigger = scope.facts.record(failure(), 'contributor')
      this.#submit(
        attempt,
        incidentPresentationDecision('preview', 'failed', trigger),
      )
    } catch {
      attempt.decisionSettled = true
      // Diagnostic construction is isolated from the visible preview result.
    }
  }

  #exclude(
    attempt: PreviewAttempt,
    reason: PresentationExclusionReason,
  ): void {
    if (attempt.scope === undefined || attempt.decisionSettled) return
    try {
      this.#submit(
        attempt,
        excludedPresentationDecision('preview', reason),
      )
    } catch {
      attempt.decisionSettled = true
      // An exclusion-construction failure cannot acquire preview authority.
    }
  }

  #submit(attempt: PreviewAttempt, decision: PresentationDecision): void {
    const scope = attempt.scope
    if (scope === undefined || attempt.decisionSettled) return
    attempt.decisionSettled = true
    try {
      this.#host.incidents?.submitDecision(scope.handle, decision)
    } catch {
      // Reporter isolation is part of the product boundary, not a caller concern.
    }
  }

  #closeAttempt(attempt: PreviewAttempt): void {
    try {
      attempt.scope?.close()
    } catch {
      // Scope closure is observational and must not affect preview ownership.
    }
  }

  #publishPreview(preview: ReceiverSnapshot['preview']): void {
    this.#host.publish({ ...this.#host.snapshot(), preview })
  }
}

// A seek keeps the preview open only when its failure is confined to the
// requested position: the bounded MP4 parser rejected that one segment
// (VideoPreviewError). Decoder failures and the presentation deadline arrive
// through mediaFailed instead. Every other seek failure (a broker range read or
// disconnect, lease or source invalidation such as
// ReceiveSourceInvalidatedError, an integrity check such as PreviewRuntimeError,
// a remote rejection whether retryable or not, or a dead peer session) says the
// session itself cannot serve another seek, so the preview closes into the
// error state.
function isLocalSeekFailure(error: unknown): boolean {
  return error instanceof VideoPreviewError
}

function videoPosition(presentation: PreviewPresentation): number | undefined {
  return presentation.kind === 'video' ? presentation.positionSeconds : undefined
}

function previewFailureFact(
  stage: 'preview_open' | 'preview_seek' | 'cleanup',
  error: unknown,
): FailureFact {
  return error instanceof RemoteOperationError || error instanceof RemoteRevisionError
    ? error.failureFact
    : unclassifiedFailureFact({ stage, recoveryDisposition: 'none', error })
}
