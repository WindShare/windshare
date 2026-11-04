import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'

import { Mp4RangePreview, VideoPreviewError, type PreviewRangeSource } from '../../src/preview/mp4-range'
import type { ByteRange } from '../../src/content/geometry'

function box(bytes: Uint8Array, offset: number, size: number, type: string): void {
  new DataView(bytes.buffer).setUint32(offset, size, false)
  bytes.set([...type].map((value) => value.charCodeAt(0)), offset + 4)
}

class RecordedSource implements PreviewRangeSource {
  readonly exactSize: bigint
  readonly ranges: ByteRange[] = []
  readonly #bytes: Uint8Array<ArrayBuffer>

  constructor(bytes: Uint8Array<ArrayBuffer>) {
    this.#bytes = bytes
    this.exactSize = BigInt(bytes.byteLength)
  }

  async read(range: ByteRange, signal: AbortSignal): Promise<Uint8Array<ArrayBuffer>> {
    signal.throwIfAborted()
    this.ranges.push(range)
    return this.#bytes.slice(Number(range.start), Number(range.end))
  }
}

describe('v2 MP4 bounded range parser', () => {
  it('jumps over mdat without reading the full file and probes both metadata edges', async () => {
    const bytes = new Uint8Array(1 << 20)
    box(bytes, 0, 24, 'ftyp')
    box(bytes, 24, bytes.byteLength - 32, 'mdat')
    box(bytes, bytes.byteLength - 8, 8, 'moov')
    const source = new RecordedSource(bytes)

    await expect(Mp4RangePreview.open(source, new AbortController().signal))
      .rejects.toBeInstanceOf(VideoPreviewError)
    expect(source.ranges[0]).toEqual({ start: 0n, end: 256n * 1024n })
    expect(source.ranges[1]).toEqual({
      start: BigInt(bytes.byteLength - 256 * 1024),
      end: BigInt(bytes.byteLength),
    })
    expect(source.ranges.every((range) => range.end - range.start <= 256n * 1024n)).toBe(true)
    expect(source.ranges.some((range) => range.start === 0n && range.end === BigInt(bytes.byteLength)))
      .toBe(false)
  })

  it('rejects invalid top-level geometry before media decode', async () => {
    const bytes = new Uint8Array(1_024)
    box(bytes, 0, 24, 'ftyp')
    box(bytes, 24, 2_000, 'mdat')
    const source = new RecordedSource(bytes)
    await expect(Mp4RangePreview.open(source, new AbortController().signal))
      .rejects.toThrow('invalid size')
  })
})

describe('v2 MP4 edit-list presentation time', () => {
  // `ffmpeg -f lavfi -i testsrc=size=64x64:rate=10 -t 2 -c:v libx264 -g 10 -bf 2
  // -pix_fmt yuv420p -fflags +bitexact -map_metadata -1`: keyframes are presented
  // at 0 s and 1 s, and B-frames shift composition time by 0.2 s in the edit list.
  function fixture(name: string): Uint8Array<ArrayBuffer> {
    return new Uint8Array(readFileSync(new URL(`../fixtures/preview/${name}`, import.meta.url)))
  }

  async function openFixture(name: string): Promise<Mp4RangePreview> {
    return Mp4RangePreview.open(new RecordedSource(fixture(name)), new AbortController().signal)
  }

  async function seekPositions(targets: readonly number[], name = 'h264-bframes-edit-list.mp4'): Promise<number[]> {
    const signal = new AbortController().signal
    const preview = await openFixture(name)
    const positions: number[] = []
    for (const target of targets) positions.push((await preview.segmentAt(target, signal)).positionSeconds)
    return positions
  }

  function expectPositions(actual: readonly number[], expected: readonly number[]): void {
    expect(actual).toHaveLength(expected.length)
    for (const [index, value] of expected.entries()) expect(actual[index]).toBeCloseTo(value, 6)
  }

  it('seeks and reports keyframes on the presentation timeline', async () => {
    const [start, second, afterSecond, end] = await seekPositions([0, 1, 1.1, 2])
    expect(start).toBeCloseTo(0, 6)
    expect(second).toBeCloseTo(1, 6)
    expect(afterSecond).toBeCloseTo(1, 6)
    // The authenticated duration ends after the last raw decode-order sample.
    expect(end).toBeCloseTo(1, 6)
  })

  // Same encode with `-itsoffset 0.5` before `-i`: a 0.5 s empty edit precedes
  // the 0.2 s B-frame shift, so keyframes are presented at 0.5 s and 1.5 s.
  it('places keyframes after a leading empty edit', async () => {
    expectPositions(await seekPositions([0, 0.4, 1.4, 1.5, 2], 'h264-leading-empty-edit.mp4'),
      [0.5, 0.5, 0.5, 1.5, 1.5])
  })

  // Same encode with `-bf 0 -use_editlist 0`: no edit list, composition time is
  // presentation time, and positions match the raw keyframe times.
  it('keeps raw keyframe times when the file has no edit list', async () => {
    expectPositions(await seekPositions([0, 0.9, 1, 1.5, 2], 'h264-no-edit-list.mp4'), [0, 0, 1, 1, 1])
  })

  // A 3 s `-g 10 -bf 2` encode copied with `-ss 0.5 -c copy`: the edit list hides
  // the keyframe before the cut (presented at -0.5 s), so the first keyframe on
  // the timeline is 0.5 s. Reporting 0 would leave the browser waiting forever.
  it('skips a head-trimmed keyframe that is presented before zero', async () => {
    expectPositions(await seekPositions([0, 0.4, 0.5, 1, 1.5, 2.5], 'h264-head-trimmed.mp4'),
      [0.5, 0.5, 0.5, 0.5, 1.5, 1.5])
  })

  // 29.97 fps `-t 2 -g 28 -bf 2` copied with `-ss 0.685 -c copy` (timescale 30000,
  // edit media_time 22552). The first visible keyframe has cts 30030, so it is at
  // 7478/30000 s. 30030 / 30000 * 30000 < 30030 in float math, so seeking mp4box
  // to exactly cts / timescale picks the hidden keyframe before it.
  it('selects the first visible keyframe when cts / timescale rounds down', async () => {
    expectPositions(await seekPositions([0, 0.2, 0.5, 1.2], 'h264-ntsc-head-trimmed-float.mp4'),
      [7_478 / 30_000, 7_478 / 30_000, 7_478 / 30_000, 35_506 / 30_000])
    expect(7_478 / 30_000).toBeCloseTo(0.249267, 6)
  })

  // `-itsoffset 0.1234 -f lavfi -i testsrc=size=64x64:rate=30 -t 2 -g 30 -bf 2
  // -video_track_timescale 600`: the empty edit is 133 ms in the 1000 Hz movie
  // timescale, which the track presents as 80 ticks (80/600 s), followed by the
  // 40-tick B-frame shift. Keyframes are at 80/600 s and 680/600 s.
  it('rounds a leading empty edit to track-timescale ticks', async () => {
    expectPositions(await seekPositions([0, 0.14, 1.133, 1.14, 2], 'h264-ts600-empty-edit.mp4'),
      [80 / 600, 80 / 600, 80 / 600, 680 / 600, 680 / 600])
  })

  // `-g 100` (a single keyframe) copied with `-ss 0.5 -c copy`: no keyframe is on
  // the presentation timeline, so the preview must fail instead of reporting 0.
  it('rejects a trimmed clip with no keyframe on the presentation timeline', async () => {
    const preview = await openFixture('h264-head-trimmed-single-keyframe.mp4')
    await expect(preview.segmentAt(0, new AbortController().signal))
      .rejects.toThrow(VideoPreviewError)
    await expect(preview.segmentAt(1, new AbortController().signal))
      .rejects.toThrow('trimmed without a full frame to show')
  })
})
