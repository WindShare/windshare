import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  newFileCheckpointV2,
} from '../../src/output/persistence/checkpoint'
import {
  classifyMaterializationLedgerAppend,
  createMaterializationLedgerBinding,
} from '../../src/output/materialization-ledger/codec'
import {
  createFinalizedFileMaterializationRecords,
  createMaterializationDirectoryAdmittedEntry,
  createMaterializationDirectoryFinalizedEntry,
  decodeMaterializationFinalFileProofV1,
  decodeMaterializationLedgerEntryV1,
} from '../../src/output/materialization-ledger/journal'
import {
  MaterializationLedgerDirectoryOutcome,
  MaterializationLedgerEntryKind,
} from '../../src/output/materialization-ledger/model'
import { outputFault, FaultScope, OutputFaultCode } from '../../src/transfer/fault'
import { snapshotMaterializationRootRelativePath } from '../../src/transfer/job/coordinate/direct-tree'
import { VerifiedFinalOutputFile } from '../../src/transfer/output-session'

describe('materialization ledger canonical records', () => {
  it('binds the W1 final-output proof shape to one final checkpoint and file entry', async () => {
    const binding = await ledgerBinding()
    const checkpoint = finalCheckpoint(['folder', 'file.bin'], 4, 12n)
    const finalOutput = verifiedFinalOutput(checkpoint)
    const records = await createFinalizedFileMaterializationRecords({
      binding,
      finalOutput,
      finalCheckpoint: checkpoint,
    })

    await expect(decodeMaterializationFinalFileProofV1(
      records.finalProof,
      binding,
    )).resolves.toEqual(records.finalProof)
    await expect(decodeMaterializationLedgerEntryV1(
      records.ledgerEntry,
      binding,
    )).resolves.toEqual(records.ledgerEntry)
    expect(records.ledgerEntry).toEqual(expect.objectContaining({
      kind: MaterializationLedgerEntryKind.FileFinalized,
      relativePath: ['folder', 'file.bin'],
      fileId: finalOutput.source.fileId,
      fileRevision: finalOutput.source.fileRevision,
      exactSize: 12n,
      ownedFileIdentity: finalOutput.ownership.ownedFileIdentity,
      finalProofId: records.finalProof.proofId,
      finalProofDigest: records.finalProof.proofDigest,
    }))
    expect(Object.isFrozen(records.finalProof.finalOutput)).toBe(true)
    expect(Object.isFrozen(records.finalProof.finalOutput.ownership)).toBe(true)
    expect({
      bindingDigest: binding.ledgerBindingDigest,
      proofId: records.finalProof.proofId,
      proofDigest: records.finalProof.proofDigest,
      entryId: records.ledgerEntry.entryId,
      entryDigest: records.ledgerEntry.entryDigest,
    }).toEqual({
      bindingDigest: '098pA56SwZmaPFKFBPNiUNmMSIy_Y49BeaYsFhgFN9k',
      proofId: '4cErNY2gL_cW0xqkWfY4oakXvomd2LuBxYcf3B-Pc74',
      proofDigest: '9TP6iD0uIaqCPB4r1tAnHgBB3K2ZH2GEwg11WNodj30',
      entryId: '6HHayigSmD-fwCkzFl9Gmik0h_4U7rQZ9NBYs0s0D20',
      entryDigest: 'PGXYMQ9Ds7Gl7BNRlNitMUTZhCscscHxdlaqw2h1cuQ',
    })
    expect(Object.isFrozen(records.finalProof.finalOutput.source)).toBe(true)
  })

  it('uses stable directory coordinates rather than a session admission token', async () => {
    const binding = await ledgerBinding()
    const root = await createMaterializationDirectoryAdmittedEntry(binding, {
      relativePath: path([]),
      directoryId: identity(16, 20),
      generation: identity(16, 21),
      ownedObjectId: identity(32, 22),
      modifiedTime: { seconds: 100n, nanoseconds: 2_000_000, precision: 2 },
    })
    const reopened = await createMaterializationDirectoryAdmittedEntry(binding, {
      relativePath: path([]),
      directoryId: identity(16, 20),
      generation: identity(16, 21),
      ownedObjectId: identity(32, 22),
      modifiedTime: { seconds: 100n, nanoseconds: 2_000_000, precision: 2 },
    })
    const finalized = await createMaterializationDirectoryFinalizedEntry(binding, reopened, {
      kind: MaterializationLedgerDirectoryOutcome.Finalized,
    })
    const isolated = await createMaterializationDirectoryFinalizedEntry(binding, reopened, {
      kind: MaterializationLedgerDirectoryOutcome.IsolatedFailure,
      fault: outputFault(FaultScope.DirectoryLocal, OutputFaultCode.DirectoryMetadata),
    })

    expect({
      admittedId: root.entryId,
      admittedDigest: root.entryDigest,
      finalizedId: finalized.entryId,
      finalizedDigest: finalized.entryDigest,
    }).toEqual({
      admittedId: 'DYW3tJY65TA8lnj8Oi9PJeVG_rhnTnFAxI1gjf7Qtb8',
      admittedDigest: 't1EuLfqAewka8ASKcM1wkfmExM5w33MT3Uj6_m_lYBI',
      finalizedId: 'Bg5V_le9Rvg4ybLHeGqwskkDSQWVPv4pP9U5ywAiLBE',
      finalizedDigest: 'Y2byX3Ur7p7pvXfejiV1fg-egmoqeUhdmdBnnDJntk0',
    })
    expect(reopened).toEqual(root)
    expect(classifyMaterializationLedgerAppend(root, reopened)).toBe('idempotent')
    await expect(decodeMaterializationLedgerEntryV1(finalized, binding))
      .resolves.toEqual(finalized)
    await expect(decodeMaterializationLedgerEntryV1(isolated, binding))
      .resolves.toEqual(isolated)
    await expect(createMaterializationDirectoryAdmittedEntry(binding, {
      ...root,
      token: identity(32, 99),
    } as never)).rejects.toThrow('fields are not exact')
  })

  it('detects path-key claims across file/directory kinds and rejects changed immutable rows', async () => {
    const binding = await ledgerBinding()
    const file = (await createFinalizedFileMaterializationRecords({
      binding,
      finalOutput: verifiedFinalOutput(finalCheckpoint(['same'], 4, 1n)),
      finalCheckpoint: finalCheckpoint(['same'], 4, 1n),
    })).ledgerEntry
    const directory = await createMaterializationDirectoryAdmittedEntry(binding, {
      relativePath: path(['same']),
      directoryId: identity(16, 30),
      generation: identity(16, 31),
      ownedObjectId: identity(32, 32),
      parent: parentCoordinates(),
    })

    expect(file.pathKey).toBe(directory.pathKey)
    expect(file.entryOrder).toBe(directory.entryOrder)
    expect(() => classifyMaterializationLedgerAppend(file, directory))
      .toThrow('immutable path/order entry conflicts')
    expect(classifyMaterializationLedgerAppend(undefined, file)).toBe('insert')
    expect(classifyMaterializationLedgerAppend(file, file)).toBe('idempotent')
  })

  it('rejects foreign bindings, projection changes, unknown fields, and malformed paths', async () => {
    const binding = await ledgerBinding()
    const foreign = await createMaterializationLedgerBinding({
      operationId: identity(16, 40),
      receiveIntentDigest: binding.receiveIntentDigest,
      materializationBindingDigest: binding.materializationBindingDigest,
      authorityRef: binding.authorityRef,
    })
    const checkpoint = finalCheckpoint(['file.bin'], 4, 12n)
    const entry = (await createFinalizedFileMaterializationRecords({
      binding,
      finalOutput: verifiedFinalOutput(checkpoint),
      finalCheckpoint: checkpoint,
    })).ledgerEntry

    await expect(decodeMaterializationLedgerEntryV1(entry, foreign))
      .rejects.toThrow('foreign binding')
    await expect(decodeMaterializationLedgerEntryV1({
      ...entry,
      relativePath: path(['physical-compatible-name.bin']),
    }, binding)).rejects.toThrow('projection disagrees')
    await expect(decodeMaterializationLedgerEntryV1({
      ...entry,
      logicalArtifactPath: ['logical-name.bin'],
    }, binding)).rejects.toThrow('fields are not exact')
    await expect(createMaterializationDirectoryAdmittedEntry(binding, {
      relativePath: ['\ud800'] as never,
      directoryId: identity(16, 30),
      generation: identity(16, 31),
      ownedObjectId: identity(32, 32),
      parent: parentCoordinates(),
    })).rejects.toThrow()
  })

  it('rejects a final-output proof whose exact path, object, source, or size is stale', async () => {
    const binding = await ledgerBinding()
    const checkpoint = finalCheckpoint(['file.bin'], 4, 12n)
    const valid = verifiedFinalOutput(checkpoint)
    const variants = [
      new VerifiedFinalOutputFile(
        { ...valid.ownership, canonicalPath: path(['other.bin']) },
        valid.source,
        valid.fileSize,
      ),
      new VerifiedFinalOutputFile(
        { ...valid.ownership, ownedFileIdentity: identity(32, 80) },
        valid.source,
        valid.fileSize,
      ),
      new VerifiedFinalOutputFile(
        valid.ownership,
        { ...valid.source, fileRevision: identity(16, 81) },
        valid.fileSize,
      ),
      new VerifiedFinalOutputFile(valid.ownership, valid.source, valid.fileSize + 1n),
    ]

    for (const finalOutput of variants) {
      await expect(createFinalizedFileMaterializationRecords({
        binding,
        finalOutput,
        finalCheckpoint: checkpoint,
      })).rejects.toThrow('disagrees with its final checkpoint')
    }
  })
})

async function ledgerBinding() {
  return createMaterializationLedgerBinding({
    operationId: identity(16, 1),
    receiveIntentDigest: identity(32, 2),
    materializationBindingDigest: identity(32, 3),
    authorityRef: identity(32, 6),
  })
}

function finalCheckpoint(
  relativePath: readonly string[],
  fileByte: number,
  exactSize: bigint,
) {
  return newFileCheckpointV2({
    operationId: identity(16, 1),
    receiveIntentDigest: identity(32, 2),
    materializationBindingDigest: identity(32, 3),
    fileId: identity(16, fileByte),
    fileRevision: identity(16, fileByte + 1),
    canonicalPath: relativePath,
    exactSize,
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    authorityRef: identity(32, 6),
    ownedObjectId: identity(32, fileByte + 2),
    stateGeneration: 1n,
    checkpointGeneration: 2n,
    verifiedRanges: exactSize === 0n ? [] : [{ start: 0n, end: exactSize }],
    phase: FILE_CHECKPOINT_PHASE_ACTIVE,
    commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
  })
}

function verifiedFinalOutput(checkpoint: ReturnType<typeof finalCheckpoint>) {
  return new VerifiedFinalOutputFile(
    {
      backend: 'browser-fsa',
      outputSessionId: 'output-session-1',
      canonicalPath: path(checkpoint.canonicalPath),
      ownedFileIdentity: checkpoint.ownedObjectId,
    },
    {
      shareInstance: identity(16, 9),
      fileId: checkpoint.fileId,
      fileRevision: checkpoint.fileRevision,
    },
    checkpoint.exactSize,
  )
}

function parentCoordinates() {
  return {
    relativePath: path([]),
    directoryId: identity(16, 20),
    generation: identity(16, 21),
    ownedObjectId: identity(32, 22),
  }
}

function path(value: readonly string[]) {
  return snapshotMaterializationRootRelativePath(value)
}

function identity(width: number, value: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(value))
}
