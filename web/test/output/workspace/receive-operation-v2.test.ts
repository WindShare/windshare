import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../../src/crypto/bytes'
import {
  createDirectorySelectionResultRoot,
  createDirectResumableZipPlan,
  createFSAOwnedFileBinding,
  createReceiveIntent,
  createSelectionSpec,
  createZipArchiveArtifact,
  deriveArtifactChoiceIdentity,
} from '../../../src/transfer/intent'
import {
  RECEIVE_RECORD_OPERATION,
  createReceiveOperationV2,
  decodeStoredReceiveOperation,
  storedReceiveOperationRecord,
} from '../../../src/output/workspace/records'

describe('ReceiveOperation V2', () => {
  it('binds the derived choice identity and frozen pre-click ranking', async () => {
    const receiveIntent = await directZipIntent()
    const choice = await deriveArtifactChoiceIdentity(receiveIntent.artifact, receiveIntent.plan)
    const operation = await createReceiveOperationV2({
      receiveIntent,
      preClickRanking: [choice.id],
    })

    expect(operation).toMatchObject({
      schemaVersion: 2,
      choiceId: choice.id,
      preClickRanking: [choice.id],
    })
    await expect(decodeStoredReceiveOperation(storedReceiveOperationRecord(operation)))
      .resolves.toEqual(operation)
  })

  it('rejects a record whose indexed V2 envelope disagrees with canonical authority', async () => {
    const receiveIntent = await directZipIntent()
    const choice = await deriveArtifactChoiceIdentity(receiveIntent.artifact, receiveIntent.plan)
    const operation = await createReceiveOperationV2({
      receiveIntent,
      preClickRanking: [choice.id],
    })
    const record = storedReceiveOperationRecord(operation)
    await expect(decodeStoredReceiveOperation({
      ...record,
      id: `windshare/receive-operation/v2/${identity(16, 99)}/${RECEIVE_RECORD_OPERATION}`,
    })).rejects.toThrow()
  })
})

async function directZipIntent() {
  const operationId = identity(16, 10)
  const selection = await createSelectionSpec({
    shareInstance: identity(16, 1),
    syntheticRoot: identity(16, 2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const artifact = await createZipArchiveArtifact(
    createDirectorySelectionResultRoot(identity(16, 3), 'reports'),
  )
  const binding = await createFSAOwnedFileBinding({
    operationId,
    artifact,
    stableName: `reports.windshare-${identity(16, 11)}.zip`,
    targetRef: identity(32, 4),
    policies: {
      zipEncoding: identity(32, 5),
      layout: identity(32, 6),
      checkpoint: identity(32, 7),
      journalBudget: identity(32, 8),
      epoch: identity(32, 9),
    },
  })
  const plan = await createDirectResumableZipPlan(artifact, binding)
  return createReceiveIntent({ selection, artifact, plan })
}

function identity(width: number, fill: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(fill))
}
