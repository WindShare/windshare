import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  observeVideoFrame,
  VIDEO_FRAME_PRESENTATION_TIMEOUT_MS,
} from '../../src/ui/share-content/video-frame-presentation'

class FrameDecoder extends EventTarget {
  readonly HAVE_METADATA = 1
  readonly HAVE_CURRENT_DATA = 2
  readyState = 0
  // A decoder timeline that contains the requested frame time, as in Chromium.
  duration = 10
  seeking = false
  readonly seeks: number[] = []
  readonly callbacks = new Map<number, VideoFrameRequestCallback>()
  readonly cancelled: number[] = []
  #time = 0
  #nextCallback = 0

  get currentTime() { return this.#time }
  set currentTime(seconds: number) {
    this.#time = seconds
    this.seeks.push(seconds)
  }
  requestVideoFrameCallback(callback: VideoFrameRequestCallback) {
    const id = ++this.#nextCallback
    this.callbacks.set(id, callback)
    return id
  }
  cancelVideoFrameCallback(id: number) {
    this.cancelled.push(id)
    this.callbacks.delete(id)
  }
  metadata() {
    this.readyState = this.HAVE_METADATA
    this.dispatchEvent(new Event('loadedmetadata'))
  }
  seeked() {
    this.seeking = false
    this.readyState = this.HAVE_CURRENT_DATA
    this.dispatchEvent(new Event('seeked'))
  }
  frame(mediaTime: number) {
    const callbacks = [...this.callbacks.values()]
    this.callbacks.clear()
    const metadata = { mediaTime, width: 640, height: 360,
      presentationTime: 0, expectedDisplayTime: 0, presentedFrames: 1 }
    for (const callback of callbacks) callback(0, metadata)
    return () => { for (const callback of callbacks) callback(0, metadata) }
  }
}

function observe(video = new FrameDecoder(), time = 0.1) {
  const presented = vi.fn()
  const failed = vi.fn()
  const close = observeVideoFrame(video, time, presented, failed)
  return { video, presented, failed, close }
}

describe('video frame presentation ownership', () => {
  it('waits for the selected sample even when media events and currentTime report seek completion', () => {
    const { video, presented, failed } = observe()
    video.metadata()
    expect(video.currentTime).toBeCloseTo(0.1)
    video.dispatchEvent(new Event('seeked'))
    video.dispatchEvent(new Event('loadeddata'))
    expect(presented).not.toHaveBeenCalled()

    video.frame(0)
    expect(presented).not.toHaveBeenCalled()
    expect(video.callbacks.size).toBe(1)
    const duplicate = video.frame(0.1)
    expect(presented).toHaveBeenCalledExactlyOnceWith()
    duplicate()
    video.dispatchEvent(new Event('error'))
    expect(presented).toHaveBeenCalledTimes(1)
    expect(failed).not.toHaveBeenCalled()
    expect(video.callbacks.size).toBe(0)
  })

  it('accepts a composited target frame before the seeked event is delivered', () => {
    const { video, presented } = observe()
    video.metadata()
    video.frame(0.1)
    expect(presented).toHaveBeenCalledTimes(1)
    video.dispatchEvent(new Event('seeked'))
    expect(presented).toHaveBeenCalledTimes(1)
  })

  it('attaches to cached metadata and tolerates media timestamp rounding', () => {
    const video = new FrameDecoder()
    video.metadata()
    const { presented } = observe(video, 1 / 30)
    expect(video.seeks).toEqual([1 / 30])
    video.frame(0.033333)
    expect(presented).toHaveBeenCalledTimes(1)
  })

  it('presents the first frame without issuing a redundant seek', () => {
    const { video, presented } = observe(new FrameDecoder(), 0)
    video.metadata()
    expect(video.seeks).toEqual([])
    video.frame(0)
    expect(presented).toHaveBeenCalledTimes(1)
  })

  it('cancels callbacks and metadata handling when the decoder is replaced or closed', () => {
    const { video, presented, failed, close } = observe()
    const queued = video.callbacks.values().next().value!
    close()
    close()
    video.metadata()
    queued(0, { mediaTime: 0.1, width: 640, height: 360,
      presentationTime: 0, expectedDisplayTime: 0, presentedFrames: 1 })
    video.dispatchEvent(new Event('error'))
    expect(video.seeks).toEqual([])
    expect(video.cancelled).toEqual([1])
    expect(video.callbacks.size).toBe(0)
    expect(presented).not.toHaveBeenCalled()
    expect(failed).not.toHaveBeenCalled()
  })

  it('settles decoder errors once and releases the pending frame callback', () => {
    const { video, presented, failed } = observe()
    video.dispatchEvent(new Event('error'))
    video.dispatchEvent(new Event('error'))
    video.metadata()
    expect(failed).toHaveBeenCalledTimes(1)
    expect(presented).not.toHaveBeenCalled()
    expect(video.cancelled).toEqual([1])
    expect(video.seeks).toEqual([])
  })

  it.each(['seek', 'callback'] as const)('reports a rejected %s without retaining a callback', kind => {
    const video = new FrameDecoder()
    video.metadata()
    if (kind === 'seek') {
      vi.spyOn(video, 'currentTime', 'set').mockImplementation(() => { throw new Error('seek rejected') })
    } else {
      vi.spyOn(video, 'requestVideoFrameCallback').mockImplementation(() => { throw new Error('callback rejected') })
    }
    const { presented, failed } = observe(video)
    expect(failed).toHaveBeenCalledTimes(1)
    expect(presented).not.toHaveBeenCalled()
    expect(video.callbacks.size).toBe(0)
  })
})

describe('video frame presentation fallbacks', () => {
  afterEach(() => { vi.useRealTimers() })

  it('fails a decoder that loads metadata but never presents a frame', () => {
    vi.useFakeTimers()
    const { video, presented, failed } = observe()
    // The segment URL may still be loading; only a loaded decoder is timed.
    vi.advanceTimersByTime(VIDEO_FRAME_PRESENTATION_TIMEOUT_MS * 2)
    expect(failed).not.toHaveBeenCalled()
    video.metadata()
    video.seeked()
    video.frame(0)
    vi.advanceTimersByTime(VIDEO_FRAME_PRESENTATION_TIMEOUT_MS - 1)
    expect(failed).not.toHaveBeenCalled()
    vi.advanceTimersByTime(1)
    expect(failed).toHaveBeenCalledExactlyOnceWith()
    expect(presented).not.toHaveBeenCalled()
    expect(video.callbacks.size).toBe(0)
    video.frame(0.1)
    vi.advanceTimersByTime(VIDEO_FRAME_PRESENTATION_TIMEOUT_MS)
    expect(presented).not.toHaveBeenCalled()
    expect(failed).toHaveBeenCalledTimes(1)
  })

  it('cancels the presentation deadline when the frame presents or the decoder closes', () => {
    vi.useFakeTimers()
    const first = observe()
    first.video.metadata()
    first.video.frame(0.1)
    const second = observe()
    second.video.metadata()
    second.close()
    vi.advanceTimersByTime(VIDEO_FRAME_PRESENTATION_TIMEOUT_MS)
    expect(first.presented).toHaveBeenCalledTimes(1)
    expect(first.failed).not.toHaveBeenCalled()
    expect(second.failed).not.toHaveBeenCalled()
    expect(vi.getTimerCount()).toBe(0)
  })

  // Firefox rebases the one-sample segment to zero: its timeline ends at the
  // sample duration, the seek is clamped, and the frame reports mediaTime 0.
  it('accepts the only sample once a decoder timeline that ends before it has settled', () => {
    const video = new FrameDecoder()
    video.duration = 0.1
    const { presented } = observe(video, 0.2)
    video.metadata()
    expect(video.seeks).toEqual([0.2])
    video.seeking = true
    video.frame(0)
    expect(presented).not.toHaveBeenCalled()
    video.dispatchEvent(new Event('loadeddata'))
    expect(presented).not.toHaveBeenCalled()
    video.seeked()
    expect(presented).toHaveBeenCalledExactlyOnceWith()
  })

  it('accepts a rebased frame that arrives after the seek settles', () => {
    const video = new FrameDecoder()
    video.duration = 0.1
    const { presented } = observe(video, 0.1)
    video.metadata()
    video.seeked()
    expect(presented).not.toHaveBeenCalled()
    video.frame(0)
    expect(presented).toHaveBeenCalledTimes(1)
  })

  it('keeps waiting for the exact frame when the decoder timeline contains it', () => {
    const video = new FrameDecoder()
    video.duration = 0.3
    const { presented } = observe(video, 0.2)
    video.metadata()
    video.frame(0)
    video.seeked()
    video.frame(0.1)
    expect(presented).not.toHaveBeenCalled()
    video.frame(0.2)
    expect(presented).toHaveBeenCalledTimes(1)
  })
})
