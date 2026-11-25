import { describe, expect, it, vi } from 'vitest'
import { SelectionPolicy } from '../../src/catalog/selection'
import { REVISION_CODE_INVALID_LEASE, REVISION_CODE_LEASE_EXPIRED, REVISION_CODE_QUOTA } from '../../src/content/flow'
import { byteRange, type ByteRange } from '../../src/content/geometry'
import {
  RemoteOperationError,
  RevisionCapacityBusyError,
  RevisionLeaseExpiredError,
} from '../../src/content/session-services'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { createReceivedProtocolError } from '../../src/session/diagnostics'
import { createProtocolOperationIdentity, createProtocolSessionIdentity } from '../../src/session/identities'
import { TransferJob } from '../../src/transfer/job'
import { id, makeShareFixture, outputFile } from './share-fixture'

const selection = new SelectionPolicy(true).snapshot()

function protocolCorrelation() {
  return Object.freeze({
    protocolSessionId: createProtocolSessionIdentity(id(30)),
    protocolOperationId: createProtocolOperationIdentity(31n),
  })
}

function capacityBusyError(retryAfterMilliseconds = 30_000): RevisionCapacityBusyError {
  return new RevisionCapacityBusyError(createReceivedProtocolError({
    requestKind: 'open_revisions',
    correlation: protocolCorrelation(),
    content: {
      scope: 'revision',
      code: REVISION_CODE_QUOTA,
      retryable: true,
      retryAfterMilliseconds,
    },
  }))
}

function leaseOperationError(code: number): RemoteOperationError {
  return new RemoteOperationError(createReceivedProtocolError({
    requestKind: 'request_blocks',
    correlation: protocolCorrelation(),
    content: {
      scope: 'revision',
      code,
      retryable: true,
    },
  }))
}

describe('TransferJob recoverable remote budget exhaustion', () => {
  it('pauses resumable transfers without file errors when sender capacity wait exhausts its budget', async () => {
    vi.useFakeTimers()
    try {
      const done = { seed: 61, name: 'done.bin', bytes: new Uint8Array([1, 2, 3, 4]) }
      const busy = { seed: 62, name: 'busy.bin', bytes: new Uint8Array([5, 6, 7, 8]) }
      const busyId = encodeBase64Url(id(busy.seed))
      const fixture = makeShareFixture([done, busy])
      let busyAttempts = 0

      const run = new TransferJob({
        ...fixture,
        selection,
        revisions: {
          open: async (fileId, signal) => {
            if (encodeBase64Url(fileId) === busyId) {
              busyAttempts += 1
              throw capacityBusyError(30_000)
            }
            return fixture.revisions.open(fileId, signal)
          },
        },
        output: {
          sessionId: 'sess-capacity-pause',
          capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
          openFile: async () => outputFile({}),
        },
      }).run()

      await vi.advanceTimersByTimeAsync(60_000)
      const result = await run

      expect(result.status).toBe('paused')
      expect(result.pauseReason).toBe('interrupted')
      expect(result.completedFiles).toBe(1n)
      expect(result.failedFiles).toBe(0n)
      expect(result.sourceRevisionFailures).toBeUndefined()
      expect(result.contentWarning).toBeUndefined()
      expect(busyAttempts).toBe(3)
    } finally {
      vi.useRealTimers()
    }
  })

  it('keeps non-resumable capacity wait exhaustion failed without misreporting source-unavailable', async () => {
    vi.useFakeTimers()
    try {
      const busy = { seed: 63, name: 'busy.bin', bytes: new Uint8Array([1, 2, 3, 4]) }
      const fixture = makeShareFixture([busy])

      const run = new TransferJob({
        ...fixture,
        selection,
        revisions: { open: async () => { throw capacityBusyError(30_000) } },
        output: {
          sessionId: 'sess-capacity-portable',
          capabilities: { maxConcurrentFiles: 1, resumable: false, partialOutput: false },
          openFile: async () => outputFile({}),
        },
      }).run()

      await vi.advanceTimersByTimeAsync(60_000)
      const result = await run

      expect(result.status).toBe('failed')
      expect(result.failedFiles).toBe(1n)
      expect(result.error).toBeInstanceOf(RevisionCapacityBusyError)
      expect(result.sourceRevisionFailures).toBeUndefined()
      expect(result.contentWarning?.missingFiles).toEqual([{ path: ['busy.bin'], reason: 'file-failed' }])
    } finally {
      vi.useRealTimers()
    }
  })

  it('resets lease replacement retries after durable progress so long transfers complete', async () => {
    const payload = new Uint8Array(20)
    for (let index = 0; index < payload.length; index += 1) payload[index] = index + 1
    const fixture = makeShareFixture([{ seed: 64, name: 'large.bin', bytes: payload }])
    const disk = new Uint8Array(payload.length)
    let savedRanges: readonly ByteRange[] = []
    let checkpoints = 0

    const result = await new TransferJob({
      ...fixture,
      selection,
      broker: {
        readRange: async function* (_desc, _lease, span) {
          const sliceEnd = span.start + 4n < span.end ? span.start + 4n : span.end
          yield { offset: span.start, data: payload.subarray(Number(span.start), Number(sliceEnd)) }
          if (sliceEnd < span.end) throw new RevisionLeaseExpiredError()
        },
      },
      output: {
        sessionId: 'sess-lease-progress',
        capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
        openFile: async () => outputFile({
          completedRanges: savedRanges,
          writeAt: async (offset, data) => { disk.set(data, Number(offset)) },
          checkpoint: async ranges => {
            checkpoints += 1
            savedRanges = ranges
            return 'saved'
          },
        }),
      },
    }).run()

    expect(result.status).toBe('completed')
    expect(result.completedBytes).toBe(20n)
    expect(checkpoints).toBe(4)
    expect(fixture.openedFileIds).toHaveLength(5)
    expect([...disk]).toEqual([...payload])
  })

  it.each([
    ['RevisionLeaseExpiredError', () => new RevisionLeaseExpiredError()],
    ['REVISION_CODE_LEASE_EXPIRED', () => leaseOperationError(REVISION_CODE_LEASE_EXPIRED)],
    ['REVISION_CODE_INVALID_LEASE', () => leaseOperationError(REVISION_CODE_INVALID_LEASE)],
  ] as const)('pauses resumable transfers when stalled %s replacements exhaust retries', async (_label, makeError) => {
    const payload = new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8])
    const fixture = makeShareFixture([{ seed: 65, name: 'clip.bin', bytes: payload }])
    let savedRanges: readonly ByteRange[] = []
    let attempts = 0

    const result = await new TransferJob({
      ...fixture,
      selection,
      broker: {
        readRange: async function* (_desc, _lease, span) {
          attempts += 1
          if (attempts === 1) {
            yield { offset: span.start, data: payload.subarray(0, 4) }
          }
          throw makeError()
        },
      },
      output: {
        sessionId: 'sess-lease-pause',
        capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
        openFile: async () => outputFile({
          completedRanges: savedRanges,
          checkpoint: async ranges => {
            savedRanges = ranges
            return 'saved'
          },
        }),
      },
    }).run()

    expect(result.status).toBe('paused')
    expect(result.pauseReason).toBe('interrupted')
    expect(result.failedFiles).toBe(0n)
    expect(result.contentWarning).toBeUndefined()
    expect(attempts).toBe(5)
    expect(savedRanges).toEqual([byteRange(0n, 4n)])
  })

  it('keeps non-resumable lease replacement failures terminal without retrying', async () => {
    const fixture = makeShareFixture([{ seed: 66, name: 'portable.bin', bytes: new Uint8Array([1, 2, 3, 4]) }])
    const abort = vi.fn(async () => undefined)

    const result = await new TransferJob({
      ...fixture,
      selection,
      broker: {
        readRange: async function* () {
          yield await Promise.reject(new RevisionLeaseExpiredError())
        },
      },
      output: {
        sessionId: 'sess-lease-portable',
        capabilities: { maxConcurrentFiles: 1, resumable: false, partialOutput: false },
        openFile: async () => outputFile({ abort }),
      },
    }).run()

    expect(result.status).toBe('failed')
    expect(result.failedFiles).toBe(1n)
    expect(fixture.openedFileIds).toHaveLength(1)
    expect(abort).toHaveBeenCalledOnce()
  })
})
