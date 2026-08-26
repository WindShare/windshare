import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  FILE_CHECKPOINT_PHASE_PAUSED,
  newFileCheckpointV2,
  type FileCheckpointV2,
} from '../../src/output/persistence/checkpoint'
import {
  createFSARecoveryCheckpointSnapshot,
  deriveFSARecoverySummary,
} from '../../src/output/file-system-access/recovery-summary'
import type { DirectTreeIntent } from '../../src/output/file-system-access/settlement-proof'
import type { ReceiveLifecycleState } from '../../src/output/workspace/state'

describe('FSA recovery summary', () => {
  it('includes complete, verified partial, and discovered-but-unstarted files', async () => {
    const fixture = await recoveryFixture('failed')

    const summary = await deriveFSARecoverySummary({
      intent: fixture.intent,
      lifecycle: fixture.lifecycle,
      snapshot: fixture.snapshot,
    })

    expect(summary).toEqual({
      lifecycleGeneration: 7n,
      checkpointSetDigest: fixture.lifecycle.checkpointSetDigest,
      discoveredFileCount: 3n,
      discoveredBytes: 250n,
      discovery: 'known-so-far',
      completedFileCount: 1n,
      completedBytes: 50n,
      incompleteFileCount: 1n,
      verifiedPartialFileCount: 1n,
      verifiedPartialBytes: 30n,
      unstartedFileCount: 1n,
      unstartedBytes: 100n,
      preservingRemainingBytes: 170n,
      restartRemainingBytes: 200n,
      restartRedownloadBytes: 30n,
      maximumPreservingTemporaryBytes: 60n,
    })
  })

  it('keeps complete discovery distinct from known-so-far recovery', async () => {
    const fixture = await recoveryFixture('complete')

    await expect(deriveFSARecoverySummary(fixture)).resolves.toMatchObject({
      discovery: 'complete',
    })
  })

  it('rejects stale lifecycle generations and checkpoint-set digests', async () => {
    const fixture = await recoveryFixture('complete')

    await expect(deriveFSARecoverySummary({
      ...fixture,
      snapshot: Object.freeze({ ...fixture.snapshot, lifecycleGeneration: 6n }),
    })).rejects.toThrow(/lifecycle generation/)
    await expect(deriveFSARecoverySummary({
      ...fixture,
      lifecycle: Object.freeze({
        ...fixture.lifecycle,
        checkpointSetDigest: identity(32, 99),
      }),
    })).rejects.toThrow(/digest/)
  })

  it('rejects foreign and duplicate committed file authority', async () => {
    const fixture = await recoveryFixture('complete')
    const foreign = checkpoint(fixture.intent, 8, 25n, [{ start: 0n, end: 5n }], {
      operationId: identity(16, 88),
    })
    const foreignSnapshot = await createFSARecoveryCheckpointSnapshot(
      fixture.intent,
      fixture.lifecycle.generation,
      [foreign],
    )
    await expect(deriveFSARecoverySummary({
      intent: fixture.intent,
      lifecycle: Object.freeze({
        ...fixture.lifecycle,
        checkpointSetDigest: foreignSnapshot.checkpointSetDigest,
        completedFileCount: 0n,
        completedBytes: 0n,
      }),
      snapshot: foreignSnapshot,
    })).rejects.toThrow(/does not belong/)

    const duplicateSnapshot = await createFSARecoveryCheckpointSnapshot(
      fixture.intent,
      fixture.lifecycle.generation,
      [fixture.checkpoints[0]!, fixture.checkpoints[0]!],
    )
    await expect(deriveFSARecoverySummary({
      intent: fixture.intent,
      lifecycle: Object.freeze({
        ...fixture.lifecycle,
        checkpointSetDigest: duplicateSnapshot.checkpointSetDigest,
        selectionFacts: Object.freeze({
          discoveredFileCount: 4n,
          discoveredBytes: 300n,
          discovery: 'complete' as const,
        }),
        completedFileCount: 2n,
        completedBytes: 100n,
      }),
      snapshot: duplicateSnapshot,
    })).rejects.toThrow(/duplicate/)
  })

  it('rejects lifecycle totals that could underflow either recovery choice', async () => {
    const fixture = await recoveryFixture('complete')

    await expect(deriveFSARecoverySummary({
      ...fixture,
      lifecycle: Object.freeze({
        ...fixture.lifecycle,
        selectionFacts: Object.freeze({
          discoveredFileCount: 2n,
          discoveredBytes: 70n,
          discovery: 'complete' as const,
        }),
      }),
    })).rejects.toThrow(/exceed the discovered selection/)
  })
})

function recoveryFixture(discovery: 'complete' | 'failed') {
  const intent = directTreeIntent()
  const checkpoints = Object.freeze([
    checkpoint(intent, 1, 50n, [{ start: 0n, end: 50n }]),
    checkpoint(intent, 2, 100n, [
      { start: 0n, end: 20n },
      { start: 50n, end: 60n },
    ]),
  ])
  return createFixture(intent, checkpoints, discovery)
}

async function createFixture(
  intent: DirectTreeIntent,
  checkpoints: readonly FileCheckpointV2[],
  discovery: 'complete' | 'failed',
) {
  const snapshot = await createFSARecoveryCheckpointSnapshot(intent, 7n, checkpoints)
  const lifecycle: Extract<ReceiveLifecycleState, {
    kind: 'resumable-receive'
    payloadKind: 'file-set'
  }> = Object.freeze({
    kind: 'resumable-receive',
    payloadKind: 'file-set',
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    generation: 7n,
    checkpointSetDigest: snapshot.checkpointSetDigest,
    completedFileCount: 1n,
    completedBytes: 50n,
    selectionFacts: Object.freeze({
      discoveredFileCount: 3n,
      discoveredBytes: 250n,
      discovery,
    }),
    expiresAt: 10_000,
  })
  return Object.freeze({ intent, lifecycle, snapshot, checkpoints })
}

function checkpoint(
  intent: DirectTreeIntent,
  seed: number,
  exactSize: bigint,
  verifiedRanges: readonly Readonly<{ start: bigint; end: bigint }>[],
  override: Readonly<{ operationId?: string }> = {},
): FileCheckpointV2 {
  const complete = verifiedRanges.length === 1 && verifiedRanges[0]?.end === exactSize
  return newFileCheckpointV2({
    operationId: override.operationId ?? intent.operationId,
    receiveIntentDigest: intent.digest,
    materializationBindingDigest: intent.plan.reservation.digest,
    fileId: identity(16, 10 + seed),
    fileRevision: identity(16, 20 + seed),
    canonicalPath: [`file-${seed}.bin`],
    exactSize,
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    authorityRef: intent.plan.reservation.authorityRef,
    ownedObjectId: identity(32, 30 + seed),
    stateGeneration: 1n,
    checkpointGeneration: 1n,
    verifiedRanges,
    phase: complete ? FILE_CHECKPOINT_PHASE_ACTIVE : FILE_CHECKPOINT_PHASE_PAUSED,
    commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
  })
}

function directTreeIntent(): DirectTreeIntent {
  return Object.freeze({
    operationId: identity(16, 1),
    digest: identity(32, 2),
    plan: Object.freeze({
      reservation: Object.freeze({
        digest: identity(32, 3),
        authorityRef: identity(32, 4),
      }),
    }),
  }) as unknown as DirectTreeIntent
}

function identity(width: number, fill: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(fill))
}
