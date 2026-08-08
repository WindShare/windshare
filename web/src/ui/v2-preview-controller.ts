import type { V2CatalogEntry } from '../catalog/v2-records'
import type { V2JoinedBrowserShare } from './v2-gateway'
import { EMPTY_V2_PREVIEW, type V2ReceiverSnapshot } from './v2-model'
import {
  isAbortError,
  previewSnapshot,
  type ActiveV2Preview,
} from './v2-controller-state'

export interface V2PreviewControllerHost {
  snapshot(): V2ReceiverSnapshot
  publish(snapshot: V2ReceiverSnapshot): void
  publicError(error: unknown): string
}

export class V2PreviewController {
  readonly #host: V2PreviewControllerHost
  #active: ActiveV2Preview | undefined
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
    this.#closeActive()
    const active: ActiveV2Preview = {
      id: this.#nextId++,
      entry,
      controller: new AbortController(),
      connectivity,
      seekId: 0,
    }
    this.#active = active
    this.#publishPreview(Object.freeze({
      state: 'loading',
      fileId: entry.idText,
      name: entry.name,
    }))
    this.#run(joined, active).catch(() => undefined)
  }

  cancel(): void {
    this.#closeActive()
    this.#publishPreview(EMPTY_V2_PREVIEW)
  }

  seek(seconds: number): void {
    const active = this.#active
    const preview = this.#host.snapshot().preview
    if (active?.session === undefined || preview.state !== 'video') return
    const seekId = ++active.seekId
    this.#publishPreview(Object.freeze({ ...preview, seeking: true }))
    active.session.seek(seconds, active.controller.signal).then(
      (presentation) => {
        if (this.#active !== active || active.seekId !== seekId) return
        this.#publishPreview(previewSnapshot(active.entry, presentation, false))
      },
      (error: unknown) => {
        if (this.#active !== active || active.seekId !== seekId || isAbortError(error)) return
        const failure = Object.freeze({
          state: 'error' as const,
          fileId: active.entry.idText,
          name: active.entry.name,
          message: this.#host.publicError(error),
        })
        this.#closeActive()
        this.#publishPreview(failure)
      },
    )
  }

  mediaFailed(url: string): void {
    const active = this.#active
    const preview = this.#host.snapshot().preview
    if (active === undefined || (preview.state !== 'image' && preview.state !== 'video') ||
        preview.url !== url) return
    const failure = Object.freeze({
      state: 'error' as const,
      fileId: active.entry.idText,
      name: active.entry.name,
      message: 'The browser could not decode this bounded media preview.',
    })
    this.#closeActive()
    this.#publishPreview(failure)
  }

  close(): Promise<void> {
    return this.#closeActive()
  }

  async #run(joined: V2JoinedBrowserShare, active: ActiveV2Preview): Promise<void> {
    try {
      const session = await joined.preview(
        active.entry,
        active.connectivity,
        active.controller.signal,
      )
      if (this.#active !== active || active.controller.signal.aborted) {
        await session.close().catch(() => undefined)
        return
      }
      active.session = session
      this.#publishPreview(previewSnapshot(active.entry, session.current, false))
    } catch (error) {
      if (this.#active !== active || active.controller.signal.aborted) return
      this.#active = undefined
      active.connectivity.close()
      this.#publishPreview(Object.freeze({
        state: 'error',
        fileId: active.entry.idText,
        name: active.entry.name,
        message: this.#host.publicError(error),
      }))
    }
  }

  #closeActive(): Promise<void> {
    const active = this.#active
    if (active === undefined) return Promise.resolve()
    this.#active = undefined
    active.controller.abort(new DOMException('Preview closed', 'AbortError'))
    active.connectivity.close()
    return (active.session?.close() ?? Promise.resolve()).catch(() => undefined)
  }

  #publishPreview(preview: V2ReceiverSnapshot['preview']): void {
    this.#host.publish({ ...this.#host.snapshot(), preview })
  }
}
