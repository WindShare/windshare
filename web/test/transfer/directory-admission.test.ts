import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  DIRECTORY_ADMISSION_LAYOUT_VERSION,
  DirectoryAdmissionBindingError,
  createDirectoryAdmissionAuthority,
  createDirectoryAdmissionScope,
  finalizedDirectorySettlement,
  isolatedDirectorySettlement,
  sameDirectoryAdmission,
  snapshotMaterializationPath,
  validateDirectoryAdmissionBinding,
  validateDirectorySettlement,
  type CanonicalModifiedTime,
  type DirectoryAdmissionScope,
  type MaterializationDirectory,
} from '../../src/transfer/directory-admission'
import {
  DirectoryAdmissionLedger,
  DirectoryAdmissionLimitError,
} from '../../src/transfer/directory-admission-ledger'
import { FaultScope, OutputFaultCode, outputFault } from '../../src/transfer/failure/fault'
import {
  createDirectorySelectionResultRoot,
  createDirectTreePlan,
  createFSANamedEntryReservation,
  createOriginalFileArtifact,
  createReceiveIntent,
  createResultRootDirectoryTreeArtifact,
  createSelectionSpec,
  createSingleFileDirectoryTreeArtifact,
  createWorkspaceBinding,
  createWorkspaceThenPublishPlan,
  type ReceiveIntent,
} from '../../src/transfer/intent'

function identity(seed: number, width = 16): string {
  const value = new Uint8Array(width)
  value[0] = seed
  value[value.length - 1] = seed ^ 0xff
  return encodeBase64Url(value)
}

describe('DirectoryAdmission binding and session authority', () => {
  it('derives a closed scope from a legal intent and verifies session-local receipts', async () => {
    const intent = await directTreeIntent()
    const scope = await createDirectoryAdmissionScope(intent)
    const authority = createDirectoryAdmissionAuthority(scope)
    const otherAuthority = createDirectoryAdmissionAuthority(scope)
    const modifiedTime: CanonicalModifiedTime = {
      seconds: -1_234n,
      nanoseconds: 567_000_000,
      precision: 2,
    }
    const rootDirectory: MaterializationDirectory = {
      directoryId: identity(10),
      generation: identity(40),
      path: snapshotMaterializationPath([]),
    }
    const root = authority.issue(rootDirectory)
    const childDirectory: MaterializationDirectory = {
      directoryId: identity(41),
      generation: identity(42),
      path: snapshotMaterializationPath(['child']),
      parentAdmission: root,
      modifiedTime,
    }
    const child = authority.issue(childDirectory)

    expect(authority.verify(root)).toBe(true)
    expect(authority.verify(child)).toBe(true)
    expect(otherAuthority.verify(child)).toBe(false)
    expect(authority.verify({ ...child })).toBe(false)
    expect(authority.verify({ ...child, parentAdmissionId: child.admissionId })).toBe(false)
    expect(authority.verify({ ...child, path: snapshotMaterializationPath(['other']) })).toBe(false)

    expect(scope).toMatchObject({
      receiveIntentDigest: intent.digest,
      layoutVersion: DIRECTORY_ADMISSION_LAYOUT_VERSION,
      layout: 'directory-tree-result-root',
      rootExpectation: {
        kind: 'materialized-directory',
        anchorKind: 'directory',
        directoryId: identity(10),
        relativePath: [],
      },
    })
    expect(validateDirectoryAdmissionBinding(scope, childDirectory, child)).toBe(child)
    expect(authority.verify(validateDirectoryAdmissionBinding(scope, childDirectory, child))).toBe(true)

    const forgedScope = { ...scope } as DirectoryAdmissionScope
    expect(() => createDirectoryAdmissionAuthority(forgedScope))
      .toThrow(/derived from a validated receive intent/u)
    expect(() => authority.issue({
      ...rootDirectory,
      directoryId: identity(43),
    })).toThrow(/expected materialized root/u)
    expect(() => authority.issue({
      ...rootDirectory,
      path: snapshotMaterializationPath(['docs-selection']),
    })).toThrow(/expected materialized root/u)
    expect(() => authority.issue({
      directoryId: identity(44),
      generation: identity(45),
      path: snapshotMaterializationPath(['child']),
    })).toThrow(/expected materialized root/u)
    expect(() => otherAuthority.issue(childDirectory))
      .toThrow(/not issued by this admission authority/u)
  })

  it('rejects prepared and original-file plans', async () => {
    const workspaceIntent = await workspaceIntentForOriginal()
    await expect(createDirectoryAdmissionScope(workspaceIntent))
      .rejects.toThrow(/Original files/u)
  })

  it('rejects every directory admission for a single-file root expectation', async () => {
    const scope = await createDirectoryAdmissionScope(await singleFileDirectTreeIntent())
    expect(scope.rootExpectation).toEqual({ kind: 'none', anchorKind: 'single-file' })
    const authority = createDirectoryAdmissionAuthority(scope)

    expect(() => authority.issue({
      directoryId: identity(2),
      generation: identity(69),
      path: snapshotMaterializationPath([]),
    })).toThrow(/expected materialized root/u)
  })

  it('retains exact receipts and only the closed isolated metadata settlement', async () => {
    const scope = await createDirectoryAdmissionScope(await directTreeIntent())
    const authority = createDirectoryAdmissionAuthority(scope)
    const directory: MaterializationDirectory = {
      directoryId: identity(10),
      generation: identity(70),
      path: snapshotMaterializationPath([]),
    }
    const admission = authority.issue(directory)
    const copy = Object.freeze({ ...admission })
    const rebound = Object.freeze({ ...admission, generation: identity(71) })

    expect(sameDirectoryAdmission(admission, copy)).toBe(true)
    expect(sameDirectoryAdmission(admission, rebound)).toBe(false)
    expect(() => validateDirectoryAdmissionBinding(scope, directory, rebound))
      .toThrow(DirectoryAdmissionBindingError)

    const finalized = finalizedDirectorySettlement(admission)
    const isolated = isolatedDirectorySettlement(
      admission,
      outputFault(FaultScope.DirectoryLocal, OutputFaultCode.DirectoryMetadata),
    )
    expect(validateDirectorySettlement(admission, finalized)).toEqual(finalized)
    expect(validateDirectorySettlement(admission, isolated)).toEqual(isolated)
    expect(() => isolatedDirectorySettlement(
      admission,
      outputFault(FaultScope.OutputPause, OutputFaultCode.DirectoryMetadata),
    )).toThrow(/directory-local metadata/u)
  })
})

describe('DirectoryAdmissionLedger', () => {
  it('bounds retained metadata and preserves generation/path/settlement invariants', async () => {
    const scope = await createDirectoryAdmissionScope(await directTreeIntent())
    const ledger = new DirectoryAdmissionLedger(scope, { maximumAdmissions: 2 })
    const signal = new AbortController().signal
    const rootDirectory: MaterializationDirectory = {
      directoryId: identity(10),
      generation: identity(80),
      path: snapshotMaterializationPath([]),
    }
    const root = await ledger.admitDirectory(rootDirectory, signal)
    const childDirectory: MaterializationDirectory = {
      directoryId: identity(81),
      generation: identity(82),
      path: snapshotMaterializationPath(['child']),
      parentAdmission: root,
    }
    const child = await ledger.admitDirectory(childDirectory, signal)
    expect(await ledger.admitDirectory(childDirectory, signal)).toBe(child)

    await expect(ledger.admitDirectory({
      directoryId: identity(83),
      generation: identity(84),
      path: snapshotMaterializationPath(['sibling']),
      parentAdmission: root,
    }, signal)).rejects.toMatchObject({
      limitClass: 'directory-admission-count',
    })
    await expect(ledger.admitDirectory({
      ...childDirectory,
      generation: identity(85),
    }, signal)).rejects.toThrow(/already bound/u)

    expect(() => ledger.finalizeDirectory({ ...child }, signal))
      .toThrow(/forged or foreign receipt/u)

    const lease = ledger.acquireFileMutation({
      path: ['child', 'report.txt'],
      parentAdmission: child,
    })
    let settled = false
    const childSettlement = ledger.finalizeDirectory(
      child,
      signal,
      async () => 'isolated-metadata-failure',
    ).then((result) => {
      settled = true
      return result
    })
    await Promise.resolve()
    expect(settled).toBe(false)
    lease.release()
    expect((await childSettlement).kind).toBe('isolated-failure')
    expect((await ledger.finalizeDirectory(root, signal)).kind).toBe('finalized')

    const bounded = new DirectoryAdmissionLedger(scope, { maximumMetadataBytes: 1 })
    await expect(bounded.admitDirectory(rootDirectory, signal))
      .rejects.toBeInstanceOf(DirectoryAdmissionLimitError)
  })
})

async function directTreeIntent(): Promise<ReceiveIntent> {
  const selection = await selectionSpec()
  const root = createDirectorySelectionResultRoot(identity(10), 'docs')
  const artifact = await createResultRootDirectoryTreeArtifact(root)
  const reservation = await createFSANamedEntryReservation({
    operationId: identity(11),
    reservationId: identity(12),
    artifact,
    authorityRef: identity(13, 32),
    logicalReservedName: 'docs-selection',
    physicalName: 'docs-selection',
    collisionIndex: 0,
  })
  return createReceiveIntent({
    selection,
    artifact,
    plan: await createDirectTreePlan(artifact, reservation),
  })
}

async function singleFileDirectTreeIntent(): Promise<ReceiveIntent> {
  const selection = await selectionSpec()
  const artifact = await createSingleFileDirectoryTreeArtifact({
    fileId: identity(20),
    sourcePath: 'report.txt',
    outputName: 'report.txt',
  })
  const reservation = await createFSANamedEntryReservation({
    operationId: identity(21),
    reservationId: identity(22),
    artifact,
    authorityRef: identity(23, 32),
    logicalReservedName: 'report.txt',
    physicalName: 'report.txt',
    collisionIndex: 0,
  })
  return createReceiveIntent({
    selection,
    artifact,
    plan: await createDirectTreePlan(artifact, reservation),
  })
}

async function workspaceIntentForOriginal(): Promise<ReceiveIntent> {
  const selection = await selectionSpec()
  const artifact = await createOriginalFileArtifact({
    fileId: identity(30),
    sourcePath: 'report.txt',
    suggestedName: 'report.txt',
  })
  const workspace = await createWorkspaceBinding({
    operationId: identity(31),
    workspaceId: identity(32),
    artifact,
    repositoryRef: identity(33, 32),
  })
  return createReceiveIntent({
    selection,
    artifact,
    plan: await createWorkspaceThenPublishPlan(artifact, workspace),
  })
}

function selectionSpec() {
  return createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
}
