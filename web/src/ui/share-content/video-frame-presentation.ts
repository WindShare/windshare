type FrameDecoder = Pick<HTMLVideoElement,
  'currentTime' | 'duration' | 'seeking' | 'readyState' | 'HAVE_METADATA' | 'HAVE_CURRENT_DATA' |
  'addEventListener' | 'removeEventListener' |
  'requestVideoFrameCallback' | 'cancelVideoFrameCallback'>

// The range preview selects an exact sample PTS; browser media pipelines can
// round that rational timestamp to microseconds.
const MEDIA_TIME_ROUNDING_SECONDS = 1 / 1_000_000
// A preview segment is one local sample, so once its metadata has loaded the
// decoder needs no network to present it. A decoder that still has no frame
// after this long has stalled (for example `seeked` with no frame callback);
// the attempt fails into the preview error state instead of loading forever.
export const VIDEO_FRAME_PRESENTATION_TIMEOUT_MS = 10_000

export function observeVideoFrame(
  video: FrameDecoder,
  frameTimeSeconds: number,
  presented: () => void,
  failed: () => void,
): () => void {
  let closed = false
  let callbackId: number | undefined
  let deadline: ReturnType<typeof setTimeout> | undefined
  let rebasedFrame = false

  const close = () => {
    closed = true
    if (callbackId !== undefined) video.cancelVideoFrameCallback(callbackId)
    callbackId = undefined
    if (deadline !== undefined) clearTimeout(deadline)
    deadline = undefined
    video.removeEventListener('loadedmetadata', seek)
    video.removeEventListener('loadeddata', settleRebased)
    video.removeEventListener('seeked', settleRebased)
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
  const seek = () => {
    if (closed) return
    deadline ??= setTimeout(fail, VIDEO_FRAME_PRESENTATION_TIMEOUT_MS)
    try {
      if (video.currentTime !== frameTimeSeconds) video.currentTime = frameTimeSeconds
    } catch {
      fail()
    }
  }
  // Firefox rebases a one-sample segment to start at zero, so its timeline
  // ends before the sample's composition time and its frames report mediaTime
  // 0; WebKitGTK ends the timeline at the sample's decode time. When the
  // decoder's own timeline ends at or before the requested time, no frame can
  // be presented there: the seek is clamped and mediaTime cannot match. The segment holds only the selected sample, so
  // any frame presented then is that sample. Firefox can report that frame
  // before its data is current or while the clamped seek runs, when drawing it
  // would copy nothing, so it is accepted once the decoder has settled.
  const rebasedTimeline = () =>
    video.readyState >= video.HAVE_METADATA && Number.isFinite(video.duration) &&
    video.duration <= frameTimeSeconds + MEDIA_TIME_ROUNDING_SECONDS
  const settleRebased = () => {
    if (rebasedFrame && !video.seeking && video.readyState >= video.HAVE_CURRENT_DATA) finish()
  }
  const frame: VideoFrameRequestCallback = (_now, metadata) => {
    if (closed) return
    callbackId = undefined
    if (Math.abs(metadata.mediaTime - frameTimeSeconds) <= MEDIA_TIME_ROUNDING_SECONDS) {
      finish()
      return
    }
    if (rebasedTimeline()) {
      rebasedFrame = true
      settleRebased()
      return
    }
    // loadeddata/seeked can precede the compositor's target frame. A callback
    // can also still describe the initial frame after currentTime has changed.
    requestFrame()
  }
  const requestFrame = () => {
    try {
      callbackId = video.requestVideoFrameCallback(frame)
    } catch {
      fail()
    }
  }

  video.addEventListener('loadedmetadata', seek)
  video.addEventListener('loadeddata', settleRebased)
  video.addEventListener('seeked', settleRebased)
  video.addEventListener('error', fail)
  requestFrame()
  if (video.readyState >= video.HAVE_METADATA) seek()
  return close
}
