import { describe, expect, it, vi } from 'vitest'
import { observeVideoFrame } from '../../src/ui/share-content/video-frame-presentation'

class FrameDecoder extends EventTarget {
  readonly HAVE_METADATA = 1
  readyState = 0
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
