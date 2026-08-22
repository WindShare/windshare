import { describe, expect, it, vi } from 'vitest'

import type { ReceiveOperationResumeDescriptor } from '../../src/output/resume/descriptor'
import {
  AuthorityOwnedReceiveOperationMutationPort,
  type PersistedReceiveOperationReopenPort,
  type ReceiveOperationOwnedCleanupExecutor,
  type ReopenedDirectZipOperation,
} from '../../src/output/resume/reopen-authority'

describe('retained Direct ZIP cleanup routing', () => {
  it.each(['expire', 'catchUp'] as const)(
    'preserves Direct ZIP target-proof ownership through %s instead of generic cleanup',
    async method => {
      const operation = directZipOperation()
      const reopen: PersistedReceiveOperationReopenPort = {
        reopen: vi.fn(async () => operation),
      }
      const cleanup: ReceiveOperationOwnedCleanupExecutor = {
        cleanup: vi.fn(async () => ({ kind: 'already-absent' as const })),
      }
      const mutation = new AuthorityOwnedReceiveOperationMutationPort({ reopen, cleanup })

      const result = await mutation[method](descriptor())

      expect(result).toEqual({
        kind: 'continuation',
        continuation: { kind: 'direct-zip-retained-cleanup', operation },
      })
      expect(cleanup.cleanup).not.toHaveBeenCalled()
      expect(operation.close).not.toHaveBeenCalled()
      expect(reopen.reopen).toHaveBeenCalledWith(
        expect.any(Object),
        'cleanup',
        undefined,
      )
    },
  )
})

function directZipOperation(): ReopenedDirectZipOperation {
  return {
    kind: 'direct-zip',
    close: vi.fn(async () => undefined),
  } as unknown as ReopenedDirectZipOperation
}

function descriptor(): ReceiveOperationResumeDescriptor {
  return Object.freeze({
    schemaVersion: 2,
    operationId: 'operation',
    receiveIntentDigest: 'intent',
    lifecycleGeneration: 1n,
    lifecycle: Object.freeze({
      kind: 'expired',
      operationId: 'operation',
      receiveIntentDigest: 'intent',
      generation: 1n,
      priorStableState: 'resumable-receive',
      expiresAt: 1,
      cleanupState: 'cleanup-pending',
      expiryReceiptDigest: 'expiry',
    }),
    continuation: 'cleanup-expired',
    expiresAt: 1,
  })
}
