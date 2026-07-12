import { describe, expect, it } from 'vitest'

import { createSingleFileDownloadSink } from '../../src/download'
import { ReceiveSession, encodeBlock } from '../../src/session'
import {
  file,
  fixtureContext,
  range,
  recordingOutput,
} from '../download/fixtures'
import { MockFrameChannel, settle } from './helpers'

function response(index: number, payload: readonly number[]): Uint8Array {
  return encodeBlock({
    index: BigInt(index),
    sequence: 0,
    last: true,
    payload: Uint8Array.from(payload),
  })
}

describe('receive-session download integration', () => {
  it('honors the real C3 ordered sink capability without composition flags', async () => {
    const context = fixtureContext(
      [file('result.bin', 4)],
      new Map([
        [0, [range('result.bin', 0, 2)]],
        [1, [range('result.bin', 2, 2)]],
      ]),
    )
    const output = recordingOutput()
    const sink = createSingleFileDownloadSink(context, output.stream)
    const channel = new MockFrameChannel()
    const session = new ReceiveSession(
      context.plan,
      sink,
      { open: (_index, ciphertext) => ciphertext.slice() },
      { maxBlockBytes: 16 },
    )
    session.addChannel(channel)
    const completed = session.start()
    await settle()

    channel.push(response(1, [3, 4]))
    await settle()
    expect(output.chunks).toHaveLength(0)
    channel.push(response(0, [1, 2]))
    await completed

    expect(output.chunks).toEqual([Uint8Array.of(1, 2), Uint8Array.of(3, 4)])
    expect(output.closed).toBe(true)
    expect(session.state).toBe('completed')
  })
})
