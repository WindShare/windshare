import type { V2CatalogEntry } from '../catalog/v2-records'
import { V2RemoteOperationError } from '../content/v2-session-operations'
import { unclassifiedFailureFact } from '../diagnostics/incident/fact'
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
import type { V2JoinedBrowserShare } from './v2-gateway'
import { EMPTY_V2_PREVIEW, type V2ReceiverSnapshot } from './v2-model'
import {
  isAbortError,
  previewSnapshot,
  type ActiveV2Preview,
} from './v2-controller-state'

type PreviewScopeKind = 'preview_open' | 'preview_seek' | 'preview_media'

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
  readonly attempt: PreviewAttempt
}

interface ScopedActiveV2Preview extends ActiveV2Preview {
  readonly openAttempt: PreviewAttempt
  seekAttempt?: PendingPreviewSeek
  mediaAttempt?: PendingMediaPresentation
  incidentAttempt?: PreviewAttempt
}

export interface V2PreviewControllerHost {
  snapshot(): V2ReceiverSnapshot
  publish(snapshot: V2ReceiverSnapshot): void
  publicError(error: unknown): string
  readonly incidents?: PreviewIncidentPort
}

export class V2PreviewController {
  readonly #host: V2PreviewControllerHost
  #active: ScopedActiveV2Preview | undefined
  #nextId = 1

  constructor(host: V2PreviewControllerHost) {
    this.#host = host
  }

  open(
    joined: V2JoinedBrowserShare,
    entry: Extract<V2CatalogEntry, { kind: 'file' }>,
  ): void {
    // Connectivity must be the first post-guard action so the user's click
    // remains the activation boundary even when it replaces another preview.
    const connectivity = joined.beginPreviewConnectivity()
    this.#closeActive('stale_replacement')
    const active: ScopedActiveV2Preview = {
      id: this.#nextId++,
      entry,
      controller: new AbortController(),
      connectivity,
      seekId: 0,
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
    this.#run(joined, active).catch(() => undefined)
  }

  cancel(): void {
    this.#closeActive('cancelled')
    this.#publishPreview(EMPTY_V2_PREVIEW)
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
      this.#publishPreview(Object.freeze({ ...preview, seeking: true }))
    } catch (error) {
      this.#exclude(pending.attempt, 'not_user_visible')
      this.#closeAttempt(pending.attempt)
      delete active.seekAttempt
      throw error
    }
    let started: ReturnType<NonNullable<ActiveV2Preview['session']>['seek']>
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
        const media = this.#startMediaAttempt(active, presentation.url)
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
        this.#recordFailure(pending.attempt, 'preview_seek', error)
        active.incidentAttempt = pending.attempt
        delete active.seekAttempt
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
    this.#recordFailure(media.attempt, 'preview_media')
    active.incidentAttempt = media.attempt
    const failure = Object.freeze({
      state: 'error' as const,
      fileId: active.entry.idText,
      name: active.entry.name,
      message: 'The browser could not decode this bounded media preview.',
    })
    this.#closeActive('stale_replacement')
    this.#publishPreview(failure)
  }

  close(): Promise<void> {
    return this.#closeActive('cancelled')
  }

  async #run(
    joined: V2JoinedBrowserShare,
    active: ScopedActiveV2Preview,
  ): Promise<void> {
    try {
      const session = await joined.preview(
        active.entry,
        active.connectivity,
        active.controller.signal,
      )
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
      const media = this.#startMediaAttempt(active, session.current.url)
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
      this.#recordFailure(active.openAttempt, 'preview_open', error)
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

  #startMediaAttempt(
    active: ScopedActiveV2Preview,
    url: string,
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
      attempt: this.#newAttempt('preview_media'),
    }
    active.mediaAttempt = media
    return media
  }

  #closeActive(reason: PresentationExclusionReason): Promise<void> {
    const active = this.#active
    if (active === undefined) return Promise.resolve()
    this.#active = undefined
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
    active: ScopedActiveV2Preview,
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

  #closeAllAttempts(active: ScopedActiveV2Preview): void {
    const attempts = new Set<PreviewAttempt>([
      active.openAttempt,
      ...(active.seekAttempt === undefined ? [] : [active.seekAttempt.attempt]),
      ...(active.mediaAttempt === undefined ? [] : [active.mediaAttempt.attempt]),
      ...(active.incidentAttempt === undefined ? [] : [active.incidentAttempt]),
    ])
    for (const attempt of attempts) this.#closeAttempt(attempt)
  }

  #recordCleanupFailure(active: ScopedActiveV2Preview, error?: unknown): void {
    const attempt =
      active.incidentAttempt ??
      active.seekAttempt?.attempt ??
      active.mediaAttempt?.attempt ??
      active.openAttempt
    try {
      attempt.scope?.facts.record(
        error instanceof V2RemoteOperationError
          ? error.failureFact
          : unclassifiedFailureFact({
              stage: 'cleanup',
              recoveryDisposition: 'none',
            }),
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
    stage: 'preview_open' | 'preview_seek' | 'preview_media',
    error?: unknown,
  ): void {
    const scope = attempt.scope
    if (scope === undefined || attempt.decisionSettled) return
    try {
      const trigger = scope.facts.record(
        error instanceof V2RemoteOperationError
          ? error.failureFact
          : unclassifiedFailureFact({
              stage,
              recoveryDisposition: 'none',
            }),
        'contributor',
      )
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

  #publishPreview(preview: V2ReceiverSnapshot['preview']): void {
    this.#host.publish({ ...this.#host.snapshot(), preview })
  }
}
