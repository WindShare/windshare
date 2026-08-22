import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  FILE_CHECKPOINT_NAMESPACE,
  FILE_CHECKPOINT_OWNERSHIP_MARKER,
  canonicalFileCheckpointBytes,
  canonicalCheckpointLineageBytes,
  checkpointIdentityEqual,
  decodeFileCheckpointV2,
  deriveCheckpointLineageID,
  encodeFileCheckpointV2,
  newFileCheckpointV2,
  selectVerifiedCheckpoint,
  validateFileCheckpointTransition,
  type FileCheckpointV2,
} from '../../src/output/persistence/checkpoint'
import {
  canonicalDirectoryAdmissionMessageV2,
  createDirectoryAdmission,
  createDirectoryAdmissionScope,
  deriveDirectoryAdmissionToken,
  DirectorySettlementKind,
  finalizedDirectorySettlement,
  isolatedDirectorySettlement,
  sameDirectoryAdmission,
  validateDirectorySettlement,
  verifyDirectoryAdmissionToken,
  type CanonicalModifiedTime,
  type DirectoryAdmission,
  type DirectoryAdmissionLayout,
  type DirectoryAdmissionScope,
  type MaterializationDirectory,
} from '../../src/transfer/directory-admission'
import { FaultDomain, FaultScope, OutputFaultCode, outputFault } from '../../src/transfer/fault'
import {
  createCatalogRootDirectoryTreeArtifact,
  createCompleteDirectoryResultRoot,
  createArtifactChoiceIdentity,
  createDirectorySelectionResultRoot,
  createDirectAtomicPlan,
  createDirectTreePlan,
  createFSANamedEntryReservation,
  createManagedAtomicReservation,
  createNativeContainerRootReservation,
  createNativeNamedEntryReservation,
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
  deriveArtifactChoiceIdentity,
  type ArtifactKind,
  type ArtifactSpec,
  type CanonicalDigestValue,
  type DestinationReservation,
  type MaterializationPlan,
  type MaterializationKind,
  type GuaranteeProfile,
  type PreparationPolicy,
  type ReceiveIntent,
  type ResultRootLayout,
  type SelectionRulesSpec,
} from '../../src/transfer/intent'
import { b64ToBytes, loadVectorFile, type VectorCase } from '../vectors'

const receiveIntentVectors = loadVectorFile(
  new URL('../../../core/testvectors/receive-intent-v3.json', import.meta.url),
)
const artifactChoiceVectors = loadVectorFile(
  new URL('../../../core/testvectors/artifact-choice-v1.json', import.meta.url),
)
const admissionVectors = loadVectorFile(
  new URL('../../../core/testvectors/directory-admission-v2.json', import.meta.url),
)
const checkpointVectors = loadVectorFile(
  new URL('../../../core/testvectors/file-checkpoint-v2.json', import.meta.url),
)

describe('Go↔TypeScript ArtifactChoiceIdentityV1 vectors', () => {
  for (const vector of artifactChoiceVectors.cases) {
    it(`replays ${vector.name}`, async () => {
      const input = requiredRecord(vector.input, 'artifact choice input')
      const identity = await createArtifactChoiceIdentity({
        artifactKind: artifactKind(requiredString(input.artifactKind, 'artifact kind')),
        materializationKind: materializationKind(requiredString(
          input.materializationKind,
          'materialization kind',
        )),
        guaranteeProfile: guaranteeProfile(requiredString(input.guaranteeProfile, 'guarantee profile')),
        preparation: preparationPolicy(requiredString(input.preparation, 'preparation policy')),
      })
      const expected = requiredRecord(vector.expected, 'artifact choice expected values')
      expect(encodeBase64Url(identity.canonicalBytes)).toBe(requiredString(
        expected.canonicalBytesB64Url,
        'artifact choice canonical bytes',
      ))
      expect(identity.id).toBe(requiredString(expected.artifactChoiceId, 'artifact choice ID'))
      if (identity.materializationKind === 'direct-resumable-zip') {
        expect(vector.routeSupport).toBe('available-exact-reviewed-platform-only')
        const policy = requiredRecord(vector.policyAvailability, 'direct ZIP policy availability')
        expect(policy).toMatchObject({
          directZipEpochPolicyDigest: 'dVc_DFPK_50xrZ7_GK0oQ9noWgHhb-2eZEnl4-0kUOo',
          zipRouteRecommendationPolicyDigest: 'zHRGRc5-OvZ4Z8U2E1ORwNWnccnf_p35QB8iSXlixqI',
          reason: 'exact-reviewed-runtime-required',
        })
      }
    })
  }
})

describe('Go↔TypeScript ReceiveIntentV3 vectors', () => {
  for (const vector of receiveIntentVectors.cases) {
    it(`replays ${vector.name}`, async () => {
      await replayReceiveIntent(vector)
    })
  }
})

describe('Go↔TypeScript DirectoryAdmissionV2 vectors', () => {
  for (const vector of admissionVectors.cases) {
    it(`replays ${vector.name}`, async () => {
      const intentVector = receiveIntentCase(requiredString(vector.receiveIntentCase, 'receive intent case'))
      const intent = await replayReceiveIntent(intentVector)
      const { admission, directory, scope, secret } = await buildDirectoryAdmission(
        vector,
        admissionVectors.cases,
        intent,
      )

      expect(scope.receiveIntentDigest).toBe(intent.digest)
      expect(scope.syntheticRoot).toBe(intent.syntheticRoot)
      expect(vector.schemaVersion).toBe(2)
      expect(encodeBase64Url(canonicalDirectoryAdmissionMessageV2(scope, directory)))
        .toBe(requiredString(vector.messageB64Url, 'directory admission message'))
      expect(await deriveDirectoryAdmissionToken(secret, scope, directory))
        .toBe(requiredString(vector.token, 'directory admission token'))
      expect(await verifyDirectoryAdmissionToken(secret, scope, directory, admission.token)).toBe(true)
      expect(admission.token).toBe(requiredString(vector.token, 'directory admission token'))
      replayDirectorySettlement(vector, admission)
    })
  }
})

describe('Go↔TypeScript FileCheckpointV2 vectors', () => {
  it('replays canonical bindings and storage envelopes', async () => {
    const names = [
      'candidate',
      'verified',
      'paused',
      'next-candidate',
      'next-verified',
      'foreign-authority',
    ] as const
    const records = new Map<string, FileCheckpointV2>()
    for (const name of names) {
      const vector = checkpointCase(name)
      const record = checkpointRecord(vector)
      const intent = await replayReceiveIntent(receiveIntentCase(
        requiredString(vector.receiveIntentCase, 'receive intent case'),
      ))

      expect(record.schemaVersion).toBe(requiredNumber(vector.schemaVersion, 'checkpoint schema version'))
      expect(record.ownershipMarker).toBe(FILE_CHECKPOINT_OWNERSHIP_MARKER)
      expect(record.namespace).toBe(FILE_CHECKPOINT_NAMESPACE)
      expect(record.operationId).toBe(intent.operationId)
      expect(record.receiveIntentDigest).toBe(intent.digest)
      expect(record.materializationBindingDigest).toBe(intent.bindingDigest)
      expect(record.recordId).toBe(requiredString(vector.recordId, 'checkpoint record ID'))
      expect(deriveCheckpointLineageID(record))
        .toBe(requiredString(vector.checkpointLineageId, 'checkpoint lineage ID'))
      expect(encodeBase64Url(canonicalCheckpointLineageBytes(record))).toBe(requiredString(
        vector.checkpointLineageCanonicalBytesB64Url,
        'checkpoint lineage canonical bytes',
      ))
      expect(record.checksum).toBe(requiredString(vector.checksum, 'checkpoint checksum'))
      expect(encodeBase64Url(canonicalFileCheckpointBytes(record)))
        .toBe(requiredString(vector.canonicalBytesB64Url, 'checkpoint canonical bytes'))
      const encoded = encodeFileCheckpointV2(record)
      expect(encodeBase64Url(encoded)).toBe(requiredString(vector.encodedB64Url, 'checkpoint envelope'))
      expect(decodeFileCheckpointV2(encoded)).toEqual(record)
      records.set(name, record)
    }

    const candidate = records.get('candidate')!
    const verified = records.get('verified')!
    const paused = records.get('paused')!
    const nextCandidate = records.get('next-candidate')!
    const nextVerified = records.get('next-verified')!
    const foreignAuthority = records.get('foreign-authority')!
    expect(() => validateFileCheckpointTransition(candidate, verified)).not.toThrow()
    expect(() => validateFileCheckpointTransition(verified, paused)).not.toThrow()
    expect(() => validateFileCheckpointTransition(verified, nextCandidate)).not.toThrow()
    expect(() => validateFileCheckpointTransition(nextCandidate, nextVerified)).not.toThrow()
    expect(checkpointIdentityEqual(candidate, foreignAuthority)).toBe(false)
    expect(deriveCheckpointLineageID(verified)).toBe(deriveCheckpointLineageID(candidate))
    expect(deriveCheckpointLineageID(paused)).toBe(deriveCheckpointLineageID(candidate))
    expect(deriveCheckpointLineageID(nextCandidate)).toBe(deriveCheckpointLineageID(candidate))
    expect(deriveCheckpointLineageID(nextVerified)).toBe(deriveCheckpointLineageID(candidate))
    expect(deriveCheckpointLineageID(foreignAuthority)).not.toBe(deriveCheckpointLineageID(candidate))
    expect([verified, paused, nextCandidate, nextVerified].map((record) => record.recordId))
      .toEqual([candidate.recordId, candidate.recordId, candidate.recordId, candidate.recordId])

    const crashCuts = checkpointCase('crash-cuts')
    const beforeCommit = selectVerifiedCheckpoint(candidate, verified, nextCandidate)
    expect(beforeCommit.recordId).toBe(requiredString(crashCuts.beforeCommitRecordId, 'before-commit record'))
    expect(beforeCommit.checkpointGeneration).toBe(BigInt(requiredString(
      crashCuts.beforeCommitCheckpointGeneration,
      'before-commit generation',
    )))
    const afterCommit = selectVerifiedCheckpoint(candidate, verified, nextCandidate, nextVerified)
    expect(afterCommit.recordId).toBe(requiredString(crashCuts.afterCommitRecordId, 'after-commit record'))
    expect(afterCommit.checkpointGeneration).toBe(BigInt(requiredString(
      crashCuts.afterCommitCheckpointGeneration,
      'after-commit generation',
    )))
  })

  it('replays every included and excluded lineage axis without redefining RecordID', () => {
    const baselineVector = checkpointCase('candidate')
    const baseline = checkpointRecord(baselineVector)
    const baselineLineage = deriveCheckpointLineageID(baseline)
    const names = [
      'lineage-excludes-revision',
      'lineage-excludes-size',
      'lineage-excludes-owned-object',
      'lineage-operation',
      'lineage-intent',
      'lineage-binding',
      'lineage-file',
      'lineage-path-segments-a',
      'lineage-path-segments-b',
      'lineage-path-unicode',
      'lineage-materializer-fsa',
      'lineage-materializer-origin-private',
      'lineage-materializer-atomic',
    ] as const
    const records = new Map<string, FileCheckpointV2>()

    for (const name of names) {
      const vector = checkpointCase(name)
      const record = checkpointRecord(vector)
      const lineageId = deriveCheckpointLineageID(record)
      const relation = requiredString(vector.lineageRelation, 'checkpoint lineage relation')
      expect(lineageId).toBe(requiredString(vector.checkpointLineageId, 'checkpoint lineage ID'))
      expect(encodeBase64Url(canonicalCheckpointLineageBytes(record))).toBe(requiredString(
        vector.checkpointLineageCanonicalBytesB64Url,
        'checkpoint lineage canonical bytes',
      ))
      expect(relation === 'same' ? lineageId === baselineLineage : lineageId !== baselineLineage)
        .toBe(true)
      expect(record.recordId).toBe(requiredString(vector.recordId, 'checkpoint record ID'))
      expect(record.checksum).toBe(requiredString(vector.checksum, 'checkpoint checksum'))
      expect(encodeBase64Url(canonicalFileCheckpointBytes(record)))
        .toBe(requiredString(vector.canonicalBytesB64Url, 'checkpoint canonical bytes'))
      const encoded = encodeFileCheckpointV2(record)
      expect(encodeBase64Url(encoded)).toBe(requiredString(vector.encodedB64Url, 'checkpoint envelope'))
      expect(decodeFileCheckpointV2(encoded)).toEqual(record)
      records.set(name, record)
    }

    for (const name of [
      'lineage-excludes-revision',
      'lineage-excludes-size',
      'lineage-excludes-owned-object',
    ] as const) {
      expect(deriveCheckpointLineageID(records.get(name)!)).toBe(baselineLineage)
      expect(records.get(name)!.recordId).not.toBe(baseline.recordId)
    }
    expect(deriveCheckpointLineageID(records.get('lineage-path-segments-a')!))
      .not.toBe(deriveCheckpointLineageID(records.get('lineage-path-segments-b')!))
    expect([
      baseline.materializerKind,
      records.get('lineage-materializer-fsa')!.materializerKind,
      records.get('lineage-materializer-origin-private')!.materializerKind,
      records.get('lineage-materializer-atomic')!.materializerKind,
    ]).toEqual([1, 2, 3, 4])
  })
})

async function replayReceiveIntent(vector: VectorCase): Promise<ReceiveIntent> {
  const selectionInput = requiredRecord(vector.selection, 'selection')
  const rulesInput = requiredRecord(selectionInput.rules, 'selection rules')
  const rules = selectionRules(rulesInput)
  const selection = await createSelectionSpec({
    shareInstance: requiredString(selectionInput.shareInstance, 'share instance'),
    syntheticRoot: requiredString(selectionInput.syntheticRoot, 'synthetic root'),
    rules,
  })
  const artifact = await artifactSpec(requiredRecord(vector.artifact, 'artifact'))
  const { plan, binding } = await materializationPlan(
    requiredRecord(vector.plan, 'materialization plan'),
    artifact,
  )
  const intent = await createReceiveIntent({ selection, artifact, plan })
  const expected = requiredRecord(vector.expected, 'expected ReceiveIntent values')

  expect(encodeBase64Url(selection.canonicalBytes))
    .toBe(requiredString(expected.selectionCanonicalBytesB64Url, 'selection canonical bytes'))
  expect(selection.digest).toBe(requiredString(expected.selectionDigest, 'selection digest'))
  expect(encodeBase64Url(artifact.canonicalBytes))
    .toBe(requiredString(expected.artifactCanonicalBytesB64Url, 'artifact canonical bytes'))
  expect(artifact.digest).toBe(requiredString(expected.artifactDigest, 'artifact digest'))
  expect(encodeBase64Url(binding.canonicalBytes))
    .toBe(requiredString(expected.bindingCanonicalBytesB64Url, 'binding canonical bytes'))
  expect(binding.digest).toBe(requiredString(expected.bindingDigest, 'binding digest'))
  const choice = await deriveArtifactChoiceIdentity(artifact, plan)
  expect(encodeBase64Url(choice.canonicalBytes)).toBe(requiredString(
    expected.artifactChoiceCanonicalBytesB64Url,
    'artifact choice canonical bytes',
  ))
  expect(choice.id).toBe(requiredString(expected.artifactChoiceId, 'artifact choice ID'))
  expect(encodeBase64Url(plan.canonicalBytes))
    .toBe(requiredString(expected.planCanonicalBytesB64Url, 'materialization plan canonical bytes'))
  expect(intent.operationId).toBe(requiredString(expected.operationId, 'operation ID'))
  expect(encodeBase64Url(intent.canonicalBytes))
    .toBe(requiredString(expected.receiveIntentCanonicalBytesB64Url, 'ReceiveIntent canonical bytes'))
  expect(intent.digest).toBe(requiredString(expected.receiveIntentDigest, 'ReceiveIntent digest'))
  return intent
}

function selectionRules(input: Record<string, unknown>): SelectionRulesSpec {
  const mode = requiredString(input.mode, 'selection mode')
  if (mode === 'catalog-path') {
    if (input.defaultSelected !== false) throw new Error('catalog-path selection must default to false')
    return {
      mode,
      defaultSelected: false,
      paths: requiredStringArray(input.paths, 'selection paths'),
    }
  }
  if (mode !== 'node-id') throw new Error('selection mode is invalid')
  if (typeof input.defaultSelected !== 'boolean') throw new Error('selection default is not boolean')
  if (!Array.isArray(input.rules)) throw new Error('node selection rules are not an array')
  return {
    mode,
    defaultSelected: input.defaultSelected,
    rules: input.rules.map((value) => {
      const rule = requiredRecord(value, 'node selection rule')
      const kind = requiredString(rule.kind, 'selection rule kind')
      if (kind !== 'directory' && kind !== 'file') throw new Error('selection rule kind is invalid')
      if (typeof rule.selected !== 'boolean') throw new Error('selection rule decision is not boolean')
      return { kind, id: requiredString(rule.id, 'selection rule identity'), selected: rule.selected }
    }),
  }
}

async function artifactSpec(input: Record<string, unknown>): Promise<ArtifactSpec> {
  switch (requiredString(input.kind, 'artifact kind')) {
    case 'original-file':
      return createOriginalFileArtifact({
        fileId: requiredString(input.fileId, 'artifact file ID'),
        sourcePath: requiredString(input.sourcePath, 'artifact source path'),
        suggestedName: requiredString(input.suggestedName, 'artifact suggested name'),
      })
    case 'directory-tree': {
      const layout = requiredRecord(input.layout, 'directory-tree layout')
      switch (requiredString(layout.kind, 'directory-tree layout kind')) {
        case 'single-file':
          return createSingleFileDirectoryTreeArtifact({
            fileId: requiredString(layout.fileId, 'single-file ID'),
            sourcePath: requiredString(layout.sourcePath, 'single-file source path'),
            outputName: requiredString(layout.outputName, 'single-file output name'),
          })
        case 'result-root':
          return createResultRootDirectoryTreeArtifact(resultRoot(requiredRecord(layout.root, 'result root')))
        case 'catalog-root':
          return createCatalogRootDirectoryTreeArtifact()
        default:
          throw new Error('directory-tree layout kind is invalid')
      }
    }
    case 'zip-archive': {
      if (input.encoding !== 'store' || input.completeness !== 'complete-only') {
        throw new Error('ZIP vector does not encode the complete-only store contract')
      }
      const artifact = await createZipArchiveArtifact(resultRoot(requiredRecord(input.layout, 'ZIP layout')))
      expect(artifact.suggestedName).toBe(requiredString(input.suggestedName, 'ZIP suggested name'))
      return artifact
    }
    default:
      throw new Error('artifact kind is invalid')
  }
}

function resultRoot(input: Record<string, unknown>): ResultRootLayout {
  const anchor = requiredRecord(input.anchor, 'result-root anchor')
  let result: ResultRootLayout
  switch (requiredString(input.class, 'result-root class')) {
    case 'complete-directory':
      result = createCompleteDirectoryResultRoot(
        requiredString(anchor.directoryId, 'result-root directory ID'),
        requiredString(anchor.sourcePath, 'result-root source path'),
      )
      break
    case 'directory-selection':
      result = createDirectorySelectionResultRoot(
        requiredString(anchor.directoryId, 'result-root directory ID'),
        requiredString(anchor.sourcePath, 'result-root source path'),
      )
      break
    case 'synthetic-selection':
      if (anchor.kind !== 'synthetic-root') throw new Error('synthetic result root has the wrong anchor')
      result = createSyntheticSelectionResultRoot()
      break
    default:
      throw new Error('result-root class is invalid')
  }
  expect(result.name).toBe(requiredString(input.name, 'result-root name'))
  return result
}

async function materializationPlan(
  input: Record<string, unknown>,
  artifact: ArtifactSpec,
): Promise<{ plan: MaterializationPlan; binding: CanonicalDigestValue }> {
  switch (requiredString(input.kind, 'materialization plan kind')) {
    case 'direct-tree': {
      const reservation = await destinationReservation(
        requiredRecord(input.reservation, 'destination reservation'),
        artifact,
      )
      return { plan: await createDirectTreePlan(artifact, reservation), binding: reservation }
    }
    case 'direct-atomic': {
      const reservation = await destinationReservation(
        requiredRecord(input.reservation, 'destination reservation'),
        artifact,
      )
      if (reservation.kind !== 'atomic-target') throw new Error('direct-atomic vector lacks an atomic target')
      return { plan: await createDirectAtomicPlan(artifact, reservation), binding: reservation }
    }
    case 'workspace-then-publish': {
      const workspaceInput = requiredRecord(input.workspace, 'workspace binding')
      const workspace = await createWorkspaceBinding({
        operationId: requiredString(workspaceInput.operationId, 'workspace operation ID'),
        workspaceId: requiredString(workspaceInput.workspaceId, 'workspace ID'),
        artifact,
        repositoryRef: requiredString(workspaceInput.repositoryRef, 'workspace repository'),
      })
      const publicationGuarantee = requiredString(input.publicationGuarantee, 'publication guarantee')
      if (publicationGuarantee !== 'managed-atomic' && publicationGuarantee !== 'browser-handoff') {
        throw new Error('workspace publication guarantee is invalid')
      }
      return {
        plan: await createWorkspaceThenPublishPlan(artifact, workspace, publicationGuarantee),
        binding: workspace,
      }
    }
    case 'portable-handoff': {
      const portableInput = requiredRecord(input.portable, 'portable binding')
      const portable = await createPortableBinding({
        operationId: requiredString(portableInput.operationId, 'portable operation ID'),
        portablePlanId: requiredString(portableInput.portablePlanId, 'portable plan ID'),
        artifact,
      })
      return { plan: await createPortableHandoffPlan(artifact, portable), binding: portable }
    }
    default:
      throw new Error('materialization plan kind is invalid')
  }
}

function artifactKind(value: string): ArtifactKind {
  if (value !== 'original-file' && value !== 'directory-tree' && value !== 'zip-archive') {
    throw new Error('artifact kind is invalid')
  }
  return value
}

function materializationKind(value: string): MaterializationKind {
  if (value !== 'direct-tree' && value !== 'direct-atomic' &&
      value !== 'workspace-then-publish' && value !== 'portable-handoff' &&
      value !== 'direct-resumable-zip') {
    throw new Error('materialization kind is invalid')
  }
  return value
}

function guaranteeProfile(value: string): GuaranteeProfile {
  if (value !== 'native-tree' && value !== 'fsa-tree' && value !== 'managed-atomic' &&
      value !== 'browser-handoff' && value !== 'fsa-owned-file') {
    throw new Error('guarantee profile is invalid')
  }
  return value
}

function preparationPolicy(value: string): PreparationPolicy {
  if (value !== 'none' && value !== 'exact-zip' && value !== 'exact-artifact') {
    throw new Error('preparation policy is invalid')
  }
  return value
}

async function destinationReservation(
  input: Record<string, unknown>,
  artifact: ArtifactSpec,
): Promise<DestinationReservation> {
  const base = {
    operationId: requiredString(input.operationId, 'reservation operation ID'),
    reservationId: requiredString(input.reservationId, 'reservation ID'),
    artifact,
    authorityRef: requiredString(input.authorityRef, 'reservation authority'),
  }
  switch (requiredString(input.kind, 'reservation kind')) {
    case 'container-root':
      return createNativeContainerRootReservation(base)
    case 'named-container-entry': {
      const named = {
        ...base,
        logicalReservedName: requiredString(input.logicalReservedName, 'logical reserved name'),
        physicalName: requiredString(input.physicalName, 'physical name'),
        collisionIndex: requiredNumber(input.collisionIndex, 'collision index'),
      }
      switch (requiredString(input.authorityKind, 'reservation authority kind')) {
        case 'native-container': return createNativeNamedEntryReservation({
          operationId: named.operationId,
          reservationId: named.reservationId,
          artifact: named.artifact,
          authorityRef: named.authorityRef,
          logicalReservedName: named.logicalReservedName,
          collisionIndex: named.collisionIndex,
        })
        case 'fsa-container': return createFSANamedEntryReservation(named)
        default: throw new Error('named reservation authority kind is invalid')
      }
    }
    case 'atomic-target': {
      const guarantees = requiredRecord(input.guarantees, 'atomic guarantees')
      const nameAuthority = requiredString(guarantees.nameAuthority, 'atomic name authority')
      if (nameAuthority !== 'application-chosen' && nameAuthority !== 'user-chosen') {
        throw new Error('atomic name authority is invalid')
      }
      return createManagedAtomicReservation({
        ...base,
        nameAuthority,
        requestedName: requiredString(input.requestedName, 'requested name'),
        reservedName: requiredString(input.reservedName, 'reserved name'),
        collisionIndex: requiredNumber(input.collisionIndex, 'collision index'),
      })
    }
    default:
      throw new Error('reservation kind is invalid')
  }
}

async function buildDirectoryAdmission(
  vector: VectorCase,
  allCases: readonly VectorCase[],
  intent: ReceiveIntent,
): Promise<{
  admission: DirectoryAdmission
  directory: MaterializationDirectory
  scope: DirectoryAdmissionScope
  secret: Uint8Array
}> {
  const scopeInput = requiredRecord(vector.scope, 'directory admission scope')
  const scope = await createDirectoryAdmissionScope(intent)
  expect(scope).toEqual({
    receiveIntentDigest: requiredString(scopeInput.receiveIntentDigest, 'scope ReceiveIntent digest'),
    layoutVersion: requiredLayoutVersion(scopeInput.layoutVersion),
    layout: directoryAdmissionLayout(scopeInput.layout),
    syntheticRoot: requiredString(scopeInput.syntheticRoot, 'scope synthetic root'),
  } satisfies DirectoryAdmissionScope)
  const parentName = vector.parentCase
  const parentAdmission = parentName === null || parentName === undefined
    ? undefined
    : (await buildDirectoryAdmission(
        requiredCase(allCases, requiredString(parentName, 'parent case')),
        allCases,
        intent,
      )).admission
  const directoryInput = requiredRecord(vector.directory, 'materialization directory')
  const modifiedTime = canonicalModifiedTime(directoryInput.modifiedTime)
  const directory: MaterializationDirectory = {
    directoryId: requiredString(directoryInput.directoryId, 'directory ID'),
    generation: requiredString(directoryInput.generation, 'directory generation'),
    path: requiredStringArray(directoryInput.path, 'directory path'),
    ...(parentAdmission === undefined ? {} : { parentAdmission }),
    ...(modifiedTime === undefined ? {} : { modifiedTime }),
  }
  const secret = b64ToBytes(requiredString(vector.secretB64Url, 'directory admission secret'))
  return {
    admission: await createDirectoryAdmission(secret, scope, directory),
    directory,
    scope,
    secret,
  }
}

function replayDirectorySettlement(vector: VectorCase, admission: DirectoryAdmission): void {
  const expected = requiredRecord(vector.settlement, 'directory settlement')
  let settlement
  switch (requiredString(expected.kind, 'directory settlement kind')) {
    case DirectorySettlementKind.Finalized:
      settlement = finalizedDirectorySettlement(admission)
      break
    case DirectorySettlementKind.IsolatedFailure: {
      const fault = requiredRecord(expected.fault, 'directory settlement fault')
      expect(fault).toEqual({
        domain: FaultDomain.Output,
        scope: FaultScope.DirectoryLocal,
        code: OutputFaultCode.DirectoryMetadata,
      })
      settlement = isolatedDirectorySettlement(
        admission,
        outputFault(FaultScope.DirectoryLocal, OutputFaultCode.DirectoryMetadata),
      )
      break
    }
    default:
      throw new Error('directory settlement kind is invalid')
  }
  const validated = validateDirectorySettlement(admission, settlement)
  expect(validated.kind).toBe(expected.kind)
  expect(validated.admission.token).toBe(requiredString(expected.admissionToken, 'settlement admission token'))
  expect(sameDirectoryAdmission(validated.admission, admission)).toBe(true)
}

function checkpointRecord(vector: VectorCase): FileCheckpointV2 {
  return newFileCheckpointV2({
    ownershipMarker: requiredString(vector.ownershipMarker, 'checkpoint ownership marker'),
    namespace: requiredString(vector.namespace, 'checkpoint namespace'),
    operationId: requiredString(vector.operationId, 'checkpoint operation ID'),
    receiveIntentDigest: requiredString(vector.receiveIntentDigest, 'checkpoint ReceiveIntent digest'),
    materializationBindingDigest: requiredString(
      vector.materializationBindingDigest,
      'checkpoint materialization binding digest',
    ),
    fileId: requiredString(vector.fileId, 'checkpoint file ID'),
    fileRevision: requiredString(vector.fileRevision, 'checkpoint file revision'),
    canonicalPath: requiredStringArray(vector.canonicalPath, 'checkpoint path'),
    exactSize: BigInt(requiredString(vector.exactSize, 'checkpoint exact size')),
    materializerKind: requiredNumber(vector.materializerKind, 'checkpoint materializer kind'),
    authorityRef: requiredString(vector.authorityRef, 'checkpoint authority reference'),
    ownedObjectId: requiredString(vector.ownedObjectId, 'checkpoint owned object ID'),
    stateGeneration: BigInt(requiredString(vector.stateGeneration, 'checkpoint state generation')),
    checkpointGeneration: BigInt(requiredString(
      vector.checkpointGeneration,
      'checkpoint generation',
    )),
    verifiedRanges: requiredArray(vector.verifiedRanges, 'checkpoint verified ranges').map((value) => {
      const range = requiredRecord(value, 'checkpoint verified range')
      return {
        start: BigInt(requiredString(range.start, 'checkpoint range start')),
        end: BigInt(requiredString(range.end, 'checkpoint range end')),
      }
    }),
    phase: requiredNumber(vector.phase, 'checkpoint phase'),
    commitState: requiredNumber(vector.commitState, 'checkpoint commit state'),
    quarantineReason: requiredNumber(vector.quarantineReason, 'checkpoint quarantine reason'),
    quarantineOrigin: requiredNumber(vector.quarantineOrigin, 'checkpoint quarantine origin'),
    retirementReason: requiredNumber(vector.retirementReason, 'checkpoint retirement reason'),
  })
}

function canonicalModifiedTime(value: unknown): CanonicalModifiedTime | undefined {
  if (value === null || value === undefined) return undefined
  const input = requiredRecord(value, 'modified time')
  const precision = requiredNumber(input.precision, 'modified time precision')
  if (precision !== 1 && precision !== 2 && precision !== 3) {
    throw new Error('modified time precision is invalid')
  }
  return {
    seconds: BigInt(requiredString(input.seconds, 'modified time seconds')),
    nanoseconds: requiredNumber(input.nanoseconds, 'modified time nanoseconds'),
    precision,
  }
}

function requiredLayoutVersion(value: unknown): 1 {
  if (value !== 1) throw new Error('directory admission layout version is invalid')
  return value
}

function directoryAdmissionLayout(value: unknown): DirectoryAdmissionLayout {
  switch (value) {
    case 'directory-tree-single-file':
    case 'directory-tree-result-root':
    case 'directory-tree-catalog-root':
    case 'zip-result-root':
      return value
    default:
      throw new Error('directory admission layout is invalid')
  }
}

function receiveIntentCase(name: string): VectorCase {
  return requiredCase(receiveIntentVectors.cases, name)
}

function checkpointCase(name: string): VectorCase {
  return requiredCase(checkpointVectors.cases, name)
}

function requiredCase(cases: readonly VectorCase[], name: string): VectorCase {
  const vector = cases.find((candidate) => candidate.name === name)
  if (vector === undefined) throw new Error(`missing vector case ${name}`)
  return vector
}

function requiredRecord(value: unknown, label: string): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error(`${label} is not an object`)
  }
  return value as Record<string, unknown>
}

function requiredArray(value: unknown, label: string): readonly unknown[] {
  if (!Array.isArray(value)) throw new Error(`${label} is not an array`)
  return value
}

function requiredStringArray(value: unknown, label: string): readonly string[] {
  const result = requiredArray(value, label)
  if (result.some((item) => typeof item !== 'string')) throw new Error(`${label} contains non-text`)
  return result as readonly string[]
}

function requiredString(value: unknown, label: string): string {
  if (typeof value !== 'string') throw new Error(`${label} is not text`)
  return value
}

function requiredNumber(value: unknown, label: string): number {
  if (typeof value !== 'number' || !Number.isFinite(value)) throw new Error(`${label} is not a number`)
  return value
}
