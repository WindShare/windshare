import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { createFile, type MP4BoxBuffer, type Sample } from 'mp4box'

import { Mp4RangePreview, VideoPreviewError, type PreviewRangeSource } from '../../src/preview/mp4-range'
import type { ByteRange } from '../../src/content/geometry'

function box(bytes: Uint8Array, offset: number, size: number, type: string): void {
  new DataView(bytes.buffer).setUint32(offset, size, false)
  bytes.set([...type].map((value) => value.charCodeAt(0)), offset + 4)
}

// Offset of the first box of this type at any depth, found by its fourcc.
function findBox(bytes: Uint8Array, type: string): number {
  const code = [...type].map((value) => value.charCodeAt(0))
  for (let offset = 4; offset + 4 <= bytes.byteLength; offset += 1) {
    if (code.every((value, index) => bytes[offset + index] === value)) return offset - 4
  }
  return -1
}

async function segmentSamples(bytes: Uint8Array): Promise<Sample[]> {
  const file = createFile()
  let trackId: number | undefined
  file.onReady = (info) => { trackId = info.videoTracks[0]?.id }
  const buffer = bytes.slice().buffer as MP4BoxBuffer
  buffer.fileStart = 0
  file.appendBuffer(buffer)
  file.flush()
  if (trackId === undefined) throw new Error('Segment has no video track')
  return file.getTrackSamplesInfo(trackId)
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
    expectPositions(await seekPositions([0, 0.14, 1.13, 1.14, 2], 'h264-ts600-empty-edit.mp4'),
      [80 / 600, 80 / 600, 80 / 600, 680 / 600, 680 / 600])
  })

  // 30 fps `-t 3 -g 30 -bf 2` (timescale 15360) copied with `-ss 0.5 -c copy`:
  // edit media_time 8704, keyframes at 7680/15360 = 0.5 s and 1.5 s. A seek
  // exactly on 1.5 s must not fall back a whole GOP to 0.5 s.
  it('selects a keyframe the seek lands on exactly with a nonzero offset', async () => {
    expectPositions(await seekPositions([0, 0.5, 1.49, 1.5, 2.5], 'h264-head-trimmed-on-keyframe.mp4'),
      [0.5, 0.5, 0.5, 1.5, 1.5])
  })

  // Same source copied with `-ss 0.77 -c copy`: edit media_time 12851 is not a
  // multiple of the 512-tick frame, so keyframes are at 3533/15360 s and
  // 18893/15360 s (Chromium presents them there; ffprobe rounds to 0.2 s).
  it('keeps the exact tick offset when the cut is not on a frame boundary', async () => {
    expectPositions(await seekPositions([0, 0.2, 1.2, 1.24, 2.2], 'h264-head-trimmed-mid-frame.mp4'),
      [3_533 / 15_360, 3_533 / 15_360, 3_533 / 15_360, 18_893 / 15_360, 18_893 / 15_360])
  })

  // `-f lavfi -i testsrc=size=64x64:rate=30000/1001 -t 2.1 -g 30 -bf 2
  // -video_track_timescale 30000`: keyframes at 0, 30030 and 60060 ticks. In
  // float math 1.001 * 30000 < 30030, which used to fall back to 0 s.
  it('selects 29.97 fps keyframes at their exact tick', async () => {
    expectPositions(await seekPositions([0, 1, 1.001, 2, 2.002, 2.1], 'h264-ntsc-30000.mp4'),
      [0, 0, 1.001, 1.001, 2.002, 2.002])
  })

  // Slider-style 0.1 s steps: every step reports the last keyframe at or before
  // it (or the first visible keyframe before it), never one a GOP earlier.
  it.each([
    ['h264-bframes-edit-list.mp4', [0, 1]],
    ['h264-leading-empty-edit.mp4', [0.5, 1.5]],
    ['h264-head-trimmed.mp4', [0.5, 1.5]],
    ['h264-head-trimmed-on-keyframe.mp4', [0.5, 1.5]],
    ['h264-head-trimmed-mid-frame.mp4', [3_533 / 15_360, 18_893 / 15_360]],
    ['h264-ntsc-head-trimmed-float.mp4', [7_478 / 30_000, 35_506 / 30_000]],
    ['h264-ntsc-30000.mp4', [0, 1.001, 2.002]],
    ['h264-ts600-empty-edit.mp4', [80 / 600, 680 / 600]],
  ] as const)('sweeps 0.1 s steps over %s without GOP jumps', async (name, keyframes) => {
    const preview = await openFixture(name)
    const steps = Math.floor(preview.metadata.durationSeconds * 10 + 1e-9)
    const targets = Array.from({ length: steps + 1 }, (_, step) => step / 10)
    const expected = targets.map((target) =>
      keyframes.findLast((keyframe) => keyframe <= target + 1e-9) ?? keyframes[0])
    expectPositions(await seekPositions(targets, name), expected)
  })

  // `-g 100` (a single keyframe) copied with `-ss 0.5 -c copy`: the edit list
  // hides the only keyframe (at -0.5 s). One sample can only decode that
  // keyframe, so every target shows it at position 0, earlier than the clip's
  // first visible frame, instead of failing the preview.
  it('shows the hidden keyframe when the edit list hides every keyframe', async () => {
    const signal = new AbortController().signal
    const preview = await openFixture('h264-head-trimmed-single-keyframe.mp4')
    for (const target of [0, 0.7, 1.5]) {
      const segment = await preview.segmentAt(target, signal)
      expect(segment.positionSeconds).toBe(0)
      expect(segment.frameSeconds).toBeCloseTo(2_048 / 10_240, 9)
    }
  })

  // Same `h264-no-edit-list.mp4` with its stss entry count patched to zero:
  // no sample is a keyframe, so open rejects it before any seek.
  it('rejects a file without a presentable keyframe at open', async () => {
    const bytes = fixture('h264-no-edit-list.mp4')
    const stss = findBox(bytes, 'stss')
    new DataView(bytes.buffer).setUint32(stss + 12, 0, false)
    await expect(Mp4RangePreview.open(new RecordedSource(bytes), new AbortController().signal))
      .rejects.toThrow(new VideoPreviewError('MP4 has no keyframe on its presentation timeline'))
  })

  // The decoder element gets the sample's raw composition time, since the
  // one-sample segment carries no edit list. The no-B-frame fixtures are
  // `-f lavfi -i testsrc=size=64x64:rate=10 -c:v libx264 -g 10 -bf 0 -pix_fmt
  // yuv420p -fflags +bitexact -map_metadata -1`, with `-itsoffset 0.5` before
  // `-i` and `-t 2` (leading empty edit: keyframes at 0.5 s and 1.5 s, raw cts
  // 0 and 1 s), or `-t 3` copied with `-ss 0.5 -c copy` (head-trimmed:
  // keyframes at 0.5 s and 1.5 s, raw cts 1 and 2 s).
  it.each([
    ['h264-bframes-edit-list.mp4', 0, 0, 2_048 / 10_240],
    ['h264-bframes-edit-list.mp4', 1.5, 1, 12_288 / 10_240],
    ['h264-leading-empty-edit.mp4', 0, 0.5, 2_048 / 10_240],
    ['h264-leading-empty-edit-no-bframes.mp4', 0, 0.5, 0],
    ['h264-leading-empty-edit-no-bframes.mp4', 1.5, 1.5, 1],
    ['h264-head-trimmed.mp4', 0, 0.5, 12_288 / 10_240],
    ['h264-head-trimmed-no-bframes.mp4', 0, 0.5, 1],
    ['h264-head-trimmed-no-bframes.mp4', 1.5, 1.5, 2],
    ['h264-no-edit-list.mp4', 1, 1, 1],
  ] as const)('reports %s at %f s as position %f s, frame %f s', async (name, target, position, frame) => {
    const preview = await openFixture(name)
    const segment = await preview.segmentAt(target, new AbortController().signal)
    expect(segment.positionSeconds).toBeCloseTo(position, 9)
    expect(segment.frameSeconds).toBeCloseTo(frame, 9)
    const bytes = new Uint8Array(await segment.bytes.arrayBuffer())
    expect(findBox(fixture(name), 'edts') >= 0 || name === 'h264-no-edit-list.mp4').toBe(true)
    expect(findBox(bytes, 'edts')).toBe(-1)
    const samples = await segmentSamples(bytes)
    expect(samples).toHaveLength(1)
    expect(samples[0]).toMatchObject({ is_sync: true, cts: Math.round(frame * 10_240) })
  })

  // Open-GOP x264 (`-t 3 -g 12 -bf 2 -x264-params
  // open-gop=1:scenecut=0:b-pyramid=0:b-adapt=0`) copied with `-frames:v 13`:
  // decode order ends with the keyframe at 1.2 s followed only by its two
  // leading B-frames, so the last decode-order sample ends at that keyframe's
  // cts. mp4box's time-based seek skips a track whose last sample ends before
  // the target and silently keeps sample 0; the seek must still reach it.
  it('reaches a last keyframe followed only by its leading frames', async () => {
    const signal = new AbortController().signal
    const preview = await openFixture('h264-open-gop-leading-tail.mp4')
    for (const target of [1.2, 1.3]) {
      const segment = await preview.segmentAt(target, signal)
      expect(segment.positionSeconds).toBeCloseTo(1.2, 9)
      expect(segment.frameSeconds).toBeCloseTo(13_312 / 10_240, 9)
      const samples = await segmentSamples(new Uint8Array(await segment.bytes.arrayBuffer()))
      expect(samples).toMatchObject([{ is_sync: true, cts: 13_312 }])
    }
    expect((await preview.segmentAt(1.1, signal)).positionSeconds).toBe(0)
  })
})
