import type { Page } from '@playwright/test'
import { BROWSER_CONTRACT_HOST_PATH } from './contract-host'

const FIXTURE_DIRECTORY = '/test/fixtures/preview'
// Longer than the production observer's own presentation deadline, so a stalled
// decoder reports 'failed' from production code before the harness gives up.
const HARNESS_TIMEOUT_MS = 15_000

export interface FrameObservation {
  readonly target: number
  readonly outcome: 'presented' | 'failed' | 'timeout'
  readonly keyframeSeconds: number
  readonly decodeSeconds: number
  readonly mediaTime: number | null
  readonly currentTime: number
}

// Runs production segmentAt and observeVideoFrame against a real <video> for
// each target. Only the frame callback is wrapped, to read the mediaTime of the
// frame the observer accepted.
export async function presentSegments(
  page: Page,
  fixture: string,
  targets: readonly number[],
): Promise<FrameObservation[]> {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  return page.evaluate(async ({ fixturePath, targets, timeoutMs }) => {
    const mp4RangePath = '/src/preview/mp4-range.ts'
    const presentationPath = '/src/ui/share-content/video-frame-presentation.ts'
    const [mp4RangeModule, presentationModule] = await Promise.all([
      import(mp4RangePath) as Promise<typeof import('../../src/preview/mp4-range')>,
      import(presentationPath) as Promise<typeof import('../../src/ui/share-content/video-frame-presentation')>,
    ])

    const response = await fetch(fixturePath)
    if (!response.ok) throw new Error(`Fixture returned HTTP ${response.status}`)
    const bytes = new Uint8Array(await response.arrayBuffer())
    const source = {
      exactSize: BigInt(bytes.byteLength),
      async read(range: { readonly start: bigint; readonly end: bigint }, signal: AbortSignal) {
        signal.throwIfAborted()
        return bytes.slice(Number(range.start), Number(range.end))
      },
    }
    const signal = new AbortController().signal
    const preview = await mp4RangeModule.Mp4RangePreview.open(source, signal)

    const observations = []
    for (const target of targets) {
      const segment = await preview.segmentAt(target, signal)
      const url = URL.createObjectURL(segment.bytes)
      const video = document.createElement('video')
      video.muted = true
      video.playsInline = true
      video.preload = 'auto'
      video.hidden = true
      let lastMediaTime: number | undefined
      const decoder = {
        get currentTime() { return video.currentTime },
        set currentTime(value: number) { video.currentTime = value },
        get duration() { return video.duration },
        get seeking() { return video.seeking },
        get readyState() { return video.readyState },
        HAVE_METADATA: video.HAVE_METADATA,
        HAVE_CURRENT_DATA: video.HAVE_CURRENT_DATA,
        addEventListener: video.addEventListener.bind(video),
        removeEventListener: video.removeEventListener.bind(video),
        cancelVideoFrameCallback: video.cancelVideoFrameCallback.bind(video),
        requestVideoFrameCallback: (callback: VideoFrameRequestCallback) =>
          video.requestVideoFrameCallback((now, metadata) => {
            lastMediaTime = metadata.mediaTime
            callback(now, metadata)
          }),
      } as Parameters<typeof presentationModule.observeVideoFrame>[0]
      const outcome = await new Promise<'presented' | 'failed' | 'timeout'>((resolve) => {
        let close = () => {}
        const timer = setTimeout(() => { close(); resolve('timeout') }, timeoutMs)
        close = presentationModule.observeVideoFrame(
          decoder,
          segment.decodeSeconds,
          () => { clearTimeout(timer); resolve('presented') },
          () => { clearTimeout(timer); resolve('failed') },
        )
        document.body.append(video)
        video.src = url
      })
      observations.push({
        target,
        outcome,
        keyframeSeconds: segment.keyframeSeconds,
        decodeSeconds: segment.decodeSeconds,
        mediaTime: lastMediaTime ?? null,
        currentTime: video.currentTime,
      })
      video.removeAttribute('src')
      video.load()
      video.remove()
      URL.revokeObjectURL(url)
    }
    return observations
  }, { fixturePath: `${FIXTURE_DIRECTORY}/${fixture}`, targets, timeoutMs: HARNESS_TIMEOUT_MS })
}
