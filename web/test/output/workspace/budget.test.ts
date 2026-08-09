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
import {
  admitWorkspaceBudget,
  createPreparedZipWorkspaceBudget,
  decodeWorkspaceBudgetV1,
} from '../../../src/output/workspace/budget'
import { sealWorkspaceZipPreparation } from '../../../src/output/workspace/preparation'

describe('WorkspaceBudgetV1', () => {
  it('accounts raw, package, spool, and durable metadata with checked additive peak semantics', async () => {
    const intent = await zipIntent()
    const preparation = await sealWorkspaceZipPreparation(preparationInput(intent))
    const budget = await createPreparedZipWorkspaceBudget({
      receiveIntent: intent,
      preparation,
      durableMetadataBytes: 100n,
    })

    expect(budget).toEqual(expect.objectContaining({
      uniqueRawBytes: preparation.manifest.selectedRawBytes,
      packageBytes: preparation.zipLayout.exactArchiveBytes,
      peakTemporaryBytes: preparation.zipLayout.maximumSpoolBytes,
      durableMetadataBytes: 100n,
    }))
    expect(budget.peakOwnedBytes).toBe(
      budget.uniqueRawBytes + budget.packageBytes +
      budget.peakTemporaryBytes + budget.durableMetadataBytes,
    )
  })

  it('decodes only the canonical budget bound to the persisted ReceiveIntent', async () => {
    const intent = await zipIntent()
    const preparation = await sealWorkspaceZipPreparation(preparationInput(intent))
    const budget = await createPreparedZipWorkspaceBudget({
      receiveIntent: intent,
      preparation,
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

  it('subtracts only reverified owned bytes from quota while retaining job and process peaks', async () => {
    const intent = await zipIntent()
    const preparation = await sealWorkspaceZipPreparation(preparationInput(intent))
    const budget = await createPreparedZipWorkspaceBudget({
      receiveIntent: intent,
      preparation,
      durableMetadataBytes: 100n,
    })
    const accepted = admitWorkspaceBudget(budget, {
      jobLimitBytes: budget.peakOwnedBytes,
      processLimitBytes: budget.peakOwnedBytes,
      otherActiveJobPeakBytes: 0n,
      estimatedQuotaBytes: budget.peakOwnedBytes,
      currentUsageBytes: budget.uniqueRawBytes,
      minimumReserveBytes: 0n,
      verifiedAlreadyOwnedBytes: budget.uniqueRawBytes,
    })

    expect(accepted).toEqual({
      kind: 'accepted',
      budgetDigest: budget.digest,
      incrementalPhysicalPeakBytes: budget.peakOwnedBytes - budget.uniqueRawBytes,
      limitClass: 'none',
    })
    expect(admitWorkspaceBudget(budget, {
      jobLimitBytes: budget.peakOwnedBytes - 1n,
      processLimitBytes: budget.peakOwnedBytes * 2n,
      otherActiveJobPeakBytes: 0n,
      estimatedQuotaBytes: budget.peakOwnedBytes * 2n,
      currentUsageBytes: 0n,
      minimumReserveBytes: 0n,
      verifiedAlreadyOwnedBytes: budget.peakOwnedBytes,
    })).toEqual(expect.objectContaining({
      kind: 'rejected',
      reason: 'job-workspace-limit',
    }))
  })
})

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
