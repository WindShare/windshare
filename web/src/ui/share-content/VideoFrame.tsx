import { useRef, useState, type CSSProperties } from 'react'
import type { V2PreviewSnapshot } from '../v2-model'
import type { PreviewActions } from './MediaPreview'

const MINIMUM_SEEK_STEP_SECONDS = 0.1
const SEEK_STEP_COUNT = 1_000

function time(seconds: number): string {
  const whole = Math.max(0, Math.floor(seconds))
  const minutes = Math.floor(whole / 60)
  const remainder = (whole % 60).toString().padStart(2, '0')
  return minutes < 60 ? `${minutes}:${remainder}`
    : `${Math.floor(minutes / 60)}:${(minutes % 60).toString().padStart(2, '0')}:${remainder}`
}

export function VideoFrame({ preview, actions }: {
  readonly preview: Extract<V2PreviewSnapshot, { state: 'video' }>
  readonly actions: PreviewActions
}) {
  const canvas = useRef<HTMLCanvasElement>(null)
  const [presentedId, setPresentedId] = useState<number | null>(null)
  const [position, setPosition] = useState(preview.positionSeconds)
  const requested = useRef(preview.positionSeconds)
  const drag = useRef<{ pointerId: number; origin: number } | null>(null)
  const pending = preview.seeking || presentedId !== preview.presentationId
  const seek = (seconds: number) => {
    if (seconds === requested.current) return
    requested.current = seconds
    actions.seek(seconds)
  }
  const cancelDrag = () => {
    if (drag.current === null) return
    setPosition(drag.current.origin)
    drag.current = null
  }
  const present = (video: HTMLVideoElement) => {
    if (video.seeking || video.readyState < video.HAVE_CURRENT_DATA) return
    try {
      const context = canvas.current?.getContext('2d')
      if (context === undefined || context === null) throw new Error('Frame presentation is unavailable')
      // A frame preview retains its last decoded pixels while the next bounded
      // segment loads. Decoder replacement must never clear or resize the visible surface.
      context.drawImage(video, 0, 0, preview.width, preview.height)
      setPresentedId(preview.presentationId)
      actions.presented(preview.presentationId)
    } catch {
      actions.failed(preview.presentationId)
    }
  }

  return <div className="video-frame-preview">
    <div className="preview-frame-stage" style={{ '--preview-ratio': preview.width / preview.height } as CSSProperties}
      aria-busy={pending}>
      <canvas ref={canvas} className="preview-frame" width={preview.width} height={preview.height}
        role="img" aria-label={`Video preview of ${preview.name}`} />
      <video key={preview.presentationId} src={preview.url} hidden muted playsInline preload="auto"
        onLoadedMetadata={event => { event.currentTarget.currentTime = preview.positionSeconds }}
        onLoadedData={event => present(event.currentTarget)}
        onSeeked={event => present(event.currentTarget)}
        onError={() => actions.failed(preview.presentationId)} />
      <span className="preview-frame-status" role="status">{pending ? 'Loading frame…' : ''}</span>
    </div>
    <div className="preview-seek">
      <div className="preview-seek-heading">
        <span>Frame preview</span>
        <span className="preview-time">{time(position)} / {time(preview.durationSeconds)}</span>
      </div>
      <input type="range" min={0} max={preview.durationSeconds}
        step={Math.max(MINIMUM_SEEK_STEP_SECONDS, preview.durationSeconds / SEEK_STEP_COUNT)}
        value={position} aria-label={`Seek ${preview.name}`}
        aria-valuetext={`${time(position)} of ${time(preview.durationSeconds)}`}
        onPointerDown={event => {
          if (!event.isPrimary || event.button !== 0) return
          drag.current = { pointerId: event.pointerId, origin: position }
          event.currentTarget.setPointerCapture(event.pointerId)
        }}
        onChange={event => {
          const seconds = event.currentTarget.valueAsNumber
          setPosition(seconds)
          // Scrubbing is local intent; only release commits a remote frame request.
          if (drag.current === null) seek(seconds)
        }}
        onPointerUp={event => {
          if (drag.current?.pointerId !== event.pointerId) return
          drag.current = null
          seek(event.currentTarget.valueAsNumber)
        }}
        onPointerCancel={cancelDrag}
        onLostPointerCapture={cancelDrag} />
      <p className="preview-seek-hint">Choose a moment to preview. Download the video to play it.</p>
    </div>
  </div>
}
