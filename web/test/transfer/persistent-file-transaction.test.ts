import { describe, expect, it, vi } from 'vitest'

import type { PersistentFileTransactionPort } from '../../src/output/persistent-tree/contracts'
import { PersistentRecoveryPreflightError } from '../../src/output/persistent-tree/recovery'
import { TransferPauseRequestedError } from '../../src/transfer/output-session'
import { PersistentOutputTransaction } from '../../src/transfer/settlement/persistent-file-transaction'

describe('persistent output transaction recovery boundary', () => {
  it('turns a declined preserving-open preflight into a recoverable transfer pause', async () => {
    const cause = new PersistentRecoveryPreflightError({
      reason: 'space-confirmation-required',
      preflight: {
        cost: {
          prefixCopyBytes: 2n,
          cumulativeWriteAmplificationBytes: 2n,
          peakTemporaryBytes: 2n,
        },
        space: 'requires-user-confirmation',
      },
      budget: {
        maximumPrefixCopyBytes: 4n,
        maximumCumulativeWriteAmplificationBytes: 4n,
        maximumPeakTemporaryBytes: 4n,
      },
    })
    const lowLevel: PersistentFileTransactionPort = {
      revision: { fileId: 'file', fileRevision: 'revision', exactSize: 4n },
      ownedObjectId: 'owned-file',
      initialDurableRanges: [{ start: 0n, end: 2n }],
      verifiedRanges: [{ start: 0n, end: 2n }],
      writeRange: () => Promise.reject(cause),
      automaticCheckpoint: () => Promise.reject(new Error('unexpected automatic checkpoint')),
      checkpoint: () => Promise.reject(new Error('unexpected checkpoint')),
      commit: () => Promise.reject(new Error('unexpected commit')),
      pause: () => Promise.resolve([{ start: 0n, end: 2n }]),
      retire: () => Promise.resolve(),
      close: () => Promise.resolve(),
    }
    const transaction = new PersistentOutputTransaction({
      transaction: lowLevel,
      revision: {
        shareInstance: 'share',
        fileId: 'file',
        fileRevision: 'revision',
        exactSize: 4n,
      },
      ownership: {
        backend: 'test',
        outputSessionId: 'session',
        canonicalPath: ['payload.bin'],
        ownedFileIdentity: 'owned-file',
      },
      checkpointNamespace: {
        operationId: 'operation',
        receiveIntentDigest: 'intent',
        materializationBindingDigest: 'binding',
      },
      isolated: true,
      releaseMutation: vi.fn(),
      recordProof: vi.fn(),
    })

    const rejection = await transaction.writeRange(
      2n,
      Uint8Array.of(3, 4),
      new AbortController().signal,
    ).catch((error: unknown) => error)

    expect(rejection).toBeInstanceOf(TransferPauseRequestedError)
    expect(rejection).toMatchObject({ cause })
  })
})
