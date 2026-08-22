import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import { sha256 } from '../../src/crypto/digest'
import * as receiveContract from '../../src/transfer/intent'
import {
  BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
  DEFAULT_PORTABLE_ARTIFACT_LIMIT,
  DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES,
  DEFAULT_PORTABLE_MAXIMUM_PARTS,
  DESTINATION_RESERVATION_VERSION,
  MATERIALIZATION_PLAN_VERSION,
  RECEIVE_INTENT_VERSION,
  browserHandoffGuarantees,
  canonicalReceiveIntentBytes,
  collisionName,
  createArtifactChoiceIdentity,
  createCatalogRootDirectoryTreeArtifact,
  createDirectorySelectionResultRoot,
  createDirectAtomicPlan,
  createDirectResumableZipPlan,
  createDirectTreePlan,
  createFSAOwnedFileBinding,
  createFSANamedEntryReservation,
  createManagedAtomicReservation,
  createNativeContainerRootReservation,
  createOriginalFileArtifact,
  createPortableBinding,
  createPortableHandoffPlan,
  createReceiveIntent,
  createResultRootDirectoryTreeArtifact,
  createSelectionSpec,
  createSingleFileDirectoryTreeArtifact,
  createSyntheticSelectionResultRoot,
  createWorkspaceBinding,
  createWorkspaceThenPublishPlan,
  createZipArchiveArtifact,
  decodeReceiveIntent,
  deriveArtifactChoiceIdentity,
  fsaOwnedFileGuarantees,
  fsaTreeGuarantees,
  managedAtomicGuarantees,
  materializationPlanBindingDigest,
  nativeTreeGuarantees,
  receiveIntentDigest,
  unavailableDirectZipPolicyDigests,
  validateReceiveIntent,
} from '../../src/transfer/intent'
import type { ReceiveIntent, SelectionSpec } from '../../src/transfer/intent'

function identity(seed: number, width = 16): string {
  const bytes = new Uint8Array(width)
  bytes[0] = seed
  bytes[bytes.length - 1] = seed ^ 0xff
  return encodeBase64Url(bytes)
}

async function selection(): Promise<SelectionSpec> {
  return createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: {
      mode: 'node-id',
      defaultSelected: true,
      rules: [
        { kind: 'file', id: identity(5), selected: false },
        { kind: 'directory', id: identity(4), selected: true },
      ],
    },
  })
}

describe('ReceiveIntent canonical authority', () => {
  it('binds exact selection, artifact, reservation, plan, and operation bytes', async () => {
    const selectionSpec = await selection()
    const artifact = await createCatalogRootDirectoryTreeArtifact()
    const reservation = await createNativeContainerRootReservation({
      operationId: identity(10),
      reservationId: identity(11),
      artifact,
      authorityRef: identity(12, 32),
    })
    const plan = await createDirectTreePlan(artifact, reservation)
    const intent = await createReceiveIntent({ selection: selectionSpec, artifact, plan })

    expect(intent.version).toBe(RECEIVE_INTENT_VERSION)
    expect(intent.operationId).toBe(identity(10))
    expect(intent.bindingDigest).toBe(materializationPlanBindingDigest(plan))
    expect(intent.shareInstance).toBe(selectionSpec.shareInstance)
    expect(intent.syntheticRoot).toBe(selectionSpec.syntheticRoot)
    expect(intent.canonicalBytes).toEqual(canonicalReceiveIntentBytes({
      selection: selectionSpec,
      artifact,
      plan,
    }))
    expect(intent.digest).toBe(encodeBase64Url(await sha256(intent.canonicalBytes)))
    expect(intent.digest).toBe('_-zVJFYomTUJqymN56HiUL1q6NyF_a69IEmAdVUMLl0')
    expect(await receiveIntentDigest(intent)).toBe(intent.digest)
    const decoded = await decodeReceiveIntent(intent.canonicalBytes)
    expect(decoded).toEqual(intent)
    expect(decoded).not.toBe(intent)
    expect(canonicalReceiveIntentBytes(decoded)).toEqual(intent.canonicalBytes)
    await expect(validateReceiveIntent(decoded)).resolves.toEqual(decoded)

    expectReceiveIntentV3Prefix(intent, selectionSpec.canonicalBytes.byteLength)
    const callerBytes = intent.canonicalBytes
    const firstByte = callerBytes[0]
    if (firstByte === undefined) {
      throw new Error('ReceiveIntent canonical bytes must not be empty')
    }
    callerBytes[0] = firstByte ^ 0xff
    await expect(validateReceiveIntent(intent)).resolves.toEqual(intent)
  })

  it('constructs only the closed artifact and plan combinations', async () => {
    const selectionSpec = await selection()
    const pathSelectionSpec = await createSelectionSpec({
      shareInstance: identity(1),
      syntheticRoot: identity(2),
      rules: {
        mode: 'catalog-path',
        defaultSelected: false,
        paths: ['reports/annual.txt', 'docs/report.txt'],
      },
    })
    const operationId = identity(20)
    const reservationId = identity(21)
    const authorityRef = identity(22, 32)
    const repositoryRef = identity(23, 32)
    const fileId = identity(24)
    const directoryId = identity(25)

    const original = await createOriginalFileArtifact({
      fileId,
      sourcePath: 'docs/report.txt',
      suggestedName: 'report.txt',
    })
    const singleTree = await createSingleFileDirectoryTreeArtifact({
      fileId,
      sourcePath: 'docs/report.txt',
      outputName: 'report.txt',
    })
    const resultRoot = createDirectorySelectionResultRoot(directoryId, 'docs')
    const resultTree = await createResultRootDirectoryTreeArtifact(resultRoot)
    const syntheticRoot = createSyntheticSelectionResultRoot()
    const archive = await createZipArchiveArtifact(resultRoot)

    const fsaReservation = await createFSANamedEntryReservation({
      operationId,
      reservationId,
      artifact: resultTree,
      authorityRef,
      logicalReservedName: 'docs-selection',
      physicalName: 'docs-selection.windshare-abcdef',
      collisionIndex: 0,
    })
    const directTree = await createDirectTreePlan(resultTree, fsaReservation)
    const atomicReservation = await createManagedAtomicReservation({
      operationId,
      reservationId,
      artifact: original,
      authorityRef,
      nameAuthority: 'user-chosen',
      requestedName: 'report.txt',
      reservedName: 'report.txt',
      collisionIndex: 0,
    })
    const directAtomic = await createDirectAtomicPlan(original, atomicReservation)
    const workspace = await createWorkspaceBinding({
      operationId,
      workspaceId: identity(26),
      artifact: archive,
      repositoryRef,
    })
    const workspacePlan = await createWorkspaceThenPublishPlan(archive, workspace)
    const portable = await createPortableBinding({
      operationId,
      portablePlanId: identity(27),
      artifact: original,
    })
    const portablePlan = await createPortableHandoffPlan(original, portable)
    await expect(createFSAOwnedFileBinding({
      operationId,
      artifact: archive,
      stableName: `docs-selection.windshare-${identity(30)}.zip`,
      targetRef: identity(31, 32),
      policies: unavailableDirectZipPolicyDigests(),
    })).rejects.toThrow(/absent/u)
    // Opaque nonzero fixture digests exercise the closed codec; they are not production policy evidence.
    const directBinding = await createFSAOwnedFileBinding({
      operationId,
      artifact: archive,
      stableName: `docs-selection.windshare-${identity(30)}.zip`,
      targetRef: identity(31, 32),
      policies: {
        zipEncoding: identity(32, 32),
        layout: identity(33, 32),
        checkpoint: identity(34, 32),
        journalBudget: identity(35, 32),
        epoch: identity(36, 32),
      },
    })
    const directZip = await createDirectResumableZipPlan(archive, directBinding)

    expect(directTree.kind).toBe('direct-tree')
    expect(directTree.version).toBe(MATERIALIZATION_PLAN_VERSION)
    expect(fsaReservation).toMatchObject({
      version: DESTINATION_RESERVATION_VERSION,
      logicalReservedName: 'docs-selection',
      physicalName: 'docs-selection.windshare-abcdef',
    })
    expect(directAtomic.kind).toBe('direct-atomic')
    expect(atomicReservation.requestedName).toBe('report.txt')
    expect(directZip.kind).toBe('direct-resumable-zip')
    expect(directBinding.guarantees).toEqual(fsaOwnedFileGuarantees())
    expect((await deriveArtifactChoiceIdentity(archive, directZip)).id).toBe(
      '0dkx9vDTzvH7B7a9EUoJBOWLCWgmVwLoFH3jjRmfHFU',
    )
    expect((await createArtifactChoiceIdentity({
      artifactKind: 'zip-archive',
      materializationKind: 'workspace-then-publish',
      guaranteeProfile: 'browser-handoff',
      preparation: 'exact-zip',
    })).id).toBe('RW0aXukzHVFiMjNEaoYb8qGKTN-AKAhw7u-Yi_-WsoQ')
    expect(workspacePlan.preparation).toBe('exact-zip')
    expect(portablePlan.publicationGuarantee).toBe('browser-handoff')
    expect(portable).toMatchObject({
      maximumArtifactBytes: DEFAULT_PORTABLE_ARTIFACT_LIMIT,
      assemblyPartBytes: DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES,
      maximumParts: DEFAULT_PORTABLE_MAXIMUM_PARTS,
      objectUrlLeaseMilliseconds: BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
    })
    expect(syntheticRoot.name).toBe('windshare')
    expect(archive.suggestedName).toBe('docs-selection.zip')
    expect(receiveContract.RESULT_NAME_POLICY).toBe('windshare/result-name/v1-unicode-15.0.0')
    expect(receiveContract.COLLISION_SUFFIX_HEX_CHARS).toBe(10)
    expect(nativeTreeGuarantees()).toEqual({
      profile: 'native-tree',
      nameAuthority: 'application-chosen',
      replacement: 'atomic-no-replace',
      delivery: 'managed-target',
      targetVisibility: 'committed-objects-visible',
      artifactAvailability: 'committed-objects-usable',
      cleanupAuthority: 'no-whole-target-rollback',
    })
    expect(fsaTreeGuarantees().replacement).toBe('coordinated-no-replace')
    expect(managedAtomicGuarantees('user-chosen').targetVisibility).toBe(
      'hidden-until-verified-publication',
    )
    expect(browserHandoffGuarantees().delivery).toBe('browser-handoff')

    await expect(createDirectTreePlan(singleTree, atomicReservation)).rejects.toThrow()
    await expect(createDirectAtomicPlan(resultTree, fsaReservation)).rejects.toThrow()
    await expect(createDirectAtomicPlan(archive, atomicReservation)).rejects.toThrow()
    await expect(createWorkspaceBinding({
      operationId,
      workspaceId: identity(28),
      artifact: resultTree,
      repositoryRef,
    })).rejects.toThrow(/complete artifact/u)
    await expect(createPortableBinding({
      operationId,
      portablePlanId: identity(29),
      artifact: resultTree,
    })).rejects.toThrow(/complete artifact/u)

    for (const [artifact, plan, intentSelection] of [
      [resultTree, directTree, selectionSpec],
      [original, directAtomic, selectionSpec],
      [archive, directZip, selectionSpec],
      [archive, workspacePlan, selectionSpec],
      [original, portablePlan, pathSelectionSpec],
    ] as const) {
      const intent = await createReceiveIntent({
        selection: intentSelection,
        artifact,
        plan,
      })
      const decoded = await decodeReceiveIntent(intent.canonicalBytes)
      expect(decoded).toEqual(intent)
      expect(decoded.operationId).toBe(operationId)
      expect(decoded.artifact.digest).toBe(artifact.digest)
      expect(decoded.bindingDigest).toBe(materializationPlanBindingDigest(plan))
      await expect(validateReceiveIntent(decoded)).resolves.toEqual(decoded)
      await expect(decodeReceiveIntent(intent.canonicalBytes.slice(0, -1))).rejects.toThrow(
        /canonical bytes/u,
      )
    }
  })

  it('rejects malformed bytes without normalizing persisted authority', async () => {
    const selectionSpec = await selection()
    const artifact = await createOriginalFileArtifact({
      fileId: identity(60),
      sourcePath: 'report.txt',
      suggestedName: 'report.txt',
    })
    const reservation = await createManagedAtomicReservation({
      operationId: identity(61),
      reservationId: identity(62),
      artifact,
      authorityRef: identity(63, 32),
      nameAuthority: 'application-chosen',
      requestedName: 'report.txt',
      reservedName: 'report.txt',
      collisionIndex: 0,
    })
    const directPlan = await createDirectAtomicPlan(artifact, reservation)
    const directIntent = await createReceiveIntent({
      selection: selectionSpec,
      artifact,
      plan: directPlan,
    })
    const workspace = await createWorkspaceBinding({
      operationId: identity(64),
      workspaceId: identity(65),
      artifact,
      repositoryRef: identity(66, 32),
    })
    const workspacePlan = await createWorkspaceThenPublishPlan(artifact, workspace)
    const workspaceIntent = await createReceiveIntent({
      selection: selectionSpec,
      artifact,
      plan: workspacePlan,
    })
    const portable = await createPortableBinding({
      operationId: identity(67),
      portablePlanId: identity(68),
      artifact,
    })
    const portablePlan = await createPortableHandoffPlan(artifact, portable)
    const portableIntent = await createReceiveIntent({
      selection: selectionSpec,
      artifact,
      plan: portablePlan,
    })
    const otherArtifact = await createOriginalFileArtifact({
      fileId: identity(69),
      sourcePath: 'summary.txt',
      suggestedName: 'summary.txt',
    })

    const directBytes = directIntent.canonicalBytes
    const directFrames = receiveIntentFrames(directBytes)
    const workspacePreparation = planField(workspaceIntent.canonicalBytes, 1)
    const portableRoute = planField(portableIntent.canonicalBytes, 0)
    const malformed: readonly (readonly [string, Uint8Array])[] = [
      ['trailing intent byte', appendBytes(directBytes, Uint8Array.of(0))],
      ['truncated intent', directBytes.slice(0, directBytes.byteLength - 1)],
      ['wrong intent domain', mutateByte(directBytes, 0, directBytes[0]! ^ 1)],
      ['legacy V2 envelope', legacyReceiveIntentV2Bytes(directBytes)],
      ['unknown artifact kind', mutateByte(
        directBytes,
        directFrames.artifact.payloadOffset + recordFieldsOffset(ARTIFACT_SPEC_TEST_DOMAIN),
        0xff,
      )],
      ['unknown plan kind', mutateByte(
        directBytes,
        directFrames.plan.payloadOffset + recordFieldsOffset(MATERIALIZATION_PLAN_TEST_DOMAIN),
        0xff,
      )],
      ['unsorted selection rules', swapNodeSelectionRules(directBytes)],
      ['zero operation identity', zeroDirectOperationIdentity(directBytes)],
      ['wrong managed guarantee', mutateManagedGuaranteeNameAuthority(directBytes, 3)],
      ['artifact and plan mismatch', malformedReceiveIntentEnvelope(
        selectionSpec.canonicalBytes,
        otherArtifact.canonicalBytes,
        directPlan.canonicalBytes,
      )],
      ['workspace preparation drift', mutateByte(
        workspaceIntent.canonicalBytes,
        workspacePreparation.payloadOffset,
        1,
      )],
      ['portable guarantee drift', mutateByte(
        portableIntent.canonicalBytes,
        portableRoute.payloadOffset,
        1,
      )],
      ['portable limit drift', mutatePortableArtifactLimit(portableIntent.canonicalBytes)],
      ['artifact name drift', mutateOriginalArtifactName(directBytes)],
    ]

    for (const [name, encoded] of malformed) {
      let error: unknown
      try {
        await decodeReceiveIntent(encoded)
      } catch (candidate) {
        error = candidate
      }
      expect(error, name).toBeInstanceOf(Error)
      expect((error as Error).message, name).toMatch(/canonical bytes/u)
    }
  })
})

describe('ReceiveIntent input integrity', () => {
  it('rejects non-canonical selection, names, identities, and collision decisions', async () => {
    await expect(createSelectionSpec({
      shareInstance: identity(1),
      syntheticRoot: identity(2),
      rules: {
        mode: 'catalog-path',
        defaultSelected: false,
        paths: ['docs/re\u0301sume\u0301.txt'],
      },
    })).rejects.toThrow(/canonical/u)
    await expect(createSelectionSpec({
      shareInstance: identity(1),
      syntheticRoot: identity(2),
      rules: {
        mode: 'catalog-path',
        defaultSelected: false,
        paths: ['docs/report.txt', 'docs/report.txt'],
      },
    })).rejects.toThrow(/duplicate/u)
    await expect(createSelectionSpec({
      shareInstance: identity(1),
      syntheticRoot: identity(2),
      rules: {
        mode: 'node-id',
        defaultSelected: false,
        rules: [
          { kind: 'directory', id: identity(9), selected: true },
          { kind: 'file', id: identity(9), selected: true },
        ],
      },
    })).rejects.toThrow(/duplicate/u)
    await expect(createOriginalFileArtifact({
      fileId: identity(30),
      sourcePath: 'docs/report.txt',
      suggestedName: 'renamed.txt',
    })).rejects.toThrow(/source-path leaf/u)
    await expect(createOriginalFileArtifact({
      fileId: identity(30),
      sourcePath: '.wsresume-secret',
      suggestedName: '.wsresume-secret',
    })).rejects.toThrow(/path policy|portable policy/u)
    await expect(collisionName(identity(31), 'report.txt', -1, true)).rejects.toThrow(/32-bit/u)

    const first = await collisionName(identity(31), 'report.txt', 1, true)
    const repeated = await collisionName(identity(31), 'report.txt', 1, true)
    const secondOperation = await collisionName(identity(32), 'report.txt', 1, true)
    expect(first).toBe(repeated)
    expect(first).toMatch(/^report-[0-9a-f]{10}\.txt$/u)
    expect(secondOperation).not.toBe(first)
  })

  it('recomputes canonical authority and exposes no legacy draft/output surface', async () => {
    const selectionSpec = await selection()
    const artifact = await createOriginalFileArtifact({
      fileId: identity(40),
      sourcePath: 'report.txt',
      suggestedName: 'report.txt',
    })
    const reservation = await createManagedAtomicReservation({
      operationId: identity(41),
      reservationId: identity(42),
      artifact,
      authorityRef: identity(43, 32),
      nameAuthority: 'application-chosen',
      requestedName: 'report.txt',
      reservedName: 'report.txt',
      collisionIndex: 0,
    })
    const plan = await createDirectAtomicPlan(artifact, reservation)
    const intent = await createReceiveIntent({ selection: selectionSpec, artifact, plan })
    const forgedDigest = { ...intent, digest: identity(44, 32) } as ReceiveIntent
    const forgedOperation = { ...intent, operationId: identity(45) } as ReceiveIntent
    const forgedBytes = {
      ...intent,
      canonicalBytes: Uint8Array.from(intent.canonicalBytes, (byte, index) =>
        index === 0 ? byte ^ 1 : byte),
    } as ReceiveIntent

    expect(() => canonicalReceiveIntentBytes({
      selection: selectionSpec,
      artifact,
      plan: { ...plan },
    })).toThrow(/created or validated/u)

    await expect(validateReceiveIntent(forgedDigest)).rejects.toThrow(/digest/u)
    await expect(validateReceiveIntent(forgedOperation)).rejects.toThrow(/derived authority/u)
    await expect(validateReceiveIntent(forgedBytes)).rejects.toThrow(/canonical bytes/u)
    expect('createTransferIntentDraft' in receiveContract).toBe(false)
    expect('freezeTransferIntent' in receiveContract).toBe(false)
    expect('TransferOutputLocator' in receiveContract).toBe(false)
    expect(Object.hasOwn(intent, 'output')).toBe(false)
  })
})

function expectReceiveIntentV3Prefix(intent: ReceiveIntent, selectionBytes: number): void {
  const prefix = new TextEncoder().encode('windshare/receive-intent/v3\0')
  expect(intent.canonicalBytes.slice(0, prefix.byteLength)).toEqual(prefix)
  expect(intent.canonicalBytes[prefix.byteLength]).toBe(RECEIVE_INTENT_VERSION)
  const firstFrame = intent.canonicalBytes.slice(prefix.byteLength + 1)
  expect(readUint64(firstFrame)).toBe(BigInt(selectionBytes))
}

function readUint64(value: Uint8Array): bigint {
  return new DataView(value.buffer, value.byteOffset, 8).getBigUint64(0)
}

const TEST_TEXT_ENCODER = new TextEncoder()
const RECEIVE_INTENT_TEST_DOMAIN = 'windshare/receive-intent/v3'
const SELECTION_SPEC_TEST_DOMAIN = 'windshare/selection-spec/v1'
const ARTIFACT_SPEC_TEST_DOMAIN = 'windshare/artifact-spec/v1'
const DESTINATION_RESERVATION_TEST_DOMAIN = 'windshare/destination-reservation/v3'
const PORTABLE_BINDING_TEST_DOMAIN = 'windshare/portable-binding/v1'
const MATERIALIZATION_PLAN_TEST_DOMAIN = 'windshare/materialization-plan/v3'

interface FrameLocation {
  readonly payloadOffset: number
  readonly payloadLength: number
  readonly nextOffset: number
}

function recordFieldsOffset(domain: string): number {
  return TEST_TEXT_ENCODER.encode(domain).byteLength + 2
}

function recordVersionOffset(domain: string): number {
  return TEST_TEXT_ENCODER.encode(domain).byteLength + 1
}

function legacyReceiveIntentV2Bytes(value: Uint8Array): Uint8Array {
  const domainV2 = mutateByte(
    value, TEST_TEXT_ENCODER.encode(RECEIVE_INTENT_TEST_DOMAIN).byteLength - 1, '2'.charCodeAt(0),
  )
  return mutateByte(domainV2, recordVersionOffset(RECEIVE_INTENT_TEST_DOMAIN), 2)
}

function locateFrame(value: Uint8Array, offset: number): FrameLocation {
  const length = readUint64(value.subarray(offset))
  if (length > BigInt(Number.MAX_SAFE_INTEGER)) throw new Error('test frame is too large')
  const payloadLength = Number(length)
  const payloadOffset = offset + 8
  const nextOffset = payloadOffset + payloadLength
  if (nextOffset > value.byteLength) throw new Error('test frame is truncated')
  return { payloadOffset, payloadLength, nextOffset }
}

function receiveIntentFrames(value: Uint8Array): Readonly<{
  selection: FrameLocation
  artifact: FrameLocation
  plan: FrameLocation
}> {
  const selection = locateFrame(value, recordFieldsOffset(RECEIVE_INTENT_TEST_DOMAIN))
  const artifact = locateFrame(value, selection.nextOffset)
  const plan = locateFrame(value, artifact.nextOffset)
  return { selection, artifact, plan }
}

function planBinding(value: Uint8Array): FrameLocation {
  const { plan } = receiveIntentFrames(value)
  return locateFrame(
    value,
    plan.payloadOffset + recordFieldsOffset(MATERIALIZATION_PLAN_TEST_DOMAIN) + 1,
  )
}

function planField(value: Uint8Array, index: number): FrameLocation {
  let offset = planBinding(value).nextOffset
  for (let fieldIndex = 0; fieldIndex < index; fieldIndex += 1) {
    offset = locateFrame(value, offset).nextOffset
  }
  return locateFrame(value, offset)
}

function mutateByte(value: Uint8Array, offset: number, byte: number): Uint8Array {
  if (offset < 0 || offset >= value.byteLength) throw new Error('test mutation is outside the record')
  const mutated = Uint8Array.from(value)
  mutated[offset] = byte
  return mutated
}

function appendBytes(left: Uint8Array, right: Uint8Array): Uint8Array {
  const result = new Uint8Array(left.byteLength + right.byteLength)
  result.set(left)
  result.set(right, left.byteLength)
  return result
}

function swapNodeSelectionRules(value: Uint8Array): Uint8Array {
  const { selection } = receiveIntentFrames(value)
  let offset = selection.payloadOffset + recordFieldsOffset(SELECTION_SPEC_TEST_DOMAIN)
  for (let fieldIndex = 0; fieldIndex < 4; fieldIndex += 1) {
    offset = locateFrame(value, offset).nextOffset
  }
  offset += 8
  const firstStart = offset
  for (let fieldIndex = 0; fieldIndex < 3; fieldIndex += 1) {
    offset = locateFrame(value, offset).nextOffset
  }
  const firstEnd = offset
  const secondStart = offset
  for (let fieldIndex = 0; fieldIndex < 3; fieldIndex += 1) {
    offset = locateFrame(value, offset).nextOffset
  }
  const secondEnd = offset
  if (firstEnd - firstStart !== secondEnd - secondStart) {
    throw new Error('test selection rules must have equal encoded lengths')
  }
  const mutated = Uint8Array.from(value)
  mutated.set(value.slice(secondStart, secondEnd), firstStart)
  mutated.set(value.slice(firstStart, firstEnd), secondStart)
  return mutated
}

function zeroDirectOperationIdentity(value: Uint8Array): Uint8Array {
  const binding = planBinding(value)
  const operation = locateFrame(
    value,
    binding.payloadOffset + recordFieldsOffset(DESTINATION_RESERVATION_TEST_DOMAIN) + 1,
  )
  const mutated = Uint8Array.from(value)
  mutated.fill(0, operation.payloadOffset, operation.nextOffset)
  return mutated
}

function mutateManagedGuaranteeNameAuthority(value: Uint8Array, byte: number): Uint8Array {
  const binding = planBinding(value)
  let offset = binding.payloadOffset +
    recordFieldsOffset(DESTINATION_RESERVATION_TEST_DOMAIN) + 1
  for (let fieldIndex = 0; fieldIndex < 5; fieldIndex += 1) {
    offset = locateFrame(value, offset).nextOffset
  }
  const guarantees = locateFrame(value, offset)
  const profile = locateFrame(value, guarantees.payloadOffset)
  const nameAuthority = locateFrame(value, profile.nextOffset)
  return mutateByte(value, nameAuthority.payloadOffset, byte)
}

function mutatePortableArtifactLimit(value: Uint8Array): Uint8Array {
  const binding = planBinding(value)
  let offset = binding.payloadOffset + recordFieldsOffset(PORTABLE_BINDING_TEST_DOMAIN)
  for (let fieldIndex = 0; fieldIndex < 3; fieldIndex += 1) {
    offset = locateFrame(value, offset).nextOffset
  }
  const maximumArtifactBytes = locateFrame(value, offset)
  const finalByte = maximumArtifactBytes.nextOffset - 1
  return mutateByte(value, finalByte, value[finalByte]! ^ 1)
}

function mutateOriginalArtifactName(value: Uint8Array): Uint8Array {
  const { artifact } = receiveIntentFrames(value)
  let offset = artifact.payloadOffset + recordFieldsOffset(ARTIFACT_SPEC_TEST_DOMAIN) + 1
  offset = locateFrame(value, offset).nextOffset
  offset = locateFrame(value, offset).nextOffset
  const name = locateFrame(value, offset)
  return mutateByte(value, name.payloadOffset, value[name.payloadOffset]! ^ 1)
}

function malformedReceiveIntentEnvelope(
  selection: Uint8Array,
  artifact: Uint8Array,
  plan: Uint8Array,
): Uint8Array {
  return appendMany([
    TEST_TEXT_ENCODER.encode(RECEIVE_INTENT_TEST_DOMAIN),
    Uint8Array.of(0, 3),
    testFrame(selection),
    testFrame(artifact),
    testFrame(plan),
  ])
}

function testFrame(value: Uint8Array): Uint8Array {
  const length = new Uint8Array(8)
  new DataView(length.buffer).setBigUint64(0, BigInt(value.byteLength))
  return appendMany([length, value])
}

function appendMany(parts: readonly Uint8Array[]): Uint8Array {
  const result = new Uint8Array(parts.reduce((total, part) => total + part.byteLength, 0))
  let offset = 0
  for (const part of parts) {
    result.set(part, offset)
    offset += part.byteLength
  }
  return result
}
