type FrameDecoder = Pick<HTMLVideoElement,
  'currentTime' | 'readyState' | 'HAVE_METADATA' |
  'addEventListener' | 'removeEventListener' |
  'requestVideoFrameCallback' | 'cancelVideoFrameCallback'>

// The range preview selects an exact sample PTS; browser media pipelines can
// round that rational timestamp to microseconds.
const MEDIA_TIME_ROUNDING_SECONDS = 1 / 1_000_000

export function observeVideoFrame(
  video: FrameDecoder,
  frameTimeSeconds: number,
  presented: () => void,
  failed: () => void,
): () => void {
  let closed = false
  let callbackId: number | undefined

  const close = () => {
    closed = true
    if (callbackId !== undefined) video.cancelVideoFrameCallback(callbackId)
    callbackId = undefined
    video.removeEventListener('loadedmetadata', seek)
    video.removeEventListener('error', fail)
  }
  const fail = () => {
    if (closed) return
    close()
    failed()
  }
  const seek = () => {
    if (closed) return
    try {
      if (video.currentTime !== frameTimeSeconds) video.currentTime = frameTimeSeconds
    } catch {
      fail()
    }
  }
  const frame: VideoFrameRequestCallback = (_now, metadata) => {
    if (closed) return
    callbackId = undefined
    // loadeddata/seeked can precede the compositor's target frame. A callback
    // can also still describe the initial frame after currentTime has changed.
    if (Math.abs(metadata.mediaTime - frameTimeSeconds) > MEDIA_TIME_ROUNDING_SECONDS) {
      requestFrame()
      return
    }
    close()
    presented()
  }
  const requestFrame = () => {
    try {
      callbackId = video.requestVideoFrameCallback(frame)
    } catch {
      fail()
    }
  }

  video.addEventListener('loadedmetadata', seek)
  video.addEventListener('error', fail)
  requestFrame()
  if (video.readyState >= video.HAVE_METADATA) seek()
  return close
}
