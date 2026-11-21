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

class FakeVisibility extends EventTarget {
  visibilityState: DocumentVisibilityState = 'visible'
  set(state: DocumentVisibilityState) {
    this.visibilityState = state
    this.dispatchEvent(new Event('visibilitychange'))
  }
}

function observe(video = new FrameDecoder(), time = 0.1, visibility?: FakeVisibility) {
  const presented = vi.fn()
  const failed = vi.fn()
  const close = observeVideoFrame(video, time, presented, failed, visibility)
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
    const initial = observe(new FrameDecoder(), 0)
    const queued = initial.video.callbacks.values().next().value!
    initial.close()
    initial.close()
    initial.video.metadata()
    queued(0, { mediaTime: 0, width: 640, height: 360,
      presentationTime: 0, expectedDisplayTime: 0, presentedFrames: 1 })
    initial.video.dispatchEvent(new Event('error'))
    expect(initial.video.seeks).toEqual([])
    expect(initial.video.cancelled).toEqual([1])
    expect(initial.video.callbacks.size).toBe(0)
    expect(initial.presented).not.toHaveBeenCalled()
    expect(initial.failed).not.toHaveBeenCalled()

    const deferred = observe()
    expect(deferred.video.callbacks.size).toBe(0)
    deferred.close()
    deferred.video.metadata()
    expect(deferred.video.seeks).toEqual([])
    expect(deferred.video.callbacks.size).toBe(0)
    expect(deferred.presented).not.toHaveBeenCalled()
    expect(deferred.failed).not.toHaveBeenCalled()
  })

  it('settles decoder errors once and releases the pending frame callback', () => {
    const initial = observe(new FrameDecoder(), 0)
    initial.video.dispatchEvent(new Event('error'))
    initial.video.dispatchEvent(new Event('error'))
    initial.video.metadata()
    expect(initial.failed).toHaveBeenCalledTimes(1)
    expect(initial.presented).not.toHaveBeenCalled()
    expect(initial.video.cancelled).toEqual([1])
    expect(initial.video.seeks).toEqual([])

    const seeking = observe()
    seeking.video.metadata()
    seeking.video.dispatchEvent(new Event('error'))
    seeking.video.dispatchEvent(new Event('error'))
    expect(seeking.failed).toHaveBeenCalledTimes(1)
    expect(seeking.presented).not.toHaveBeenCalled()
    expect(seeking.video.cancelled).toEqual([1])
    expect(seeking.video.callbacks.size).toBe(0)
  })

  it('defers frame registration until metadata when a seek is needed and recovers once after a stale post-seek frame', () => {
    const beforeSeeked = observe()
    expect(beforeSeeked.video.callbacks.size).toBe(0)
    beforeSeeked.video.metadata()
    expect(beforeSeeked.video.seeks).toEqual([0.1])
    expect(beforeSeeked.video.callbacks.size).toBe(1)

    beforeSeeked.video.frame(0)
    expect(beforeSeeked.video.seeks).toEqual([0.1])
    beforeSeeked.video.seeked()
    expect(beforeSeeked.video.seeks).toEqual([0.1, 0.1])
    // Recovery is one-shot: extra stale frames or seeked events cannot loop.
    beforeSeeked.video.frame(0)
    beforeSeeked.video.seeked()
    expect(beforeSeeked.video.seeks).toEqual([0.1, 0.1])
    beforeSeeked.video.frame(0.1)
    expect(beforeSeeked.presented).toHaveBeenCalledExactlyOnceWith()

    const afterSeeked = observe()
    afterSeeked.video.metadata()
    afterSeeked.video.seeked()
    expect(afterSeeked.video.seeks).toEqual([0.1])
    afterSeeked.video.frame(0)
    expect(afterSeeked.video.seeks).toEqual([0.1, 0.1])
    afterSeeked.video.frame(0)
    expect(afterSeeked.video.seeks).toEqual([0.1, 0.1])
    afterSeeked.video.frame(0.1)
    expect(afterSeeked.presented).toHaveBeenCalledExactlyOnceWith()
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

  it('fails a decoder whose loadedmetadata never arrives', () => {
    vi.useFakeTimers()
    const { video, presented, failed } = observe()
    // The local segment's src is already assigned, so the deadline runs from attachment.
    vi.advanceTimersByTime(VIDEO_FRAME_PRESENTATION_TIMEOUT_MS - 1)
    expect(failed).not.toHaveBeenCalled()
    vi.advanceTimersByTime(1)
    expect(failed).toHaveBeenCalledExactlyOnceWith()
    expect(presented).not.toHaveBeenCalled()
    expect(video.callbacks.size).toBe(0)
    video.metadata()
    expect(video.seeks).toEqual([])
  })

  it('fails a decoder that loads metadata but never presents a frame', () => {
    vi.useFakeTimers()
    const { video, presented, failed } = observe()
    vi.advanceTimersByTime(VIDEO_FRAME_PRESENTATION_TIMEOUT_MS / 2)
    video.metadata()
    video.seeked()
    video.frame(0)
    // Loading metadata does not restart the attempt's deadline.
    vi.advanceTimersByTime(VIDEO_FRAME_PRESENTATION_TIMEOUT_MS / 2 - 1)
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

  it('does not time a decoder while the document is hidden', () => {
    vi.useFakeTimers()
    const visibility = new FakeVisibility()
    visibility.visibilityState = 'hidden'
    const { video, presented, failed } = observe(new FrameDecoder(), 0.1, visibility)
    vi.advanceTimersByTime(VIDEO_FRAME_PRESENTATION_TIMEOUT_MS * 3)
    video.metadata()
    vi.advanceTimersByTime(VIDEO_FRAME_PRESENTATION_TIMEOUT_MS * 3)
    expect(failed).not.toHaveBeenCalled()
    expect(vi.getTimerCount()).toBe(0)

    visibility.set('visible')
    vi.advanceTimersByTime(VIDEO_FRAME_PRESENTATION_TIMEOUT_MS - 1)
    expect(failed).not.toHaveBeenCalled()
    vi.advanceTimersByTime(1)
    expect(failed).toHaveBeenCalledExactlyOnceWith()
    expect(presented).not.toHaveBeenCalled()
  })

  it('pauses on hide and restarts the full deadline when the document is visible again', () => {
    vi.useFakeTimers()
    const visibility = new FakeVisibility()
    const { video, presented, failed } = observe(new FrameDecoder(), 0.1, visibility)
    video.metadata()
    vi.advanceTimersByTime(VIDEO_FRAME_PRESENTATION_TIMEOUT_MS - 1)
    visibility.set('hidden')
    vi.advanceTimersByTime(VIDEO_FRAME_PRESENTATION_TIMEOUT_MS * 5)
    expect(failed).not.toHaveBeenCalled()
    visibility.set('visible')
    vi.advanceTimersByTime(VIDEO_FRAME_PRESENTATION_TIMEOUT_MS - 1)
    expect(failed).not.toHaveBeenCalled()
    video.frame(0.1)
    expect(presented).toHaveBeenCalledExactlyOnceWith()
    vi.advanceTimersByTime(VIDEO_FRAME_PRESENTATION_TIMEOUT_MS)
    expect(failed).not.toHaveBeenCalled()
    expect(vi.getTimerCount()).toBe(0)
    // A settled observer ignores later visibility changes.
    visibility.set('hidden')
    visibility.set('visible')
    expect(vi.getTimerCount()).toBe(0)
  })

  it('stops listening for visibility changes once closed', () => {
    vi.useFakeTimers()
    const visibility = new FakeVisibility()
    const removed = vi.spyOn(visibility, 'removeEventListener')
    const { failed, close } = observe(new FrameDecoder(), 0.1, visibility)
    close()
    expect(removed).toHaveBeenCalledWith('visibilitychange', expect.any(Function))
    visibility.set('hidden')
    visibility.set('visible')
    vi.advanceTimersByTime(VIDEO_FRAME_PRESENTATION_TIMEOUT_MS * 2)
    expect(failed).not.toHaveBeenCalled()
    expect(vi.getTimerCount()).toBe(0)
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
