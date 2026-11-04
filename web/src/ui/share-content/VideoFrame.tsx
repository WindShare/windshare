import { useEffectEvent, useLayoutEffect, useRef, useState, type CSSProperties } from 'react'
import type { PreviewSnapshot } from '../model'
import type { PreviewActions } from './MediaPreview'
import { observeVideoFrame } from './video-frame-presentation'

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
  readonly preview: Extract<PreviewSnapshot, { state: 'video' }>
  readonly actions: PreviewActions
}) {
  const canvas = useRef<HTMLCanvasElement>(null)
  const decoder = useRef<HTMLVideoElement>(null)
  const [presentedId, setPresentedId] = useState<number | null>(null)
  const [position, setPosition] = useState(preview.positionSeconds)
  const requested = useRef(preview.positionSeconds)
  const drag = useRef<{ pointerId: number; origin: number } | null>(null)
  // The presentation a shown failure rolled the slider back for.
  const failedPresentation = useRef<number | null>(null)
  const pending = preview.failure === null && (preview.seeking || presentedId !== preview.presentationId)
  const seek = (seconds: number) => {
    if (seconds === requested.current && preview.failure === null) return
    requested.current = seconds
    actions.seek(seconds)
  }
  const cancelDrag = () => {
    if (drag.current === null) return
    setPosition(drag.current.origin)
    drag.current = null
  }
  const present = useEffectEvent((video: HTMLVideoElement) => {
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
  })
  const fail = useEffectEvent(() => actions.failed(preview.presentationId))
  useLayoutEffect(() => {
    if (preview.failure === null) {
      // A new frame that succeeds after a failure was shown brings its own
      // requested position. The controller does not show a failure while a
      // newer seek is loading, so this normally matches the slider already;
      // following it keeps a rolled-back slider from staying behind the frame.
      if (failedPresentation.current === null || preview.seeking ||
          preview.presentationId === failedPresentation.current) return
      failedPresentation.current = null
    } else {
      // A failed seek leaves the previous frame on the canvas; the slider, the
      // time label and the repeated-seek guard return to that frame's position.
      failedPresentation.current = preview.presentationId
    }
    requested.current = preview.positionSeconds
    if (drag.current !== null) {
      drag.current.origin = preview.positionSeconds
      return
    }
    setPosition(preview.positionSeconds)
  }, [preview])
  useLayoutEffect(() => {
    const video = decoder.current
    if (video === null) return
    // The decoder's lifetime owns its callback, so replacement or closure cannot
    // let an obsolete frame overwrite the retained canvas or settle a new attempt.
    return observeVideoFrame(video, preview.frameSeconds, () => present(video), fail)
  }, [preview.presentationId, preview.frameSeconds, preview.url])

  return <div className="video-frame-preview">
    <div className="preview-frame-stage" style={{ '--preview-ratio': preview.width / preview.height } as CSSProperties}
      aria-busy={pending}>
      <canvas ref={canvas} className="preview-frame" width={preview.width} height={preview.height}
        role="img" aria-label={`Video preview of ${preview.name}`} />
      <video ref={decoder} key={preview.presentationId} src={preview.url} hidden muted playsInline preload="auto" />
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
      {preview.failure === null
        ? <p className="preview-seek-hint">Choose a moment to preview. Download the video to play it.</p>
        : <div className="preview-seek-failure">
          <p role="alert">{preview.failure}</p>
          <p>Choose another moment to try again. You can still download the original file.</p>
        </div>}
    </div>
  </div>
}
