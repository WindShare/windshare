import { describe, expect, it, vi } from 'vitest'

import type { PersistentFileTransactionPort } from '../../src/output/persistent-tree/contracts'
import { PersistentPreservingWriterOpenError } from '../../src/output/persistent-tree/recovery'
import { snapshotMaterializationRootRelativePath } from '../../src/transfer/job/coordinate/direct-tree'
import {
  PersistentOutputTransaction,
  PersistentWriterOpenPauseRequestedError,
} from '../../src/transfer/settlement/persistent-file-transaction'

describe('persistent output transaction recovery boundary', () => {
  it('retains the exact failed preserving open in a typed resumable pause', async () => {
    const cause = new PersistentPreservingWriterOpenError({
      materializationRelativePath: snapshotMaterializationRootRelativePath(['payload.bin']),
      cost: {
        prefixCopyBytes: 2n,
        writeAmplificationBytes: 2n,
        temporaryBytes: 2n,
      },
      purpose: 'automatic-checkpoint',
      cause: new DOMException('destination has no capacity', 'QuotaExceededError'),
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

    expect(rejection).toBeInstanceOf(PersistentWriterOpenPauseRequestedError)
    expect(rejection).toMatchObject({
      cause,
      materializationRelativePath: ['payload.bin'],
      cost: {
        prefixCopyBytes: 2n,
        writeAmplificationBytes: 2n,
        temporaryBytes: 2n,
      },
      purpose: 'automatic-checkpoint',
    })
  })
})
