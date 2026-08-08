import { describe, expect, it, vi } from 'vitest'

import { byteRange, type ByteRange } from '../../src/content/geometry'
import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  OutputCheckpointContractError,
  OutputTransactionContractError,
  bindOutputFileTransaction,
} from '../../src/transfer/output-file-transaction'
import {
  VerifiedDurableRanges,
  type BeginOutputFileResult,
  type OutputFile,
  type OutputFileOwnership,
  type OutputSession,
} from '../../src/transfer/output-session'

const sessionIdentity = Object.freeze({ backend: 'test', outputSessionId: 'session' })
const signal = new AbortController().signal
const durableSession: Pick<OutputSession, 'identity' | 'capabilities'> = Object.freeze({
  identity: sessionIdentity,
  capabilities: Object.freeze({
    durability: 'ProcessRestart', randomWrite: true, fileFailureIsolation: true, modificationTime: false,
  }),
})
const transientSession: Pick<OutputSession, 'identity' | 'capabilities'> = Object.freeze({
  identity: sessionIdentity,
  capabilities: Object.freeze({
    durability: 'None', randomWrite: false, fileFailureIsolation: false, modificationTime: false,
  }),
})
const file: OutputFile = Object.freeze({
  source: Object.freeze({
    shareInstance: identityText(1),
    fileId: identityText(2),
    fileRevision: identityText(3),
  }),
  path: Object.freeze(['file.bin']),
  exactSize: 2n,
})
const ownership: OutputFileOwnership = Object.freeze({
  ...sessionIdentity,
  canonicalPath: file.path,
  ownedFileIdentity: 'owned-file',
})

describe('bound output transaction durability evidence', () => {
  it('rejects a non-advancing checkpoint after a completed write', async () => {
    const begun = adapter([byteRange(0n, 1n)], [byteRange(0n, 1n)])
    const bound = bindOutputFileTransaction(begun, file, durableSession)

    await bound.transaction.writeRange(1n, Uint8Array.of(7), signal)

    await expect(bound.transaction.checkpoint(signal)).rejects
      .toBeInstanceOf(OutputCheckpointContractError)
  })

  it('does not let a partial durable snapshot reach adapter commit', async () => {
    const begun = adapter([byteRange(0n, 1n)], [byteRange(0n, 1n)])
    const bound = bindOutputFileTransaction(begun, file, durableSession)

    await expect(bound.transaction.commit(signal)).rejects
      .toBeInstanceOf(OutputCheckpointContractError)
    expect(begun.transaction.commit).not.toHaveBeenCalled()
  })

  it('rejects a checkpoint that invents unwritten future durability', async () => {
    const begun = adapter([], [byteRange(0n, 2n)])
    const bound = bindOutputFileTransaction(begun, file, durableSession)

    await bound.transaction.writeRange(0n, Uint8Array.of(7), signal)

    await expect(bound.transaction.checkpoint(signal)).rejects
      .toBeInstanceOf(OutputCheckpointContractError)
  })

  it('rejects an adapter-authored abort disposition outside the closed contract', async () => {
    const begun = adapter([], [], 'forged')
    const bound = bindOutputFileTransaction(begun, file, durableSession)

    await expect(bound.transaction.retire(new Error('stop'))).rejects
      .toBeInstanceOf(OutputTransactionContractError)
  })

  it('tracks complete transient stream coverage without inventing durable ranges', async () => {
    const begun = adapter([], [])
    const bound = bindOutputFileTransaction(begun, file, transientSession)

    await bound.transaction.writeRange(0n, Uint8Array.of(1), signal)
    await expect(bound.transaction.checkpoint(signal)).resolves.toMatchObject({ ranges: [] })
    await bound.transaction.writeRange(1n, Uint8Array.of(2), signal)
    await expect(bound.transaction.checkpoint(signal)).resolves.toMatchObject({ ranges: [] })
    await expect(bound.transaction.commit(signal)).resolves.toBeUndefined()
    expect(begun.transaction.commit).toHaveBeenCalledOnce()
  })

  it('rejects gaps and incomplete transient streams before adapter commit', async () => {
    const begun = adapter([], [])
    const bound = bindOutputFileTransaction(begun, file, transientSession)

    await expect(bound.transaction.writeRange(1n, Uint8Array.of(1), signal)).rejects
      .toBeInstanceOf(OutputCheckpointContractError)
    await bound.transaction.writeRange(0n, Uint8Array.of(1), signal)
    await expect(bound.transaction.commit(signal)).rejects.toBeInstanceOf(OutputCheckpointContractError)
    expect(begun.transaction.commit).not.toHaveBeenCalled()
  })
})

function adapter(
  initial: readonly ByteRange[],
  checkpoint: readonly ByteRange[],
  retirementDisposition: unknown = 'FileIsolated',
): BeginOutputFileResult {
  return Object.freeze({
    durableRanges: durable(initial),
    transaction: Object.freeze({
      writeRange: vi.fn(async () => undefined),
      checkpoint: vi.fn(async () => durable(checkpoint)),
      commit: vi.fn(async () => undefined),
      retire: vi.fn(async () => retirementDisposition) as never,
      pause: vi.fn(async () => undefined),
    }),
  })
}

function durable(ranges: readonly ByteRange[]): VerifiedDurableRanges {
  return new VerifiedDurableRanges(ownership, file.source, file.exactSize, ranges)
}

function identityText(first: number): string {
  const identity = new Uint8Array(16)
  identity[0] = first
  return encodeBase64Url(identity)
}
