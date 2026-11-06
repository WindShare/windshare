import { isAutomaticPhotoCandidate, AutomaticPhotoDeferredError, AUTOMATIC_PHOTO_TIMEOUT_MILLISECONDS } from '../preview/automatic-photo'
import type { CatalogEntry } from '../catalog/records'
import { RemoteOperationError } from '../content/session-operations'
import { RemoteRevisionError } from '../content/session-services'
import { isPositionLocalPreviewFailure } from '../preview/preview'
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
import type { PreviewTraceEvent } from '../application/receive/observation/contracts'
import type { DomainTraceSource } from '../diagnostics/trace/ports'
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

type ReadyPreviewSnapshot = Extract<ReceiverSnapshot['preview'], { state: 'image' | 'video' }>

interface PendingMediaPresentation {
  readonly preview: ReadyPreviewSnapshot
  readonly attempt: PreviewAttempt
}

interface ScopedActivePreview extends ActivePreview {
  readonly intent: 'manual' | 'automatic-photo'
  readonly openAttempt: PreviewAttempt
  automaticDeadline?: ReturnType<typeof setTimeout>
  seekAttempt?: PendingPreviewSeek
  mediaAttempt?: PendingMediaPresentation
  incidentAttempt?: PreviewAttempt
  // Retain the complete accepted presentation instead of independent flags and
  // positions that could disagree about what is actually on the canvas.
  presented?: ReadyPreviewSnapshot
  // The attempt whose incident reports this session's local seek failures.
  localFailureAttempt?: PreviewAttempt
}

export interface PreviewControllerHost {
  snapshot(): ReceiverSnapshot
  publish(snapshot: ReceiverSnapshot): void
  publicError(error: unknown): string
  readonly incidents?: PreviewIncidentPort
  readonly trace?: DomainTraceSource<PreviewTraceEvent>
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
      this.#publishPreview(Object.freeze({ ...preview, requestedSeconds: seconds, seeking: true, failure: null }))
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
        const projected = previewSnapshot(active.entry, presentation, this.#nextId++)
        const media = this.#startMediaAttempt(active, projected.state === 'video'
          ? Object.freeze({ ...projected, requestedSeconds: seconds })
          : projected)
        try {
          this.#publishPreview(media.preview)
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
        delete active.seekAttempt
        const fact = () => previewFailureFact('preview_seek', error)
        if (active.presented?.state === 'video' && isPositionLocalPreviewFailure(error)) {
          // Only this position failed; the session, its previous frame and the
          // download stay available, so the preview remains open.
          this.#recordLocalFailure(active, pending.attempt, fact)
          this.#publishVideoFailure(active, this.#host.publicError(error))
          return
        }
        this.#recordFailure(pending.attempt, fact)
        this.#retainIncidentAttempt(active, pending.attempt)
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
      media.preview.presentationId !== presentationId
    ) {
      return
    }
    this.#exclude(media.attempt, 'success')
    this.#closeAttempt(media.attempt)
    delete active.mediaAttempt
    active.presented = media.preview
    if (preview.state === 'video' && media.preview.state === 'video' && preview.failure !== null &&
        preview.requestedSeconds !== media.preview.requestedSeconds) {
      // A prior frame can arrive after a newer request failed. Its accepted
      // snapshot owns the rollback position, without changing the latest error.
      this.#publishPreview(Object.freeze({ ...preview, requestedSeconds: media.preview.requestedSeconds }))
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
      media.preview.presentationId !== presentationId
    ) {
      return
    }
    const fact = () => unclassifiedFailureFact({
      stage: 'preview_media',
      recoveryDisposition: 'none',
    })
    delete active.mediaAttempt
    if (preview.state === 'video' && active.presented?.state === 'video') {
      // A later seek's frame did not present: keep the earlier frame on screen.
      this.#recordLocalFailure(active, media.attempt, fact)
      // A newer seek is still reading its range, and the snapshot already
      // belongs to it (it reuses this presentation's id while it loads). Its
      // own result decides the slider, status and failure next, so this stale
      // frame only adds to the incident: publishing it would clear seeking,
      // show an error and roll the slider back under a seek still in flight.
      if (active.seekAttempt !== undefined) return
      this.#publishVideoFailure(active, MEDIA_FAILURE_MESSAGE)
      return
    }
    this.#recordFailure(media.attempt, fact)
    this.#retainIncidentAttempt(active, media.attempt)
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
      const media = this.#startMediaAttempt(active,
        previewSnapshot(active.entry, session.current, this.#nextId++))
      this.#publishPreview(media.preview)
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
        this.#traceAutomaticDeferral(joined, active, error)
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

  #traceAutomaticDeferral(
    joined: JoinedBrowserShare,
    active: ScopedActivePreview,
    error: AutomaticPhotoDeferredError,
  ): void {
    try {
      this.#host.trace?.current?.({
        name: 'preview_transition', transition: 'deferred', previewId: active.id,
        shareInstanceId: joined.descriptor.shareInstanceId, fileId: active.entry.idText, error,
      })
    } catch {
      // A diagnostic sink cannot turn optional presentation into a visible failure.
    }
  }

  #retainIncidentAttempt(active: ScopedActivePreview, attempt: PreviewAttempt): void {
    const previous = active.incidentAttempt
    active.incidentAttempt = attempt
    if (previous !== undefined && previous !== attempt) this.#closeAttempt(previous)
  }

  // Scrubbing a damaged region must not flood diagnostics. Local failures share
  // one incident, with later failures recorded as consequences. A session-ending
  // failure owns a separate incident because it requires different recovery.
  #recordLocalFailure(
    active: ScopedActivePreview,
    attempt: PreviewAttempt,
    failure: () => FailureFact,
  ): void {
    const reported = active.localFailureAttempt
    if (reported === undefined) {
      this.#recordFailure(attempt, failure)
      active.localFailureAttempt = attempt
      this.#retainIncidentAttempt(active, attempt)
      return
    }
    this.#closeAttempt(attempt)
    try {
      reported.scope?.facts.record(failure(), 'consequence')
    } catch {
      // Repeated-failure evidence cannot change the visible preview result.
    }
  }

  #publishVideoFailure(active: ScopedActivePreview, failure: string): void {
    const preview = this.#host.snapshot().preview
    if (preview.state !== 'video' || active.presented?.state !== 'video') return
    // The canvas still shows the last presented frame, so the slider, the
    // time label and the next seek's baseline return to the position the
    // user requested for it.
    const requestedSeconds = active.presented.requestedSeconds
    this.#publishPreview(Object.freeze({ ...preview, requestedSeconds, seeking: false, failure }))
  }

  #startMediaAttempt(
    active: ScopedActivePreview,
    preview: ReadyPreviewSnapshot,
  ): PendingMediaPresentation {
    const replaced = active.mediaAttempt
    if (replaced !== undefined) {
      this.#exclude(replaced.attempt, 'stale_replacement')
      this.#closeAttempt(replaced.attempt)
      delete active.mediaAttempt
    }
    const media: PendingMediaPresentation = {
      preview,
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

function previewFailureFact(
  stage: 'preview_open' | 'preview_seek' | 'cleanup',
  error: unknown,
): FailureFact {
  return error instanceof RemoteOperationError || error instanceof RemoteRevisionError
    ? error.failureFact
    : unclassifiedFailureFact({ stage, recoveryDisposition: 'none', error })
}
