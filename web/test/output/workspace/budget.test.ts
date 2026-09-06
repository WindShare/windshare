import { describe, expect, it } from 'vitest'

import { canonicalFrame, canonicalIdentity, canonicalRecord, canonicalU8, canonicalU64 } from '../../../src/output/workspace/canonical'
import {
  createPreparationAdmissionReceipt,
  decodePreparationAdmissionAuthority,
} from '../../../src/output/workspace/receipts/admission'
import { createPersistedReceiveRecord, RECEIVE_RECORD_RECEIPT } from '../../../src/output/workspace/records'
import { encodeBase64Url } from '../../../src/crypto/bytes'
import {
  createReceiveIntent,
  createSelectionSpec,
  createSyntheticSelectionResultRoot,
  createWorkspaceBinding,
  createWorkspaceThenPublishPlan,
  createZipArchiveArtifact,
} from '../../../src/transfer/intent'
import {
  admitWorkspaceBudget,
  createProgressiveZipWorkspaceBudget,
  decodeWorkspaceBudgetV1,
} from '../../../src/output/workspace/budget'

describe('WorkspaceBudgetV1', () => {
  it('reserves durable metadata without inventing unknown payload or copies', async () => {
    const intent = await zipIntent()
    const budget = await createProgressiveZipWorkspaceBudget({
      receiveIntent: intent,
      durableMetadataBytes: 100n,
    })

    expect(budget).toEqual(expect.objectContaining({
      uniqueRawBytes: 0n,
      durableMetadataBytes: 100n,
    }))
    expect(budget.peakOwnedBytes).toBe(
      budget.uniqueRawBytes + budget.durableMetadataBytes,
    )
  })

  it('decodes only the canonical budget bound to the persisted ReceiveIntent', async () => {
    const intent = await zipIntent()
    const budget = await createProgressiveZipWorkspaceBudget({
      receiveIntent: intent,
      durableMetadataBytes: 100n,
    })

    await expect(decodeWorkspaceBudgetV1(budget.canonicalBytes, intent))
      .resolves.toEqual(budget)
    const altered = new Uint8Array(budget.canonicalBytes)
    const finalByteIndex = altered.length - 1
    altered[finalByteIndex] = altered[finalByteIndex]! ^ 1
    await expect(decodeWorkspaceBudgetV1(altered, intent))
      .rejects.toThrow()
  })

  it('rejects the removed prepared ZIP evidence wire tag', async () => {
    const intent = await zipIntent()
    const budget = await createProgressiveZipWorkspaceBudget({
      receiveIntent: intent, durableMetadataBytes: 100n,
    })
    // The removed tag must fail before persisted claims can reacquire content authority.
    const altered = canonicalRecord('windshare/workspace-budget/v1', 1, [
      canonicalFrame(canonicalIdentity(budget.operationId, 16, 'operation')),
      canonicalFrame(canonicalIdentity(budget.receiveIntentDigest, 32, 'intent')),
      canonicalFrame(canonicalIdentity(budget.workspaceBindingDigest, 32, 'workspace')),
      canonicalFrame(canonicalU8(2)),
      canonicalFrame(canonicalU64(0n)),
      canonicalFrame(canonicalU64(100n)),
      canonicalFrame(canonicalU64(100n)),
    ])
    await expect(decodeWorkspaceBudgetV1(altered, intent))
      .rejects.toThrow('workspace budget evidence discriminant is invalid')
  })

  it.each([null, 1_000_000n])('reopens admission with quota estimate %s', async (estimatedQuotaBytes) => {
    const intent = await zipIntent()
    const budget = await createProgressiveZipWorkspaceBudget({
      receiveIntent: intent, durableMetadataBytes: 100n,
    })
    const receipt = await createPreparationAdmissionReceipt({
      operationId: intent.operationId, receiveIntentDigest: intent.digest, workspaceBudget: budget,
      contentRequestCountAtAdmission: 0n, estimatedQuotaBytes, currentUsageBytes: 0n,
      minimumReserveBytes: 0n, incrementalPhysicalPeakBytes: 100n,
    })
    const record = await createPersistedReceiveRecord({
      operationId: intent.operationId, kind: RECEIVE_RECORD_RECEIPT,
      canonicalBytes: receipt.canonicalBytes,
    })
    await expect(decodePreparationAdmissionAuthority(record, intent)).resolves.toEqual({ budget, receipt })
    await expect(decodePreparationAdmissionAuthority(
      { ...record, digest: identity(32, 9) }, intent,
    )).rejects.toThrow('preparation admission receipt authority changed')
  })

  it('admits metadata independently of future payload sizes', async () => {
    const intent = await zipIntent()
    const budget = await createProgressiveZipWorkspaceBudget({
      receiveIntent: intent,
      durableMetadataBytes: 100n,
    })
    const accepted = admitWorkspaceBudget(budget, {
      outstandingGrowthBytes: 0n, metadataHeadroomBytes: 0n,
      estimatedQuotaBytes: budget.peakOwnedBytes,
      currentUsageBytes: budget.uniqueRawBytes,
      minimumReserveBytes: 0n,
      verifiedAlreadyOwnedBytes: budget.uniqueRawBytes,
    })

    expect(accepted).toEqual({
      kind: 'accepted',
      budgetDigest: budget.digest,
      incrementalPhysicalPeakBytes: budget.durableMetadataBytes,
      limitClass: 'none',
    })
    expect(admitWorkspaceBudget(budget, {
      outstandingGrowthBytes: 0n, metadataHeadroomBytes: 0n,
      estimatedQuotaBytes: budget.peakOwnedBytes * 2n,
      currentUsageBytes: 0n,
      minimumReserveBytes: 0n,
      verifiedAlreadyOwnedBytes: budget.peakOwnedBytes,
    })).toEqual(expect.objectContaining({
      kind: 'accepted',
    }))
  })
  it('canonically binds progressive discovery without guessing future payload bytes', async () => {
    const intent = await zipIntent()
    const budget = await createProgressiveZipWorkspaceBudget({ receiveIntent: intent, durableMetadataBytes: 4096n })
    expect(budget.evidence).toEqual({ kind: 'progressive-zip' })
    expect(budget.peakOwnedBytes).toBe(4096n)
    await expect(decodeWorkspaceBudgetV1(budget.canonicalBytes, intent)).resolves.toEqual(budget)
    expect(admitWorkspaceBudget(budget, {
      outstandingGrowthBytes: 0n, metadataHeadroomBytes: 0n, currentUsageBytes: 0n,
      minimumReserveBytes: 0n, verifiedAlreadyOwnedBytes: 0n,
    }).kind).toBe('accepted')
  })

})

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
