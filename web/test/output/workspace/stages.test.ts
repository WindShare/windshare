import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../../src/crypto/bytes'
import {
  createReceiveIntent,
  createSelectionSpec,
  createSyntheticSelectionResultRoot,
  createWorkspaceBinding,
  createWorkspaceThenPublishPlan,
  createZipArchiveArtifact,
} from '../../../src/transfer/intent'
import { admitWorkspaceBudget } from '../../../src/output/workspace/budget'
import { sealPackagedArtifact } from '../../../src/output/workspace/aggregate'
import type { WorkspaceOwnedCleanupPort } from '../../../src/output/workspace/cleanup'
import { sealWorkspaceZipPreparation } from '../../../src/output/workspace/preparation'
import {
  RECEIVE_RECORD_LIFECYCLE_STATE,
  type ManifestPageRecord,
  type PersistedReceiveRecord,
  type ReceiveOperationHandleRecord,
  type ReceiveOperationLeaseRecord,
  type ReceiveRecordKind,
} from '../../../src/output/workspace/records'
import type {
  ReceiveOperationRepository,
  ReceiveOperationTransition,
} from '../../../src/output/workspace/repository'
import {
  assertWorkspaceContentGate,
  stableStateKind,
  workspaceZipLayoutHandleId,
  WorkspaceOperationStages,
  type WorkspaceBudgetAuthority,
  type WorkspaceStageTraceEvent,
} from '../../../src/output/workspace/stages'
import { initialReceiveLifecycleState } from '../../../src/output/workspace/state'
import {
  decodeStoredReceiveLifecycleState,
  storedReceiveLifecycleState,
} from '../../../src/output/workspace/state-codec'

describe('workspace stage admission gate', () => {
  it('issues zero content requests and durably discards an over-budget preparation', async () => {
    const intent = await zipIntent()
    const preparation = await sealWorkspaceZipPreparation(preparationInput(intent))
    const repository = new MemoryReceiveOperationRepository()
    await repository.commitTransition({
      operationId: intent.operationId,
      lifecycle: initialReceiveLifecycleState({
        operationId: intent.operationId,
        receiveIntentDigest: intent.digest,
      }),
    })
    const contentRequests = 0n
    const stages = await WorkspaceOperationStages.open({
      repository,
      receiveIntent: intent,
      leaseId: identity(16, 11),
      clock: () => 1_000,
      contentRequests: { count: () => contentRequests },
    })
    await stages.beginReceive(identity(16, 12))
    const cleanup: WorkspaceOwnedCleanupPort = {
      removeOwnedObject: async () => Object.freeze({ kind: 'already-absent' }),
      removeFileCheckpoints: async () => Object.freeze({
        kind: 'clean',
        removedRecordDigests: [],
      }),
    }
    const authority: WorkspaceBudgetAuthority = {
      claim: async (budget) => {
        const capacity = Object.freeze({
          jobLimitBytes: 1n,
          processLimitBytes: 1n,
          otherActiveJobPeakBytes: 0n,
          estimatedQuotaBytes: 1n,
          currentUsageBytes: 0n,
          minimumReserveBytes: 0n,
          verifiedAlreadyOwnedBytes: 0n,
        })
        const admission = admitWorkspaceBudget(budget, capacity)
        if (admission.kind !== 'rejected') throw new Error('test budget unexpectedly fit')
        return Object.freeze({ kind: 'rejected', capacity, admission })
      },
    }

    const result = await stages.admitPreparedZip({
      preparation,
      authority,
      durableMetadataBytesExcludingAdmissionRecords: 0n,
      rejectionCleanup: { targets: [], metadataHandleIds: [], port: cleanup },
    })

    expect(result).toEqual(expect.objectContaining({
      kind: 'rejected',
      state: expect.objectContaining({ kind: 'discarded' }),
    }))
    const lifecycle = await repository.readLifecycle(intent.operationId)
    expect(lifecycle === undefined ? undefined : decodeStoredReceiveLifecycleState(lifecycle).kind)
      .toBe('discarded')
  })

  it('reissues only a durably admitted gate and releases a rejected recovery claim', async () => {
    const intent = await zipIntent()
    const preparation = await sealWorkspaceZipPreparation(preparationInput(intent))
    const repository = new MemoryReceiveOperationRepository()
    await repository.commitTransition({
      operationId: intent.operationId,
      lifecycle: initialReceiveLifecycleState({
        operationId: intent.operationId,
        receiveIntentDigest: intent.digest,
      }),
    })
    let contentRequests = 0n
    let releases = 0
    const traceNames: string[] = []
    const stages = await WorkspaceOperationStages.open({
      repository,
      receiveIntent: intent,
      leaseId: identity(16, 11),
      clock: () => 1_000,
      contentRequests: { count: () => contentRequests },
      onTrace: (event) => {
        traceNames.push(event.name)
        throw new Error('telemetry unavailable')
      },
    })
    await stages.beginReceive(identity(16, 12))
    const capacity = Object.freeze({
      jobLimitBytes: 1_000_000n,
      processLimitBytes: 2_000_000n,
      otherActiveJobPeakBytes: 0n,
      estimatedQuotaBytes: 3_000_000n,
      currentUsageBytes: 0n,
      minimumReserveBytes: 0n,
      verifiedAlreadyOwnedBytes: 0n,
    })
    const authority: WorkspaceBudgetAuthority = {
      claim: async (budget) => {
        const admission = admitWorkspaceBudget(budget, capacity)
        if (admission.kind !== 'accepted') throw new Error('test budget unexpectedly rejected')
        return Object.freeze({
          kind: 'accepted',
          claim: Object.freeze({
            budgetDigest: budget.digest,
            capacity,
            admission,
            release: async () => { releases += 1 },
          }),
        })
      },
    }
    const cleanup: WorkspaceOwnedCleanupPort = {
      removeOwnedObject: async () => Object.freeze({ kind: 'already-absent' }),
      removeFileCheckpoints: async () => Object.freeze({ kind: 'clean', removedRecordDigests: [] }),
    }
    const admitted = await stages.admitPreparedZip({
      preparation,
      authority,
      durableMetadataBytesExcludingAdmissionRecords: 0n,
      rejectionCleanup: { targets: [], metadataHandleIds: [], port: cleanup },
    })
    if (admitted.kind !== 'accepted') throw new Error('test admission unexpectedly rejected')
    await expect(repository.readHandle(workspaceZipLayoutHandleId(
      intent.operationId,
      preparation.manifest.preparationId,
    ))).resolves.toEqual(expect.objectContaining({
      kind: 19,
      handle: preparation.zipLayout,
    }))
    expect(traceNames).toEqual([
      'receive.preparation.started',
      'receive.preparation.sealed',
      'receive.preparation_admission.accepted',
    ])

    const reopened = await stages.reopenAdmittedContent({
      budget: admitted.content.budget,
      claim: admitted.content.claim,
    })
    assertWorkspaceContentGate(reopened.gate, {
      operationId: intent.operationId,
      receiveIntentDigest: intent.digest,
      workspaceBudgetDigest: admitted.content.budget.digest,
    })

    contentRequests = 1n
    await expect(stages.reopenAdmittedContent({
      budget: admitted.content.budget,
      claim: admitted.content.claim,
    })).rejects.toThrow('before durable budget admission')
    expect(releases).toBe(1)
  })

  it('expires a not-started handoff from the exact waiting-to-save predecessor', async () => {
    const intent = await zipIntent()
    const packaged = await sealPackagedArtifact({
      operationId: intent.operationId,
      receiveIntentDigest: intent.digest,
      sealedMaterializationDigest: identity(32, 20),
      artifactSpecDigest: intent.artifact.digest,
      packageOwnedObjectId: identity(32, 21),
      exactBytes: 3n,
      artifactReceiptDigest: identity(32, 22),
      layoutDigest: identity(32, 23),
    })
    const expiresAt = 2_000
    const waiting = Object.freeze({
      kind: 'waiting-to-save' as const,
      operationId: intent.operationId,
      receiveIntentDigest: intent.digest,
      generation: 1n,
      packageDigest: packaged.digest,
      expiresAt,
    })
    expect(stableStateKind(waiting)).toBe('waiting-to-save')
    expect(() => stableStateKind(initialReceiveLifecycleState({
      operationId: intent.operationId,
      receiveIntentDigest: intent.digest,
    }))).toThrow('not durably expirable')
    expect(() => stableStateKind(Object.freeze({
      ...waiting,
      kind: 'artifact-sealed' as const,
    }))).toThrow('not durably expirable')

    const repository = new MemoryReceiveOperationRepository()
    await repository.commitTransition({ operationId: intent.operationId, lifecycle: waiting })
    let now = 1_000
    const trace: WorkspaceStageTraceEvent[] = []
    const stages = await WorkspaceOperationStages.open({
      repository,
      receiveIntent: intent,
      leaseId: identity(16, 24),
      clock: () => now,
      contentRequests: { count: () => 0n },
      onTrace: event => trace.push(event),
    })
    const attempt = await stages.startHandoff({
      package: packaged,
      publicationAttemptId: identity(16, 25),
      suggestedName: 'WindShare.zip',
      packagedFileSupported: true,
    })

    now = expiresAt
    const expired = await stages.recordHandoffNotStarted({
      package: packaged,
      attempt,
      reason: 'user-cancelled',
    })
    const stored = await repository.readLifecycle(intent.operationId)

    expect(expired).toMatchObject({
      kind: 'expired',
      priorStableState: 'waiting-to-save',
      expiresAt,
    })
    expect(stored === undefined ? undefined : decodeStoredReceiveLifecycleState(stored))
      .toMatchObject({
        kind: 'expired',
        priorStableState: 'waiting-to-save',
        expiresAt,
      })
    expect(trace).toContainEqual(expect.objectContaining({
      name: 'receive.operation.expired',
      prior_stable_state: 'waiting-to-save',
      expires_at_ms: expiresAt,
    }))
  })
})

class MemoryReceiveOperationRepository implements ReceiveOperationRepository {
  readonly #records = new Map<string, PersistedReceiveRecord>()
  readonly #pages = new Map<string, ManifestPageRecord>()
  readonly #handles = new Map<string, ReceiveOperationHandleRecord>()

  close(): void {}

  async commitTransition(transition: ReceiveOperationTransition): Promise<void> {
    for (const id of transition.deleteRecordIds ?? []) this.#records.delete(id)
    for (const id of transition.deleteManifestPageIds ?? []) this.#pages.delete(id)
    for (const id of transition.deleteHandleIds ?? []) this.#handles.delete(id)
    for (const record of transition.records ?? []) this.#records.set(record.id, record)
    for (const page of transition.manifestPages ?? []) this.#pages.set(page.id, page)
    for (const handle of transition.handles ?? []) this.#handles.set(handle.id, handle)
    if (transition.lifecycle !== undefined) {
      const record = await storedReceiveLifecycleState(transition.lifecycle)
      for (const [id, existing] of this.#records) {
        if (existing.operationId === transition.operationId &&
            existing.kind === RECEIVE_RECORD_LIFECYCLE_STATE) this.#records.delete(id)
      }
      this.#records.set(record.id, record)
    }
  }

  readRecord(id: string): Promise<PersistedReceiveRecord | undefined> {
    return Promise.resolve(this.#records.get(id))
  }

  readLifecycle(operationId: string): Promise<PersistedReceiveRecord | undefined> {
    return Promise.resolve([...this.#records.values()].find((record) =>
      record.operationId === operationId && record.kind === RECEIVE_RECORD_LIFECYCLE_STATE))
  }

  listRecords(operationId: string, kind?: ReceiveRecordKind): Promise<readonly PersistedReceiveRecord[]> {
    return Promise.resolve([...this.#records.values()].filter((record) =>
      record.operationId === operationId && (kind === undefined || record.kind === kind)))
  }

  listManifestPages(operationId: string, kind?: ReceiveRecordKind): Promise<readonly ManifestPageRecord[]> {
    return Promise.resolve([...this.#pages.values()].filter((page) =>
      page.operationId === operationId && (kind === undefined || page.kind === kind)))
  }

  readHandle<T = unknown>(id: string): Promise<ReceiveOperationHandleRecord<T> | undefined> {
    return Promise.resolve(this.#handles.get(id) as ReceiveOperationHandleRecord<T> | undefined)
  }

  readLease(): Promise<ReceiveOperationLeaseRecord | undefined> {
    return Promise.resolve(undefined)
  }
}

function preparationInput(intent: Awaited<ReturnType<typeof zipIntent>>) {
  const directoryId = intent.selection.syntheticRoot
  const generation = identity(16, 8)
  const rootName = intent.artifact.kind === 'zip-archive' ? intent.artifact.layout.name : 'WindShare'
  return {
    receiveIntent: intent,
    preparationId: identity(16, 9),
    generations: [{ directoryId, generation }],
    entries: [
      {
        kind: 'directory' as const,
        sourcePath: [],
        artifactPath: [rootName],
        directoryId,
        generation,
        role: 'result-root' as const,
      },
      {
        kind: 'file' as const,
        sourcePath: ['file.bin'],
        artifactPath: [rootName, 'file.bin'],
        fileId: identity(16, 10),
        containingDirectoryId: directoryId,
        generation,
        exactSize: 3n,
      },
    ],
  }
}

async function zipIntent() {
  const artifact = await createZipArchiveArtifact(createSyntheticSelectionResultRoot())
  const workspace = await createWorkspaceBinding({
    operationId: identity(16, 4),
    workspaceId: identity(16, 5),
    artifact,
    repositoryRef: identity(32, 6),
  })
  return createReceiveIntent({
    selection: await createSelectionSpec({
      shareInstance: identity(16, 1),
      syntheticRoot: identity(16, 2),
      rules: { mode: 'node-id', defaultSelected: true, rules: [] },
    }),
    artifact,
    plan: await createWorkspaceThenPublishPlan(artifact, workspace),
  })
}

function identity(width: number, fill: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(fill))
}
