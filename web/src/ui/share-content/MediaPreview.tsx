import { useRef } from 'react'
import type { V2PreviewSnapshot } from '../v2-model'

export interface PreviewActions {
  readonly close: () => void
  readonly seek: (seconds: number) => void
  readonly presented: (presentationId: number) => void
  readonly failed: (presentationId: number) => void
}

const MINIMUM_SEEK_STEP_SECONDS = 0.1
const SEEK_STEP_COUNT = 1_000

function time(seconds: number): string {
  const whole = Math.max(0, Math.floor(seconds))
  return `${Math.floor(whole / 60)}:${(whole % 60).toString().padStart(2, '0')}`
}

function VideoFrame({ preview, actions }: {
  readonly preview: Extract<V2PreviewSnapshot, { state: 'video' }>
  readonly actions: PreviewActions
}) {
  const video = useRef<HTMLVideoElement>(null)
  const position = () => {
    if (video.current !== null) video.current.currentTime = preview.positionSeconds
  }
  return <>
    <video ref={video} key={preview.presentationId} className="preview-media"
      src={preview.url} aria-label={`Video preview of ${preview.name}`} muted playsInline preload="auto"
      onLoadedMetadata={position}
      onLoadedData={() => { position(); actions.presented(preview.presentationId) }}
      onSeeked={() => actions.presented(preview.presentationId)}
      onError={() => actions.failed(preview.presentationId)} />
    <label className="preview-seek">
      <span>Frame preview · {time(preview.positionSeconds)} / {time(preview.durationSeconds)}
        {preview.seeking ? ' · seeking…' : ''}</span>
      <input type="range" min={0} max={preview.durationSeconds}
        step={Math.max(MINIMUM_SEEK_STEP_SECONDS, preview.durationSeconds / SEEK_STEP_COUNT)}
        value={preview.positionSeconds} aria-label={`Seek ${preview.name}`}
        aria-busy={preview.seeking} onChange={event => actions.seek(event.currentTarget.valueAsNumber)} />
    </label>
  </>
}

export function MediaPreview({ preview, actions, inline = false }: {
  readonly preview: V2PreviewSnapshot
  readonly actions: PreviewActions
  readonly inline?: boolean
}) {
  if (preview.state === 'idle') return null
  return <section className={`media-preview ${inline ? 'media-preview-inline' : ''}`} aria-label="File preview">
    {preview.state === 'loading' && <p role="status">Opening preview…</p>}
    {preview.state === 'error' && <div className="preview-failure">
      <p role="alert">{preview.message}</p><p>You can still download the original file.</p>
    </div>}
    {preview.state === 'image' && <img className="preview-media" src={preview.url}
      alt={`Preview of ${preview.name}`}
      onLoad={() => actions.presented(preview.presentationId)}
      onError={() => actions.failed(preview.presentationId)} />}
    {preview.state === 'video' && <VideoFrame preview={preview} actions={actions} />}
    {inline && <button className="quiet-action" type="button" onClick={actions.close}>Close preview</button>}
  </section>
}
