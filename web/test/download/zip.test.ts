import {
  Uint8ArrayReader,
  Uint8ArrayWriter,
  ZipReader,
} from '@zip.js/zip.js'
import { describe, expect, it } from 'vitest'

import type { UnixMilliseconds } from '../../src/contracts'
import {
  DownloadError,
  createZipDownloadSink,
} from '../../src/download'
import {
  chunk,
  concat,
  directory,
  file,
  fixtureContext,
  range,
  recordingOutput,
} from './fixtures'

const ZIP64_END_SIGNATURE = new Uint8Array([0x50, 0x4b, 0x06, 0x06])
const ZIP_MIN_DATE_MS = new Date(1980, 0, 1).getTime()
const ZIP_MAX_DATE_MS = new Date(2107, 11, 31).getTime()

function includesBytes(haystack: Uint8Array, needle: Uint8Array): boolean {
  outer: for (let offset = 0; offset <= haystack.byteLength - needle.byteLength; offset += 1) {
    for (let index = 0; index < needle.byteLength; index += 1) {
      if (haystack[offset + index] !== needle[index]) {
        continue outer
      }
    }
    return true
  }
  return false
}

describe('streaming Zip64 sink', () => {
  it('streams store-mode Zip64 entries with selected content and authenticated metadata', async () => {
    const entries = [
      directory('tree'),
      file('tree/a.bin', 3),
      file('tree/empty.bin', 0),
      directory('tree/empty-dir'),
      file('tree/b.bin', 2),
    ]
    const context = fixtureContext(
      entries,
      new Map([
        [0, [range('tree/skipped.bin', 0, 2), range('tree/a.bin', 0, 3)]],
        [1, [range('tree/other.bin', 0, 1), range('tree/b.bin', 0, 2)]],
      ]),
    )
    const output = recordingOutput()
    const sink = createZipDownloadSink(context, output.stream)

    await sink.writeBlock(chunk(0), new Uint8Array([90, 91, 1, 2, 3]))
    expect(output.chunks.length).toBeGreaterThan(0)
    await sink.writeBlock(chunk(1), new Uint8Array([92, 4, 5]))
    await sink.finalize()

    const archive = concat(output.chunks)
    expect(output.closed).toBe(true)
    expect(includesBytes(archive, ZIP64_END_SIGNATURE)).toBe(true)

    const reader = new ZipReader(new Uint8ArrayReader(archive))
    const archived = await reader.getEntries()
    expect(archived.map((entry) => entry.filename)).toEqual([
      'tree/',
      'tree/a.bin',
      'tree/empty.bin',
      'tree/empty-dir/',
      'tree/b.bin',
    ])
    expect(archived.every((entry) => entry.zip64)).toBe(true)
    expect(archived.every((entry) => entry.compressionMethod === 0)).toBe(true)

    const files = new Map<string, Uint8Array>()
    for (const entry of archived) {
      if (!entry.directory) {
        files.set(entry.filename, await entry.getData(new Uint8ArrayWriter()))
      }
    }
    expect(files.get('tree/a.bin')).toEqual(new Uint8Array([1, 2, 3]))
    expect(files.get('tree/empty.bin')).toHaveLength(0)
    expect(files.get('tree/b.bin')).toEqual(new Uint8Array([4, 5]))
    expect(files.has('tree/skipped.bin')).toBe(false)
    expect(archived[1]?.lastModDate.getTime()).toBe(entries[1]?.mtime)
    await reader.close()
  })

  it('rejects out-of-order file offsets and aborts the archive stream', async () => {
    const context = fixtureContext(
      [file('file.bin', 4)],
      new Map([
        [0, [range('file.bin', 0, 2)]],
        [1, [range('file.bin', 2, 2)]],
      ]),
    )
    const output = recordingOutput()
    const sink = createZipDownloadSink(context, output.stream)
    const reason = new Error('cancelled')

    await expect(sink.writeBlock(chunk(1), new Uint8Array([3, 4]))).rejects.toMatchObject({
      code: 'out-of-order',
    } satisfies Partial<DownloadError>)
    expect(output.chunks).toHaveLength(0)
    await sink.abort(reason)

    expect(output.aborted).toBe(reason)
    expect(sink.has(chunk(1))).toBe(false)
  })

  it('validates every ZIP path before locking or writing the browser stream', () => {
    const context = fixtureContext([file('../escape.bin', 0)], new Map())
    const output = recordingOutput()

    expect(() => createZipDownloadSink(context, output.stream)).toThrow()
    expect(output.stream.locked).toBe(false)
    expect(output.chunks).toHaveLength(0)
  })

  it('reports a failed browser abort and still clears the output capability', async () => {
    const failure = new Error('browser abort failed')
    const output = new WritableStream<Uint8Array>({
      abort() {
        throw failure
      },
    })
    const sink = createZipDownloadSink(fixtureContext([], new Map()), output)

    await expect(sink.abort(new Error('cancelled'))).rejects.toMatchObject({
      code: 'cleanup-failed',
    } satisfies Partial<DownloadError>)
    expect(output.locked).toBe(false)
  })

  it('aborts partial archive output when finalization detects a missing suffix', async () => {
    const context = fixtureContext(
      [file('partial.bin', 2)],
      new Map([[0, [range('partial.bin', 0, 1)]]]),
    )
    const output = recordingOutput()
    const sink = createZipDownloadSink(context, output.stream)
    await sink.writeBlock(chunk(0), Uint8Array.of(1))

    await expect(sink.finalize()).rejects.toMatchObject({
      code: 'output-finalize',
    } satisfies Partial<DownloadError>)
    expect(output.aborted).toBeInstanceOf(DownloadError)
    expect(output.stream.locked).toBe(false)
    expect(sink.has(chunk(0))).toBe(false)
  })

  it('clamps authenticated mtimes to the portable ZIP domain', async () => {
    const context = fixtureContext(
      [
        file('old.bin', 0, Date.UTC(1900, 0, 1) as UnixMilliseconds),
        file('future.bin', 0, Date.UTC(2200, 0, 1) as UnixMilliseconds),
      ],
      new Map(),
    )
    const output = recordingOutput()
    const sink = createZipDownloadSink(context, output.stream)

    await sink.finalize()

    const reader = new ZipReader(new Uint8ArrayReader(concat(output.chunks)))
    const entries = await reader.getEntries()
    expect(entries.map((entry) => entry.lastModDate.getTime())).toEqual([
      ZIP_MIN_DATE_MS,
      ZIP_MAX_DATE_MS,
    ])
    await reader.close()
  })
})
