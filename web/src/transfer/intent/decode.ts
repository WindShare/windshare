import { V2_CATALOG_PATH_BYTES } from '../../catalog/path-policy'
import { encodeBase64Url, equalBytes } from '../../crypto/bytes'
import {
  ARTIFACT_SPEC_DOMAIN,
  CanonicalDecoder,
  DESTINATION_RESERVATION_DOMAIN,
  INVALID_RECEIVE_INTENT_CANONICAL_BYTES,
  MATERIALIZATION_PLAN_DOMAIN,
  MAX_CANONICAL_PATH_ENCODING_BYTES,
  PORTABLE_BINDING_DOMAIN,
  RECEIVE_INTENT_DOMAIN,
  RESULT_ROOT_LAYOUT_DOMAIN,
  SELECTION_SPEC_DOMAIN,
  TEXT_DECODER,
  WORKSPACE_BINDING_DOMAIN,
  decodeCanonicalPath,
  decodedCount,
  invalidDecodedCanonicalBytes,
  requireDecodedCanonicalBytes,
} from './canonical'
import {
  browserHandoffGuarantees,
  canonicalGuarantees,
  createFSANamedEntryReservation,
  createManagedAtomicReservation,
  createNativeContainerRootReservation,
  createNativeNamedEntryReservation,
  createPortableBinding,
  createWorkspaceBinding,
  fsaTreeGuarantees,
  managedAtomicGuarantees,
  managedNameAuthority,
  nativeTreeGuarantees,
  sameGuarantees,
} from './destination'
import {
  AUTHORITY_REFERENCE_BYTES,
  BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
  DEFAULT_PORTABLE_ARTIFACT_LIMIT,
  DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES,
  DEFAULT_PORTABLE_MAXIMUM_PARTS,
  MAX_RESULT_COMPONENT_BYTES,
  MAX_SELECTION_RULES,
  MAX_SELECTION_TARGET_UTF8_BYTES,
  STABLE_IDENTITY_BYTES,
  type ArtifactSpec,
  type AtomicTargetReservation,
  type CanonicalBytes,
  type ContainerRootReservation,
  type DestinationReservation,
  type GuaranteeSet,
  type MaterializationPlan,
  type NamedContainerEntryReservation,
  type NodeIDSelectionRules,
  type NodeSelectionRule,
  type PortableBinding,
  type ReceiveIntent,
  type ResultRootLayout,
  type SelectionRulesSpec,
  type SelectionSpec,
  type WorkspaceBinding,
} from './model'
import {
  createDirectAtomicPlan,
  createDirectTreePlan,
  createPortableHandoffPlan,
  createReceiveIntent,
  createWorkspaceThenPublishPlan,
} from './plan'
import {
  createCatalogRootDirectoryTreeArtifact,
  createCompleteDirectoryResultRoot,
  createDirectorySelectionResultRoot,
  createOriginalFileArtifact,
  createResultRootDirectoryTreeArtifact,
  createSelectionSpec,
  createSingleFileDirectoryTreeArtifact,
  createSyntheticSelectionResultRoot,
  createZipArchiveArtifact,
} from './selection'

// Persistence must rebuild authority through the same constructors as a new
// operation; accepting a merely parseable image could silently normalize a
// different selection, binding, or guarantee profile on reopen.
export async function decodeReceiveIntent(canonicalBytes: Uint8Array): Promise<ReceiveIntent> {
  if (!(canonicalBytes instanceof Uint8Array)) {
    throw new TypeError(INVALID_RECEIVE_INTENT_CANONICAL_BYTES)
  }
  const encoded = Uint8Array.from(canonicalBytes)
  try {
    const cursor = CanonicalDecoder.record(encoded, RECEIVE_INTENT_DOMAIN)
    const selectionBytes = cursor.readFrame(cursor.remaining)
    const artifactBytes = cursor.readFrame(cursor.remaining)
    const planBytes = cursor.readFrame(cursor.remaining)
    cursor.requireDone()

    const selection = await decodeSelectionSpecBytes(selectionBytes)
    const artifact = await decodeArtifactSpecBytes(artifactBytes)
    const plan = await decodeMaterializationPlanBytes(planBytes, artifact)
    const intent = await createReceiveIntent({ selection, artifact, plan })
    requireDecodedCanonicalBytes(encoded, intent.canonicalBytes, 'receive intent')
    return intent
  } catch {
    throw new TypeError(INVALID_RECEIVE_INTENT_CANONICAL_BYTES)
  }
}

async function decodeSelectionSpecBytes(encoded: CanonicalBytes): Promise<SelectionSpec> {
  const cursor = CanonicalDecoder.record(encoded, SELECTION_SPEC_DOMAIN)
  const shareInstance = encodeBase64Url(cursor.readFixedFrame(STABLE_IDENTITY_BYTES))
  const syntheticRoot = encodeBase64Url(cursor.readFixedFrame(STABLE_IDENTITY_BYTES))
  const mode = cursor.readFramedByte()
  const defaultSelected = cursor.readFramedBoolean()
  const count = cursor.readRawUint64()
  const rules = decodeSelectionRulesBytes(cursor, mode, defaultSelected, count)
  const selection = await createSelectionSpec({ shareInstance, syntheticRoot, rules })
  cursor.requireDone()
  requireDecodedCanonicalBytes(encoded, selection.canonicalBytes, 'selection spec')
  return selection
}

function decodeSelectionRulesBytes(
  cursor: CanonicalDecoder,
  mode: number,
  defaultSelected: boolean,
  count: bigint,
): SelectionRulesSpec {
  if (mode === 1) return decodeNodeSelectionRulesBytes(cursor, defaultSelected, count)
  if (mode === 2) return decodePathSelectionRulesBytes(cursor, defaultSelected, count)
  return invalidDecodedCanonicalBytes()
}

function decodeNodeSelectionRulesBytes(
  cursor: CanonicalDecoder,
  defaultSelected: boolean,
  count: bigint,
): NodeIDSelectionRules {
  const ruleCount = decodedCount(count, 0, MAX_SELECTION_RULES)
  const rules: NodeSelectionRule[] = []
  for (let index = 0; index < ruleCount; index += 1) {
    const kind = cursor.readFramedByte()
    if (kind !== 1 && kind !== 2) invalidDecodedCanonicalBytes()
    rules.push({
      kind: kind === 1 ? 'directory' : 'file',
      id: encodeBase64Url(cursor.readFixedFrame(STABLE_IDENTITY_BYTES)),
      selected: cursor.readFramedBoolean(),
    })
  }
  return { mode: 'node-id', defaultSelected, rules }
}

function decodePathSelectionRulesBytes(
  cursor: CanonicalDecoder,
  defaultSelected: boolean,
  count: bigint,
): SelectionRulesSpec {
  if (defaultSelected) invalidDecodedCanonicalBytes()
  const pathCount = decodedCount(count, 1, MAX_SELECTION_RULES)
  const paths: string[] = []
  let totalBytes = 0
  for (let index = 0; index < pathCount; index += 1) {
    const pathBytes = cursor.readFrame(V2_CATALOG_PATH_BYTES)
    totalBytes += pathBytes.byteLength
    if (totalBytes > MAX_SELECTION_TARGET_UTF8_BYTES) invalidDecodedCanonicalBytes()
    paths.push(decodeCanonicalText(pathBytes))
  }
  return { mode: 'catalog-path', defaultSelected: false, paths }
}

async function decodeArtifactSpecBytes(encoded: CanonicalBytes): Promise<ArtifactSpec> {
  const cursor = CanonicalDecoder.record(encoded, ARTIFACT_SPEC_DOMAIN)
  const kind = cursor.readRawByte()
  let artifact: ArtifactSpec
  switch (kind) {
    case 1: {
      const fileId = encodeBase64Url(cursor.readFixedFrame(STABLE_IDENTITY_BYTES))
      const sourcePath = decodeCanonicalPath(cursor.readFrame(MAX_CANONICAL_PATH_ENCODING_BYTES))
      const suggestedName = decodeCanonicalText(cursor.readFrame(MAX_RESULT_COMPONENT_BYTES))
      artifact = await createOriginalFileArtifact({ fileId, sourcePath, suggestedName })
      break
    }
    case 2:
      artifact = await decodeDirectoryTreeArtifactBytes(cursor.readFrame(cursor.remaining))
      break
    case 3: {
      const layout = decodeResultRootLayoutBytes(cursor.readFrame(cursor.remaining))
      const suggestedName = decodeCanonicalText(cursor.readFrame(MAX_RESULT_COMPONENT_BYTES))
      const encoding = cursor.readFramedByte()
      const completeness = cursor.readFramedByte()
      if (encoding !== 1 || completeness !== 1) invalidDecodedCanonicalBytes()
      artifact = await createZipArchiveArtifact(layout)
      if (artifact.suggestedName !== suggestedName) invalidDecodedCanonicalBytes()
      break
    }
    default:
      return invalidDecodedCanonicalBytes()
  }
  cursor.requireDone()
  requireDecodedCanonicalBytes(encoded, artifact.canonicalBytes, 'artifact spec')
  return artifact
}

async function decodeDirectoryTreeArtifactBytes(encoded: CanonicalBytes): Promise<ArtifactSpec> {
  const cursor = new CanonicalDecoder(encoded)
  const kind = cursor.readRawByte()
  let artifact: ArtifactSpec
  switch (kind) {
    case 1:
      artifact = await createSingleFileDirectoryTreeArtifact({
        fileId: encodeBase64Url(cursor.readFixedFrame(STABLE_IDENTITY_BYTES)),
        sourcePath: decodeCanonicalPath(cursor.readFrame(MAX_CANONICAL_PATH_ENCODING_BYTES)),
        outputName: decodeCanonicalText(cursor.readFrame(MAX_RESULT_COMPONENT_BYTES)),
      })
      break
    case 2:
      artifact = await createResultRootDirectoryTreeArtifact(
        decodeResultRootLayoutBytes(cursor.readFrame(cursor.remaining)),
      )
      break
    case 3:
      artifact = await createCatalogRootDirectoryTreeArtifact()
      break
    default:
      return invalidDecodedCanonicalBytes()
  }
  cursor.requireDone()
  return artifact
}

function decodeResultRootLayoutBytes(encoded: CanonicalBytes): ResultRootLayout {
  const cursor = CanonicalDecoder.record(encoded, RESULT_ROOT_LAYOUT_DOMAIN)
  const rootClass = cursor.readFramedByte()
  const anchorBytes = cursor.readFrame(cursor.remaining)
  const name = decodeCanonicalText(cursor.readFrame(MAX_RESULT_COMPONENT_BYTES))
  cursor.requireDone()

  const anchor = new CanonicalDecoder(anchorBytes)
  const anchorKind = anchor.readRawByte()
  let layout: ResultRootLayout
  switch (anchorKind) {
    case 1: {
      const directoryId = encodeBase64Url(anchor.readFixedFrame(STABLE_IDENTITY_BYTES))
      const sourcePath = decodeCanonicalPath(anchor.readFrame(MAX_CANONICAL_PATH_ENCODING_BYTES))
      anchor.requireDone()
      if (rootClass === 1) {
        layout = createCompleteDirectoryResultRoot(directoryId, sourcePath)
      } else if (rootClass === 2) {
        layout = createDirectorySelectionResultRoot(directoryId, sourcePath)
      } else {
        return invalidDecodedCanonicalBytes()
      }
      break
    }
    case 2:
      if (rootClass !== 3) return invalidDecodedCanonicalBytes()
      anchor.requireDone()
      layout = createSyntheticSelectionResultRoot()
      break
    default:
      return invalidDecodedCanonicalBytes()
  }
  if (layout.name !== name) invalidDecodedCanonicalBytes()
  requireDecodedCanonicalBytes(encoded, layout.canonicalBytes, 'result-root layout')
  return layout
}

async function decodeDestinationReservationBytes(
  encoded: CanonicalBytes,
  artifact: ArtifactSpec,
): Promise<DestinationReservation> {
  const cursor = CanonicalDecoder.record(encoded, DESTINATION_RESERVATION_DOMAIN)
  const kind = cursor.readRawByte()
  const operationId = encodeBase64Url(cursor.readFixedFrame(STABLE_IDENTITY_BYTES))
  const reservationId = encodeBase64Url(cursor.readFixedFrame(STABLE_IDENTITY_BYTES))
  const artifactDigest = encodeBase64Url(cursor.readFixedFrame(AUTHORITY_REFERENCE_BYTES))
  const authorityKind = cursor.readFramedByte()
  const authorityRef = encodeBase64Url(cursor.readFixedFrame(AUTHORITY_REFERENCE_BYTES))
  const guarantees = decodeGuaranteeSetBytes(cursor.readFrame(cursor.remaining))
  if (artifactDigest !== artifact.digest) invalidDecodedCanonicalBytes()
  const common: DecodedDestinationReservationCommon = {
    operationId,
    reservationId,
    artifact,
    authorityKind,
    authorityRef,
    guarantees,
  }
  let reservation: DestinationReservation
  switch (kind) {
    case 1:
      reservation = await decodeContainerRootReservationBytes(common)
      break
    case 2:
      reservation = await decodeNamedContainerEntryReservationBytes(cursor, common)
      break
    case 3:
      reservation = await decodeAtomicTargetReservationBytes(cursor, common)
      break
    default:
      return invalidDecodedCanonicalBytes()
  }
  cursor.requireDone()
  requireDecodedCanonicalBytes(encoded, reservation.canonicalBytes, 'destination reservation')
  return reservation
}

interface DecodedDestinationReservationCommon {
  readonly operationId: string
  readonly reservationId: string
  readonly artifact: ArtifactSpec
  readonly authorityKind: number
  readonly authorityRef: string
  readonly guarantees: GuaranteeSet
}

async function decodeContainerRootReservationBytes(
  common: DecodedDestinationReservationCommon,
): Promise<ContainerRootReservation> {
  if (common.authorityKind !== 1 ||
      !sameGuarantees(common.guarantees, nativeTreeGuarantees())) {
    return invalidDecodedCanonicalBytes()
  }
  return createNativeContainerRootReservation(common)
}

async function decodeNamedContainerEntryReservationBytes(
  cursor: CanonicalDecoder,
  common: DecodedDestinationReservationCommon,
): Promise<NamedContainerEntryReservation> {
  const entryKind = cursor.readFramedByte()
  if (entryKind !== 1 && entryKind !== 2) invalidDecodedCanonicalBytes()
  const requestedName = decodeCanonicalText(cursor.readFrame(MAX_RESULT_COMPONENT_BYTES))
  const reservedName = decodeCanonicalText(cursor.readFrame(MAX_RESULT_COMPONENT_BYTES))
  const collisionIndex = cursor.readFramedUint32()
  const options = { ...common, reservedName, collisionIndex }
  let reservation: NamedContainerEntryReservation
  if (common.authorityKind === 1 &&
      sameGuarantees(common.guarantees, nativeTreeGuarantees())) {
    reservation = await createNativeNamedEntryReservation(options)
  } else if (common.authorityKind === 2 &&
             sameGuarantees(common.guarantees, fsaTreeGuarantees())) {
    reservation = await createFSANamedEntryReservation(options)
  } else {
    return invalidDecodedCanonicalBytes()
  }
  const expectedEntryKind = entryKind === 1 ? 'single-file' : 'result-root'
  if (reservation.entryKind !== expectedEntryKind || reservation.requestedName !== requestedName) {
    return invalidDecodedCanonicalBytes()
  }
  return reservation
}

async function decodeAtomicTargetReservationBytes(
  cursor: CanonicalDecoder,
  common: DecodedDestinationReservationCommon,
): Promise<AtomicTargetReservation> {
  const requestedName = decodeCanonicalText(cursor.readFrame(MAX_RESULT_COMPONENT_BYTES))
  const reservedName = decodeCanonicalText(cursor.readFrame(MAX_RESULT_COMPONENT_BYTES))
  const collisionIndex = cursor.readFramedUint32()
  if (common.authorityKind !== 3 || common.guarantees.profile !== 'managed-atomic') {
    return invalidDecodedCanonicalBytes()
  }
  return createManagedAtomicReservation({
    ...common,
    nameAuthority: managedNameAuthority(common.guarantees.nameAuthority),
    requestedName,
    reservedName,
    collisionIndex,
  })
}

async function decodeWorkspaceBindingBytes(
  encoded: CanonicalBytes,
  artifact: ArtifactSpec,
): Promise<WorkspaceBinding> {
  const cursor = CanonicalDecoder.record(encoded, WORKSPACE_BINDING_DOMAIN)
  const operationId = encodeBase64Url(cursor.readFixedFrame(STABLE_IDENTITY_BYTES))
  const workspaceId = encodeBase64Url(cursor.readFixedFrame(STABLE_IDENTITY_BYTES))
  const artifactDigest = encodeBase64Url(cursor.readFixedFrame(AUTHORITY_REFERENCE_BYTES))
  const repositoryRef = encodeBase64Url(cursor.readFixedFrame(AUTHORITY_REFERENCE_BYTES))
  const workspaceKind = cursor.readFramedByte()
  const budgetPolicy = cursor.readFramedByte()
  const retentionPolicy = cursor.readFramedByte()
  cursor.requireDone()
  if (artifactDigest !== artifact.digest || workspaceKind !== 1 ||
      budgetPolicy !== 1 || retentionPolicy !== 1) {
    return invalidDecodedCanonicalBytes()
  }
  const binding = await createWorkspaceBinding({
    operationId,
    workspaceId,
    artifact,
    repositoryRef,
  })
  requireDecodedCanonicalBytes(encoded, binding.canonicalBytes, 'workspace binding')
  return binding
}

async function decodePortableBindingBytes(
  encoded: CanonicalBytes,
  artifact: ArtifactSpec,
): Promise<PortableBinding> {
  const cursor = CanonicalDecoder.record(encoded, PORTABLE_BINDING_DOMAIN)
  const operationId = encodeBase64Url(cursor.readFixedFrame(STABLE_IDENTITY_BYTES))
  const portablePlanId = encodeBase64Url(cursor.readFixedFrame(STABLE_IDENTITY_BYTES))
  const artifactDigest = encodeBase64Url(cursor.readFixedFrame(AUTHORITY_REFERENCE_BYTES))
  const maximumArtifactBytes = cursor.readFramedUint64()
  const assemblyPartBytes = cursor.readFramedUint64()
  const maximumParts = cursor.readFramedUint64()
  const objectUrlLeaseMilliseconds = cursor.readFramedUint64()
  const preparation = cursor.readFramedByte()
  cursor.requireDone()
  if (artifactDigest !== artifact.digest ||
      maximumArtifactBytes !== DEFAULT_PORTABLE_ARTIFACT_LIMIT ||
      assemblyPartBytes !== DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES ||
      maximumParts !== DEFAULT_PORTABLE_MAXIMUM_PARTS ||
      objectUrlLeaseMilliseconds !== BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS ||
      preparation !== 2) {
    return invalidDecodedCanonicalBytes()
  }
  const binding = await createPortableBinding({ operationId, portablePlanId, artifact })
  requireDecodedCanonicalBytes(encoded, binding.canonicalBytes, 'portable binding')
  return binding
}

async function decodeMaterializationPlanBytes(
  encoded: CanonicalBytes,
  artifact: ArtifactSpec,
): Promise<MaterializationPlan> {
  const cursor = CanonicalDecoder.record(encoded, MATERIALIZATION_PLAN_DOMAIN)
  const kind = cursor.readRawByte()
  let plan: MaterializationPlan
  switch (kind) {
    case 1: {
      const reservation = await decodeDestinationReservationBytes(
        cursor.readFrame(cursor.remaining),
        artifact,
      )
      if (cursor.readFramedByte() !== 0) invalidDecodedCanonicalBytes()
      plan = await createDirectTreePlan(artifact, reservation)
      break
    }
    case 2: {
      const reservation = await decodeDestinationReservationBytes(
        cursor.readFrame(cursor.remaining),
        artifact,
      )
      if (cursor.readFramedByte() !== 0) invalidDecodedCanonicalBytes()
      plan = await createDirectAtomicPlan(artifact, reservation)
      break
    }
    case 3: {
      const workspace = await decodeWorkspaceBindingBytes(
        cursor.readFrame(cursor.remaining),
        artifact,
      )
      const preparation = cursor.readFramedByte()
      plan = await createWorkspaceThenPublishPlan(artifact, workspace)
      const expectedPreparation = plan.preparation === 'exact-zip' ? 1 : 0
      if (preparation !== expectedPreparation) invalidDecodedCanonicalBytes()
      break
    }
    case 4: {
      const portable = await decodePortableBindingBytes(
        cursor.readFrame(cursor.remaining),
        artifact,
      )
      const publicationRoute = cursor.readFramedByte()
      const preparation = cursor.readFramedByte()
      if (publicationRoute !== 2 || preparation !== 2) invalidDecodedCanonicalBytes()
      plan = await createPortableHandoffPlan(artifact, portable)
      break
    }
    default:
      return invalidDecodedCanonicalBytes()
  }
  cursor.requireDone()
  requireDecodedCanonicalBytes(encoded, plan.canonicalBytes, 'materialization plan')
  return plan
}

function decodeGuaranteeSetBytes(encoded: CanonicalBytes): GuaranteeSet {
  const candidates = [
    nativeTreeGuarantees(),
    fsaTreeGuarantees(),
    managedAtomicGuarantees('application-chosen'),
    managedAtomicGuarantees('user-chosen'),
    browserHandoffGuarantees(),
  ]
  const guarantee = candidates.find((candidate) =>
    equalBytes(encoded, canonicalGuarantees(candidate)))
  return guarantee ?? invalidDecodedCanonicalBytes()
}

function decodeCanonicalText(encoded: CanonicalBytes): string {
  return TEXT_DECODER.decode(encoded)
}
