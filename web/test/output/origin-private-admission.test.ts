import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  createOriginalFileArtifact,
  createReceiveIntent,
  createSelectionSpec,
  createWorkspaceBinding,
  createWorkspaceThenPublishPlan,
} from '../../src/transfer/intent'
import {
  OriginPrivateWorkspaceBudgetAuthority,
  type OriginPrivateWorkspaceBudgetClaim,
} from '../../src/output/origin-private/admission'
import type {
  OriginPrivateWorkspaceBudgetLeaseAuthority,
  WorkspaceBudgetCapacityFacts,
  WorkspaceBudgetLeaseDecision,
  WorkspaceBudgetLeaseRecord,
} from '../../src/output/origin-private/admission-authority'
import {
  admitWorkspaceBudget,
  createSingleFileWorkspaceBudget,
  type WorkspaceBudgetV1,
} from '../../src/output/workspace/budget'

describe('origin-private WorkspaceBudget admission', () => {
  it('readmits against verified already-owned bytes without changing canonical budget authority', async () => {
    const intent = await originalFileIntent()
    const budget = await singleFileBudget(intent, 64n)
    const authority = new MemoryLeaseAuthority()
    const subject = await OriginPrivateWorkspaceBudgetAuthority.open(intent.operationId, {
      authority,
      estimate: async () => ({ usage: 10, quota: 1_000_000 }),
      verifiedAlreadyOwnedBytes: async () => 0n,
      jobLimitBytes: 1_000_000n,
      processLimitBytes: 1_000_000n,
      minimumReserveBytes: 100n,
      now: () => 1_000,
      leaseMilliseconds: 60_000,
      heartbeatMilliseconds: 30_000,
      randomToken: () => 'claim-token',
    })

    const decision = await subject.claim(budget)
    expect(decision.kind).toBe('accepted')
    if (decision.kind !== 'accepted') throw new Error('test budget was rejected')
    const claim = decision.claim as OriginPrivateWorkspaceBudgetClaim
    const readmission = await claim.readmit(40n)

    expect(readmission).toEqual(expect.objectContaining({
      kind: 'accepted',
      budgetDigest: budget.digest,
      incrementalPhysicalPeakBytes: budget.peakOwnedBytes - 40n,
    }))
    expect(authority.facts.map((facts) => facts.verifiedAlreadyOwnedBytes)).toEqual([0n, 40n])
    await claim.release()
    expect(authority.released).toEqual([[intent.operationId, 'claim-token']])
  })

  it('reclaims one canonical budget under a fresh operation lease and fences the stale token', async () => {
    const intent = await originalFileIntent()
    const budget = await singleFileBudget(intent, 64n)
    const leases = new MemoryLeaseAuthority()
    const first = await budgetAuthority(intent.operationId, leases, 'old-token')
    const original = await first.claim(budget)
    if (original.kind !== 'accepted') throw new Error('original test claim was rejected')

    const second = await budgetAuthority(intent.operationId, leases, 'fresh-token')
    const reclaimed = await second.reclaim(budget, {
      operationId: intent.operationId,
      leaseId: identity(16, 10),
    })
    if (reclaimed.kind !== 'accepted') throw new Error('reclaimed test budget was rejected')

    await expect((original.claim as OriginPrivateWorkspaceBudgetClaim).readmit(0n))
      .rejects.toThrow('ownership changed')
    expect(leases.reclaimed).toBe(1)
    await original.claim.release()
    await reclaimed.claim.release()
    expect(leases.released).toEqual([
      [intent.operationId, 'old-token'],
      [intent.operationId, 'fresh-token'],
    ])
  })

  it.each([
    ['job-workspace-limit', { jobLimitBytes: 1n, processLimitBytes: 1_000_000n, quota: 1_000_000 }],
    ['process-workspace-limit', { jobLimitBytes: 1_000_000n, processLimitBytes: 1n, quota: 1_000_000 }],
    ['quota-insufficient', { jobLimitBytes: 1_000_000n, processLimitBytes: 1_000_000n, quota: 100 }],
  ] as const)('rejects exact %s capacity before a claim becomes active', async (reason, limits) => {
    const intent = await originalFileIntent()
    const budget = await singleFileBudget(intent, 64n)
    const subject = await OriginPrivateWorkspaceBudgetAuthority.open(intent.operationId, {
      authority: new MemoryLeaseAuthority(),
      estimate: async () => ({ usage: 0, quota: limits.quota }),
      jobLimitBytes: limits.jobLimitBytes,
      processLimitBytes: limits.processLimitBytes,
      minimumReserveBytes: 100n,
      now: () => 1_000,
      leaseMilliseconds: 60_000,
      heartbeatMilliseconds: 30_000,
      randomToken: () => `claim-${reason}`,
    })

    await expect(subject.claim(budget)).resolves.toEqual(expect.objectContaining({
      kind: 'rejected',
      admission: expect.objectContaining({ kind: 'rejected', reason }),
    }))
  })
})

class MemoryLeaseAuthority implements OriginPrivateWorkspaceBudgetLeaseAuthority {
  readonly facts: WorkspaceBudgetCapacityFacts[] = []
  readonly released: [string, string][] = []
  reclaimed = 0
  #record: WorkspaceBudgetLeaseRecord | undefined

  claim(
    record: WorkspaceBudgetLeaseRecord,
    budget: WorkspaceBudgetV1,
    facts: WorkspaceBudgetCapacityFacts,
  ): Promise<WorkspaceBudgetLeaseDecision> {
    this.#record = record
    return this.#decide(budget, facts)
  }

  readmit(
    record: WorkspaceBudgetLeaseRecord,
    budget: WorkspaceBudgetV1,
    facts: WorkspaceBudgetCapacityFacts,
  ): Promise<WorkspaceBudgetLeaseDecision> {
    if (this.#record?.token !== record.token) {
      throw new DOMException('claim ownership changed', 'InvalidStateError')
    }
    return this.#decide(budget, facts)
  }

  reclaim(
    record: WorkspaceBudgetLeaseRecord,
    budget: WorkspaceBudgetV1,
    facts: WorkspaceBudgetCapacityFacts,
  ): Promise<WorkspaceBudgetLeaseDecision> {
    if (this.#record !== undefined &&
        (this.#record.operationId !== record.operationId ||
         this.#record.budgetDigest !== record.budgetDigest ||
         this.#record.peakOwnedBytes !== record.peakOwnedBytes)) {
      throw new DOMException('claim authority changed', 'InvalidStateError')
    }
    this.#record = record
    this.reclaimed += 1
    return this.#decide(budget, facts)
  }

  heartbeat(): Promise<void> {
    return Promise.resolve()
  }

  release(id: string, token: string): Promise<void> {
    this.released.push([id, token])
    if (this.#record?.id === id && this.#record.token === token) this.#record = undefined
    return Promise.resolve()
  }

  close(): void {}

  #decide(
    budget: WorkspaceBudgetV1,
    facts: WorkspaceBudgetCapacityFacts,
  ): Promise<WorkspaceBudgetLeaseDecision> {
    this.facts.push(facts)
    const capacity = Object.freeze({
      jobLimitBytes: facts.jobLimitBytes,
      processLimitBytes: facts.processLimitBytes,
      otherActiveJobPeakBytes: 0n,
      estimatedQuotaBytes: facts.estimatedQuotaBytes,
      currentUsageBytes: facts.currentUsageBytes,
      minimumReserveBytes: facts.minimumReserveBytes,
      verifiedAlreadyOwnedBytes: facts.verifiedAlreadyOwnedBytes,
    })
    const admission = admitWorkspaceBudget(budget, capacity)
    return Promise.resolve(admission.kind === 'accepted'
      ? Object.freeze({ kind: 'accepted', capacity, admission })
      : Object.freeze({ kind: 'rejected', capacity, admission }))
  }
}

function budgetAuthority(
  operationId: string,
  authority: OriginPrivateWorkspaceBudgetLeaseAuthority,
  token: string,
): Promise<OriginPrivateWorkspaceBudgetAuthority> {
  return OriginPrivateWorkspaceBudgetAuthority.open(operationId, {
    authority,
    estimate: async () => ({ usage: 10, quota: 1_000_000 }),
    verifiedAlreadyOwnedBytes: async () => 0n,
    jobLimitBytes: 1_000_000n,
    processLimitBytes: 1_000_000n,
    minimumReserveBytes: 100n,
    now: () => 1_000,
    leaseMilliseconds: 60_000,
    heartbeatMilliseconds: 30_000,
    randomToken: () => token,
  })
}

async function originalFileIntent() {
  const artifact = await createOriginalFileArtifact({
    fileId: identity(16, 3),
    sourcePath: 'root/file.bin',
    suggestedName: 'file.bin',
  })
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

async function singleFileBudget(
  intent: Awaited<ReturnType<typeof originalFileIntent>>,
  exactSize: bigint,
) {
  return createSingleFileWorkspaceBudget({
    receiveIntent: intent,
    fileId: intent.artifact.kind === 'original-file' ? intent.artifact.fileId : identity(16, 7),
    containingDirectoryId: identity(16, 8),
    generation: identity(16, 9),
    catalogSize: exactSize,
    durableMetadataBytes: 32n,
  })
}

function identity(width: number, fill: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(fill))
}
