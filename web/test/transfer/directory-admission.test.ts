import { describe, expect, it } from 'vitest'

import { decodeBase64Url, encodeBase64Url } from '../../src/crypto/bytes'
import { sha256 } from '../../src/crypto/digest'
import * as admissionContract from '../../src/transfer/directory-admission'
import {
  DIRECTORY_ADMISSION_LAYOUT_VERSION,
  DIRECTORY_ADMISSION_SCHEMA_VERSION,
  DirectoryAdmissionBindingError,
  canonicalDirectoryAdmissionMessageV2,
  createDirectoryAdmission,
  createDirectoryAdmissionScope,
  finalizedDirectorySettlement,
  isolatedDirectorySettlement,
  sameDirectoryAdmission,
  validateDirectoryAdmissionBinding,
  validateDirectorySettlement,
  verifyDirectoryAdmissionToken,
  type CanonicalModifiedTime,
  type DirectoryAdmissionScope,
  type MaterializationDirectory,
} from '../../src/transfer/directory-admission'
import {
  DirectoryAdmissionLedger,
  DirectoryAdmissionLimitError,
} from '../../src/transfer/directory-admission-ledger'
import { FaultScope, OutputFaultCode, outputFault } from '../../src/transfer/fault'
import {
  createDirectorySelectionResultRoot,
  createDirectAtomicPlan,
  createDirectTreePlan,
  createFSANamedEntryReservation,
  createManagedAtomicReservation,
  createOriginalFileArtifact,
  createReceiveIntent,
  createResultRootDirectoryTreeArtifact,
  createSelectionSpec,
  createWorkspaceBinding,
  createWorkspaceThenPublishPlan,
  type ReceiveIntent,
} from '../../src/transfer/intent'

const SECRET = Uint8Array.from({ length: 32 }, (_, index) => index + 1)

function identity(seed: number, width = 16): string {
  const value = new Uint8Array(width)
  value[0] = seed
  value[value.length - 1] = seed ^ 0xff
  return encodeBase64Url(value)
}

describe('DirectoryAdmission v2 binding', () => {
  it('derives a closed scope from a legal intent and matches the independent canonical message', async () => {
    const intent = await directTreeIntent()
    const scope = await createDirectoryAdmissionScope(intent)
    const modifiedTime: CanonicalModifiedTime = {
      seconds: -1_234n,
      nanoseconds: 567_000_000,
      precision: 2,
    }
    const rootDirectory: MaterializationDirectory = {
      directoryId: scope.syntheticRoot,
      generation: identity(40),
      path: [],
    }
    const root = await createDirectoryAdmission(SECRET, scope, rootDirectory)
    const childDirectory: MaterializationDirectory = {
      directoryId: identity(41),
      generation: identity(42),
      path: ['child'],
      parentAdmission: root,
      modifiedTime,
    }
    const child = await createDirectoryAdmission(SECRET, scope, childDirectory)

    expect(intent.digest).toBe('PKgx50isr9LVzH14HJib08_RPM501Cvw1_G2Y2b_ctQ')
    expect(root.token).toBe('wciDHOmyvV1fBGC2T_jZZlUN2dbt-T3s7YC1PFXmzI0')
    expect(child.token).toBe('VfTe1srkTkvVQNeSJS1oaYl8MZ_BBtMPEn4ZiATmL8w')

    expect(scope).toMatchObject({
      receiveIntentDigest: intent.digest,
      layoutVersion: DIRECTORY_ADMISSION_LAYOUT_VERSION,
      layout: 'directory-tree-result-root',
      syntheticRoot: intent.syntheticRoot,
    })
    expect(child.schemaVersion).toBe(DIRECTORY_ADMISSION_SCHEMA_VERSION)
    const message = canonicalDirectoryAdmissionMessageV2(scope, childDirectory)
    expect(message).toEqual(expectedDirectoryAdmissionMessage(scope, childDirectory))
    expect(encodeBase64Url(await sha256(message)))
      .toBe('-FfVAg8-AJUOylS_dYhiCz9vF2DrUTncrpE-PhQ7HOc')
    expect(validateDirectoryAdmissionBinding(scope, childDirectory, child)).toEqual(child)
    expect(await verifyDirectoryAdmissionToken(SECRET, scope, childDirectory, child.token)).toBe(true)
    expect(await verifyDirectoryAdmissionToken(
      Uint8Array.from(SECRET, (byte, index) => index === 0 ? byte ^ 1 : byte),
      scope,
      childDirectory,
      child.token,
    )).toBe(false)

    const forgedScope = { ...scope } as DirectoryAdmissionScope
    await expect(createDirectoryAdmission(SECRET, forgedScope, rootDirectory))
      .rejects.toThrow(/derived from a validated receive intent/u)
    await expect(createDirectoryAdmission(SECRET, scope, {
      ...rootDirectory,
      directoryId: identity(43),
    })).rejects.toThrow(/synthetic root/u)
    expect('canonicalDirectoryAdmissionMessageV1' in admissionContract).toBe(false)
  })

  it('rejects prepared and original-file plans', async () => {
    const workspaceIntent = await workspaceIntentForOriginal()
    await expect(createDirectoryAdmissionScope(workspaceIntent))
      .rejects.toThrow(/sealed manifest/u)

    const original = workspaceIntent.artifact
    if (original.kind !== 'original-file') throw new Error('fixture artifact mismatch')
    const reservation = await createManagedAtomicReservation({
      operationId: identity(60),
      reservationId: identity(61),
      artifact: original,
      authorityRef: identity(62, 32),
      nameAuthority: 'application-chosen',
      requestedName: 'report.txt',
      reservedName: 'report.txt',
      collisionIndex: 0,
    })
    const directOriginal = await createReceiveIntent({
      selection: workspaceIntent.selection,
      artifact: original,
      plan: await createDirectAtomicPlan(original, reservation),
    })
    await expect(createDirectoryAdmissionScope(directOriginal))
      .rejects.toThrow(/DirectAtomic original-file output/u)
  })

  it('retains exact v2 receipts and only the closed isolated metadata settlement', async () => {
    const scope = await createDirectoryAdmissionScope(await directTreeIntent())
    const directory: MaterializationDirectory = {
      directoryId: scope.syntheticRoot,
      generation: identity(70),
      path: [],
    }
    const admission = await createDirectoryAdmission(SECRET, scope, directory)
    const retry = await createDirectoryAdmission(Uint8Array.from(SECRET), scope, directory)
    const rebound = Object.freeze({ ...retry, generation: identity(71) })

    expect(sameDirectoryAdmission(admission, retry)).toBe(true)
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

describe('DirectoryAdmissionLedger v2', () => {
  it('bounds exact retained metadata and preserves generation/path/settlement invariants', async () => {
    const scope = await createDirectoryAdmissionScope(await directTreeIntent())
    const ledger = new DirectoryAdmissionLedger(scope, {
      secret: SECRET,
      maximumAdmissions: 2,
    })
    const signal = new AbortController().signal
    const rootDirectory: MaterializationDirectory = {
      directoryId: scope.syntheticRoot,
      generation: identity(80),
      path: [],
    }
    const root = await ledger.admitDirectory(rootDirectory, signal)
    const childDirectory: MaterializationDirectory = {
      directoryId: identity(81),
      generation: identity(82),
      path: ['child'],
      parentAdmission: root,
    }
    const child = await ledger.admitDirectory(childDirectory, signal)
    expect(await ledger.admitDirectory(childDirectory, signal)).toBe(child)

    await expect(ledger.admitDirectory({
      directoryId: identity(83),
      generation: identity(84),
      path: ['sibling'],
      parentAdmission: root,
    }, signal)).rejects.toMatchObject({
      limitClass: 'directory-admission-count',
    })
    await expect(ledger.admitDirectory({
      ...childDirectory,
      generation: identity(85),
    }, signal)).rejects.toThrow(/already bound/u)

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

    const bounded = new DirectoryAdmissionLedger(scope, {
      secret: SECRET,
      maximumMetadataBytes: 1,
    })
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

function expectedDirectoryAdmissionMessage(
  scope: DirectoryAdmissionScope,
  directory: MaterializationDirectory,
): Uint8Array {
  const parent = directory.parentAdmission === undefined
    ? new Uint8Array()
    : decodeIdentity(directory.parentAdmission.token)
  return concat([
    new TextEncoder().encode('windshare/directory-admission/v2\0'),
    Uint8Array.of(2),
    frame(decodeIdentity(scope.receiveIntentDigest)),
    frame(Uint8Array.of(1)),
    frame(Uint8Array.of(layoutByte(scope.layout))),
    frame(decodeIdentity(directory.directoryId)),
    frame(decodeIdentity(directory.generation)),
    frame(parent),
    frame(directory.path.length === 0
      ? Uint8Array.of(1)
      : concat([Uint8Array.of(2), frame(canonicalPath(directory.path))])),
    frame(canonicalModifiedTime(directory.modifiedTime)),
  ])
}

function canonicalPath(path: readonly string[]): Uint8Array {
  return concat([
    uint64(BigInt(path.length)),
    ...path.map((segment) => frame(new TextEncoder().encode(segment))),
  ])
}

function canonicalModifiedTime(modified: CanonicalModifiedTime | undefined): Uint8Array {
  if (modified === undefined) return Uint8Array.of(1)
  const seconds = new Uint8Array(8)
  new DataView(seconds.buffer).setBigInt64(0, modified.seconds)
  const nanoseconds = new Uint8Array(4)
  new DataView(nanoseconds.buffer).setUint32(0, modified.nanoseconds)
  return concat([
    Uint8Array.of(2),
    frame(seconds),
    frame(nanoseconds),
    frame(Uint8Array.of(modified.precision)),
  ])
}

function layoutByte(layout: DirectoryAdmissionScope['layout']): number {
  switch (layout) {
    case 'directory-tree-single-file': return 1
    case 'directory-tree-result-root': return 2
    case 'directory-tree-catalog-root': return 3
    case 'zip-result-root': return 4
  }
}

function decodeIdentity(value: string): Uint8Array {
  const decoded = decodeBase64Url(value)
  if (decoded === undefined) throw new TypeError('fixture identity is invalid')
  return decoded
}

function frame(value: Uint8Array): Uint8Array {
  return concat([uint64(BigInt(value.byteLength)), value])
}

function uint64(value: bigint): Uint8Array {
  const output = new Uint8Array(8)
  new DataView(output.buffer).setBigUint64(0, value)
  return output
}

function concat(parts: readonly Uint8Array[]): Uint8Array {
  const output = new Uint8Array(parts.reduce((sum, part) => sum + part.byteLength, 0))
  let offset = 0
  for (const part of parts) {
    output.set(part, offset)
    offset += part.byteLength
  }
  return output
}
