import { PassThrough } from 'node:stream'
import { once } from 'node:events'

import { describe, expect, it } from 'vitest'

import {
  createOwnedByteChannel,
  normalizeOwnedProcessCapture,
  waitForExactWritableCompletion,
} from '../../scripts/browser-evidence/process/owned-process-channel.mjs'

describe('owned process pull channels', () => {
  it('keeps bounded capture progress independent from a consumer that never resumes', async () => {
    const channel = createOwnedByteChannel(64, 'test stdout')
    const blockedConsumer = (async () => {
      await channel.view[Symbol.asyncIterator]().next()
      await new Promise<never>(() => {
        // The adversarial consumer deliberately never requests another chunk.
      })
    })()
    expect(blockedConsumer).toBeInstanceOf(Promise)

    channel.append(Buffer.from('first', 'utf8'))
    channel.append(Buffer.from('-second', 'utf8'))
    channel.finish()

    const snapshot = channel.view.snapshot()
    expect(snapshot).toMatchObject({
      observedBytes: 12,
      capturedBytes: 12,
      truncated: false,
      completed: true,
    })
    expect(Buffer.from(snapshot.bytes()).toString('utf8')).toBe('first-second')
  })

  it('records overflow monotonically while continuing to count discarded bytes', () => {
    const channel = createOwnedByteChannel(5, 'test stdout')
    channel.append(Buffer.from('first', 'utf8'))
    channel.append(Buffer.from('-discarded', 'utf8'))
    const firstFailure = channel.failure()
    channel.append(Buffer.from('-more', 'utf8'))
    channel.finish()

    expect(channel.failure()).toBe(firstFailure)
    expect(channel.view.snapshot()).toMatchObject({
      observedBytes: 20,
      capturedBytes: 5,
      truncated: true,
      completed: true,
    })
    expect(Buffer.from(channel.view.snapshot().bytes()).toString('utf8')).toBe('first')
  })

  it('returns detached snapshot bytes', () => {
    const channel = createOwnedByteChannel(16, 'test stdout')
    channel.append(Buffer.from('private', 'utf8'))
    channel.finish()
    const first = channel.view.snapshot().bytes()
    first.fill(0)
    expect(Buffer.from(channel.view.snapshot().bytes()).toString('utf8')).toBe('private')
  })

  it('wakes every pending puller on stream completion and iterator cancellation', async () => {
    const channel = createOwnedByteChannel(16, 'test stdout')
    const first = channel.view[Symbol.asyncIterator]()
    const second = channel.view[Symbol.asyncIterator]()
    const cancelledPull = first.next()
    await first.return?.()
    await expect(cancelledPull).resolves.toEqual({ done: true, value: undefined })

    const completedPull = second.next()
    channel.finish()
    await expect(completedPull).resolves.toEqual({ done: true, value: undefined })
  })

  it('bounds capture authority and uses event count zero as the explicit disabled state', () => {
    expect(normalizeOwnedProcessCapture(undefined)).toEqual({
      stdoutBytes: 16_777_216,
      stderrBytes: 16_777_216,
      eventCount: 0,
    })
    expect(() => normalizeOwnedProcessCapture({ stdoutBytes: 0 })).toThrow(
      'owned stdout byte limit must be an integer in [1, 67108864]',
    )
    expect(() => normalizeOwnedProcessCapture({ eventCount: 4_097 })).toThrow(
      'owned event count limit must be an integer in [0, 4096]',
    )
  })
})

describe('exact writable publication', () => {
  it('rejects a transport close that occurs before finish', async () => {
    const stream = new PassThrough()
    const completion = waitForExactWritableCompletion(stream, 'test publication')
    stream.destroy()
    await expect(completion).rejects.toThrow('closed before its bytes finished')
  })

  it('accepts finish as exact publication completion', async () => {
    const stream = new PassThrough()
    const completion = waitForExactWritableCompletion(stream, 'test publication')
    stream.resume()
    stream.end(Buffer.from('complete', 'utf8'))
    await expect(completion).resolves.toBeUndefined()
  })

  it('classifies writable state even when retirement installs its observer after close', async () => {
    const stream = new PassThrough()
    stream.destroy()
    await once(stream, 'close')

    await expect(waitForExactWritableCompletion(stream, 'late test publication'))
      .rejects.toThrow('closed before its bytes finished')
  })
})
