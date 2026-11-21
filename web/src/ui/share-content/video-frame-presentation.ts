type FrameDecoder = Pick<HTMLVideoElement,
  'currentTime' | 'readyState' | 'HAVE_METADATA' |
  'addEventListener' | 'removeEventListener' |
  'requestVideoFrameCallback' | 'cancelVideoFrameCallback'>

type SeekPhase = 'idle' | 'seeking' | 'seeked' | 'stale-while-seeking' | 'reseeked'

// The range preview selects an exact sample PTS; browser media pipelines can
// round that rational timestamp to microseconds.
const MEDIA_TIME_ROUNDING_SECONDS = 1 / 1_000_000
// A preview segment is one bounded local sample behind a blob URL, so the
// decoder needs no network to load or present it. The deadline starts when the
// observer attaches to the decoder (its src is already assigned), so a decoder
// that never reports loadedmetadata fails too. A visible decoder that still has
// no frame after this long has stalled (for example `seeked` with no frame
// callback); the attempt fails instead of loading forever. Background tabs may
// throttle or suspend media work, so the deadline does not run while the
// document is hidden and restarts in full when it becomes visible again.
export const VIDEO_FRAME_PRESENTATION_TIMEOUT_MS = 10_000

export type DocumentVisibility = Pick<Document, 'visibilityState' | 'addEventListener' | 'removeEventListener'>

function defaultVisibility(): DocumentVisibility | undefined {
  return typeof document === 'undefined' ? undefined : document
}

export function observeVideoFrame(
  video: FrameDecoder,
  frameTimeSeconds: number,
  presented: () => void,
  failed: () => void,
  visibility: DocumentVisibility | undefined = defaultVisibility(),
): () => void {
  let closed = false
  let callbackId: number | undefined
  let deadline: ReturnType<typeof setTimeout> | undefined
  let seekPhase: SeekPhase = 'idle'

  const close = () => {
    closed = true
    if (callbackId !== undefined) video.cancelVideoFrameCallback(callbackId)
    callbackId = undefined
    pauseDeadline()
    visibility?.removeEventListener('visibilitychange', visibilityChanged)
    video.removeEventListener('loadedmetadata', seek)
    video.removeEventListener('seeked', seekCompleted)
    video.removeEventListener('error', fail)
  }
  const finish = () => {
    if (closed) return
    close()
    presented()
  }
  const fail = () => {
    if (closed) return
    close()
    failed()
  }
  const pauseDeadline = () => {
    if (deadline !== undefined) clearTimeout(deadline)
    deadline = undefined
  }
  const armDeadline = () => {
    if (closed || visibility?.visibilityState === 'hidden') return
    deadline ??= setTimeout(fail, VIDEO_FRAME_PRESENTATION_TIMEOUT_MS)
  }
  const visibilityChanged = () => {
    if (closed) return
    // Hidden time does not count; a returning tab gets the full budget.
    pauseDeadline()
    armDeadline()
  }
  const requestFrame = () => {
    if (closed || callbackId !== undefined) return
    try {
      callbackId = video.requestVideoFrameCallback(frame)
    } catch {
      fail()
    }
  }
  const assignCurrentTime = () => {
    try {
      video.currentTime = frameTimeSeconds
    } catch {
      fail()
    }
  }
  const reseek = () => {
    // Chromium uses a one-shot compositor callback per requestVideoFrameCallback.
    // If an earlier frame consumes it while a paused seek presents the target
    // frame, re-arming afterwards never fires unless re-sought once.
    seekPhase = 'reseeked'
    requestFrame()
    if (closed) return
    assignCurrentTime()
  }
  const seek = () => {
    if (closed) return
    requestFrame()
    if (closed || seekPhase !== 'idle' || video.currentTime === frameTimeSeconds) return
    seekPhase = 'seeking'
    assignCurrentTime()
  }
  const seekCompleted = () => {
    if (closed) return
    if (seekPhase === 'seeking') seekPhase = 'seeked'
    else if (seekPhase === 'stale-while-seeking') reseek()
  }
  const frame: VideoFrameRequestCallback = (_now, metadata) => {
    if (closed) return
    callbackId = undefined
    if (Math.abs(metadata.mediaTime - frameTimeSeconds) <= MEDIA_TIME_ROUNDING_SECONDS) {
      finish()
      return
    }
    // loadeddata/seeked can precede the compositor's target frame. A callback
    // can also still describe the initial frame after currentTime has changed.
    requestFrame()
    if (closed) return
    if (seekPhase === 'seeking') seekPhase = 'stale-while-seeking'
    else if (seekPhase === 'seeked') reseek()
  }

  video.addEventListener('loadedmetadata', seek)
  video.addEventListener('seeked', seekCompleted)
  video.addEventListener('error', fail)
  visibility?.addEventListener('visibilitychange', visibilityChanged)
  armDeadline()
  // Defer registration until seek() when a seek is needed so the pre-seek
  // initial frame cannot consume the one-shot compositor callback early.
  if (video.readyState >= video.HAVE_METADATA) seek()
  else if (video.currentTime === frameTimeSeconds) requestFrame()
  return close
}
