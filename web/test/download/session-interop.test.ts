import {
  Uint8ArrayReader,
  Uint8ArrayWriter,
  ZipReader,
} from '@zip.js/zip.js'
import { describe, expect, it } from 'vitest'

import {
  createZipDownloadSink,
} from '../../src/download'
import { ReceiveSession, encodeBlock } from '../../src/session'
import { MockFrameChannel, settle } from '../session/helpers'
import {
  concat,
  file,
  fixtureContext,
  range,
  recordingOutput,
} from './fixtures'

function block(index: number, value: number): Uint8Array {
  return encodeBlock({
    index: BigInt(index),
    sequence: 0,
    last: true,
    payload: Uint8Array.of(value),
  })
}

describe('ordered sink and receive scheduler interoperability', () => {
  it('buffers out-of-order arrivals and streams the ZIP entry in file order', async () => {
    const context = fixtureContext(
      [file('ordered.bin', 2)],
      new Map([
        [0, [range('ordered.bin', 0, 1)]],
        [1, [range('ordered.bin', 1, 1)]],
      ]),
    )
    const output = recordingOutput()
    const sink = createZipDownloadSink(context, output.stream)
    const channel = new MockFrameChannel()
    const session = new ReceiveSession(
      context.plan,
      sink,
      {
        open: async (_index, ciphertext) => ciphertext.slice(),
      },
      { maxBlockBytes: 16 },
    )
    session.addChannel(channel)

    const completed = session.start()
    await settle()
    channel.push(block(1, 2))
    channel.push(block(0, 1))
    await completed

    expect(session.snapshot().maxBufferedBlocks).toBeGreaterThan(0)
    expect(session.snapshot().maxBufferedBlocks).toBeLessThanOrEqual(2)
    const reader = new ZipReader(new Uint8ArrayReader(concat(output.chunks)))
    const entries = await reader.getEntries()
    const entry = entries[0]
    if (entry === undefined || entry.directory) {
      throw new Error('Expected one file entry')
    }
    expect(await entry.getData(new Uint8ArrayWriter())).toEqual(new Uint8Array([1, 2]))
    await reader.close()
  })
})
