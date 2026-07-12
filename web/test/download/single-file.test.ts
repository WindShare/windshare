import { describe, expect, it } from 'vitest'

import {
  DownloadError,
  createSingleFileDownloadSink,
} from '../../src/download'
import {
  chunk,
  concat,
  file,
  fixtureContext,
  range,
  recordingOutput,
} from './fixtures'

function deferred(): { readonly promise: Promise<void>; readonly resolve: () => void } {
  let resolve!: () => void
  const promise = new Promise<void>((settle) => {
    resolve = settle
  })
  return { promise, resolve }
}

describe('single-file streaming sink', () => {
  it('streams only selected ranges in ascending file order', async () => {
    const context = fixtureContext(
      [file('selected.bin', 4)],
      new Map([
        [0, [range('skipped-prefix.bin', 0, 2), range('selected.bin', 0, 2)]],
        [1, [range('selected.bin', 2, 2), range('skipped-suffix.bin', 0, 1)]],
      ]),
    )
    const output = recordingOutput()
    const sink = createSingleFileDownloadSink(context, output.stream)

    await sink.writeBlock(chunk(0), new Uint8Array([90, 91, 1, 2]))
    await sink.writeBlock(chunk(1), new Uint8Array([3, 4, 92]))
    await sink.finalize()

    expect(sink.deliveryOrder).toBe('ascending')
    expect(sink.has(chunk(0))).toBe(true)
    expect(sink.has(chunk(1))).toBe(true)
    expect(concat(output.chunks)).toEqual(new Uint8Array([1, 2, 3, 4]))
    expect(output.closed).toBe(true)
  })

  it('rejects out-of-order file offsets instead of corrupting the stream', async () => {
    const context = fixtureContext(
      [file('selected.bin', 4)],
      new Map([
        [0, [range('selected.bin', 0, 2)]],
        [1, [range('selected.bin', 2, 2)]],
      ]),
    )
    const output = recordingOutput()
    const sink = createSingleFileDownloadSink(context, output.stream)

    await expect(sink.writeBlock(chunk(1), new Uint8Array([3, 4]))).rejects.toMatchObject({
      code: 'out-of-order',
    } satisfies Partial<DownloadError>)
    expect(output.chunks).toHaveLength(0)
    await sink.abort(new Error('test cleanup'))
  })

  it('finalizes an authenticated empty file without buffering content', async () => {
    const context = fixtureContext([file('empty.bin', 0)], new Map())
    const output = recordingOutput()
    const sink = createSingleFileDownloadSink(context, output.stream)

    await sink.finalize()

    expect(output.chunks).toHaveLength(0)
    expect(output.closed).toBe(true)
  })

  it('aborts the browser stream and clears accepted have-state', async () => {
    const context = fixtureContext(
      [file('selected.bin', 1)],
      new Map([[0, [range('selected.bin', 0, 1)]]]),
    )
    const output = recordingOutput()
    const sink = createSingleFileDownloadSink(context, output.stream)
    const reason = new Error('cancelled')

    await sink.writeBlock(chunk(0), new Uint8Array([1]))
    await sink.abort(reason)

    expect(output.aborted).toBe(reason)
    expect(sink.has(chunk(0))).toBe(false)
  })

  it('validates the selected path before locking the output stream', () => {
    const context = fixtureContext([file('../escape.bin', 0)], new Map())
    const output = recordingOutput()

    expect(() => createSingleFileDownloadSink(context, output.stream)).toThrow()
    expect(output.stream.locked).toBe(false)
  })

  it('does not retain availability when abort overtakes an in-flight stream write', async () => {
    const writeStarted = deferred()
    const releaseWrite = deferred()
    const reason = new Error('cancelled')
    let aborted: unknown
    const output = new WritableStream<Uint8Array>({
      async write() {
        writeStarted.resolve()
        await releaseWrite.promise
      },
      abort(value) {
        aborted = value
      },
    })
    const context = fixtureContext(
      [file('selected.bin', 1)],
      new Map([[0, [range('selected.bin', 0, 1)]]]),
    )
    const sink = createSingleFileDownloadSink(context, output)

    const writing = sink.writeBlock(chunk(0), Uint8Array.of(1))
    await writeStarted.promise
    const aborting = sink.abort(reason)
    releaseWrite.resolve()

    await expect(writing).rejects.toMatchObject({
      code: 'invalid-state',
    } satisfies Partial<DownloadError>)
    await aborting
    expect(aborted).toBe(reason)
    expect(sink.has(chunk(0))).toBe(false)
  })

  it('reports a failed browser abort and still releases the output capability', async () => {
    const failure = new Error('browser abort failed')
    const output = new WritableStream<Uint8Array>({
      abort() {
        throw failure
      },
    })
    const sink = createSingleFileDownloadSink(
      fixtureContext([file('empty.bin', 0)], new Map()),
      output,
    )

    await expect(sink.abort(new Error('cancelled'))).rejects.toMatchObject({
      code: 'cleanup-failed',
      cause: failure,
    } satisfies Partial<DownloadError>)
    expect(output.locked).toBe(false)
  })

  it('keeps an incomplete output abortable after finalization is refused', async () => {
    const context = fixtureContext(
      [file('partial.bin', 2)],
      new Map([[0, [range('partial.bin', 0, 1)]]]),
    )
    const output = recordingOutput()
    const sink = createSingleFileDownloadSink(context, output.stream)
    await sink.writeBlock(chunk(0), Uint8Array.of(1))

    await expect(sink.finalize()).rejects.toMatchObject({
      code: 'output-finalize',
    } satisfies Partial<DownloadError>)
    expect(output.stream.locked).toBe(true)

    const reason = new Error('incomplete output')
    await sink.abort(reason)
    expect(output.aborted).toBe(reason)
    expect(sink.has(chunk(0))).toBe(false)
    expect(output.stream.locked).toBe(false)
  })
})
