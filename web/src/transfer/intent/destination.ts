import { sha256 } from '../../crypto/digest'
import {
  DESTINATION_RESERVATION_DOMAIN,
  FSA_OWNED_FILE_BINDING_DOMAIN,
  NAME_COLLISION_DOMAIN,
  PORTABLE_BINDING_DOMAIN,
  TEXT_ENCODER,
  WORKSPACE_BINDING_DOMAIN,
  canonicalDigestValue,
  canonicalRecord,
  digestText,
  frame,
  requireSameDigestRecord,
  requireUint32,
  uint32,
  uint64,
} from './canonical'
import { requireIdentity, requireIdentityBytes } from './identity'
import {
  AUTHORITY_REFERENCE_BYTES,
  BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
  COLLISION_SUFFIX_HEX_CHARS,
  DEFAULT_PORTABLE_ARTIFACT_LIMIT,
  DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES,
  DEFAULT_PORTABLE_MAXIMUM_PARTS,
  DESTINATION_RESERVATION_VERSION,
  DIRECT_ZIP_CANDIDATE_TOKEN_LENGTH,
  DIRECT_ZIP_STABLE_NAME_INFIX,
  FSA_OWNED_FILE_BINDING_VERSION,
  FSA_RESERVED_ROOT_LAYOUT_VERSION,
  MAX_RESULT_COMPONENT_BYTES,
  PORTABLE_BINDING_VERSION,
  STABLE_IDENTITY_BYTES,
  ARCHIVE_EXTENSION,
  WORKSPACE_BINDING_VERSION,
  type ArtifactSpec,
  type AtomicTargetReservation,
  type AvailableDirectZipPolicyDigests,
  type ContainerRootReservation,
  type DestinationReservation,
  type DirectZipPolicyDigests,
  type GuaranteeSet,
  type FSAOwnedFileBinding,
  type FSANamedContainerEntryReservation,
  type NamedContainerEntryReservation,
  type NativeNamedContainerEntryReservation,
  type PortableBinding,
  type WorkspaceBinding,
} from './model'
import {
  completeArtifactName,
  requireResultName,
  validateArtifactSpec,
} from './selection'
import {
  authorityKindByte,
  canonicalGuarantees,
  reservationKindByte,
} from './destination/codec'
import {
  browserHandoffGuarantees,
  fsaOwnedFileGuarantees,
  fsaTreeGuarantees,
  managedAtomicGuarantees,
  managedNameAuthority,
  nativeTreeGuarantees,
  sameGuarantees,
  snapshotGuarantees,
} from './destination/validation'

export {
  authorityKindByte,
  browserHandoffGuarantees,
  canonicalGuarantees,
  fsaOwnedFileGuarantees,
  fsaTreeGuarantees,
  managedAtomicGuarantees,
  managedNameAuthority,
  nativeTreeGuarantees,
  reservationKindByte,
  sameGuarantees,
  snapshotGuarantees,
}

export async function collisionName(
  operationIdInput: string,
  requestedNameInput: string,
  collisionIndex: number,
  fileLike: boolean,
): Promise<string> {
  const operationId = requireIdentity(operationIdInput, STABLE_IDENTITY_BYTES, 'operation')
  const requestedName = requireResultName(requestedNameInput)
  requireUint32(collisionIndex, 'collision index')
  if (typeof fileLike !== 'boolean') throw new TypeError('collision file-like decision must be boolean')
  if (collisionIndex === 0) return requestedName
  const material = canonicalRecord(NAME_COLLISION_DOMAIN, [
    frame(requireIdentityBytes(operationId, STABLE_IDENTITY_BYTES, 'operation')),
    uint32(collisionIndex),
    frame(TEXT_ENCODER.encode(requestedName)),
  ])
  const token = (await sha256(material)).slice(0, COLLISION_SUFFIX_HEX_CHARS / 2)
  const suffix = '-' + [...token].map((byte) => byte.toString(16).padStart(2, '0')).join('')
  let stem = requestedName
  let extension = ''
  if (fileLike) {
    const dot = requestedName.lastIndexOf('.')
    if (dot > 0) {
      stem = requestedName.slice(0, dot)
      extension = requestedName.slice(dot)
    }
  }
  const maximumStemBytes = MAX_RESULT_COMPONENT_BYTES -
    TEXT_ENCODER.encode(suffix).byteLength - TEXT_ENCODER.encode(extension).byteLength
  const scalars = Array.from(stem)
  while (TEXT_ENCODER.encode(scalars.join('')).byteLength > maximumStemBytes) scalars.pop()
  if (scalars.length === 0) throw new TypeError('collision suffix consumed the complete result name')
  return requireResultName(scalars.join('') + suffix + extension)
}

export async function createNativeContainerRootReservation(input: {
  readonly operationId: string
  readonly reservationId: string
  readonly artifact: ArtifactSpec
  readonly authorityRef: string
}): Promise<ContainerRootReservation> {
  const artifact = await validateArtifactSpec(input.artifact)
  if (artifact.kind !== 'directory-tree' || artifact.layout.kind !== 'catalog-root') {
    throw new TypeError('native container-root requires a catalog-root directory tree')
  }
  return createDestinationReservation({
    kind: 'container-root',
    operationId: input.operationId,
    reservationId: input.reservationId,
    artifact,
    authorityKind: 'native-container',
    authorityRef: input.authorityRef,
    guarantees: nativeTreeGuarantees(),
  }) as Promise<ContainerRootReservation>
}

export async function createNativeNamedEntryReservation(input: {
  readonly operationId: string
  readonly reservationId: string
  readonly artifact: ArtifactSpec
  readonly authorityRef: string
  readonly logicalReservedName: string
  readonly collisionIndex: number
}): Promise<NativeNamedContainerEntryReservation> {
  return createNamedEntryReservation({
    ...input,
    authorityKind: 'native-container',
    physicalName: input.logicalReservedName,
  }) as Promise<NativeNamedContainerEntryReservation>
}

export async function createFSANamedEntryReservation(input: {
  readonly operationId: string
  readonly reservationId: string
  readonly artifact: ArtifactSpec
  readonly authorityRef: string
  readonly logicalReservedName: string
  readonly physicalName: string
  readonly collisionIndex: number
}): Promise<FSANamedContainerEntryReservation> {
  return createNamedEntryReservation({
    ...input,
    authorityKind: 'fsa-container',
  }) as Promise<FSANamedContainerEntryReservation>
}

async function createNamedEntryReservation(input: {
  readonly operationId: string
  readonly reservationId: string
  readonly artifact: ArtifactSpec
  readonly authorityRef: string
  readonly authorityKind: 'native-container' | 'fsa-container'
  readonly logicalReservedName: string
  readonly physicalName: string
  readonly collisionIndex: number
}): Promise<NamedContainerEntryReservation> {
  const artifact = await validateArtifactSpec(input.artifact)
  if (artifact.kind !== 'directory-tree' || artifact.layout.kind === 'catalog-root') {
    throw new TypeError('named container entry requires a named directory-tree layout')
  }
  const entryKind = artifact.layout.kind === 'single-file' ? 'single-file' : 'result-root'
  const requestedName = artifact.layout.kind === 'single-file'
    ? artifact.layout.outputName
    : artifact.layout.root.name
  const logicalReservedName = requireResultName(input.logicalReservedName)
  const physicalName = requireResultName(input.physicalName)
  const expected = await collisionName(
    input.operationId,
    requestedName,
    input.collisionIndex,
    entryKind === 'single-file',
  )
  if (logicalReservedName !== expected) {
    throw new TypeError('named-entry collision decision is invalid')
  }
  if (input.authorityKind === 'native-container' && physicalName !== logicalReservedName) {
    throw new TypeError('native named-entry physical name must equal its logical reservation')
  }
  return createDestinationReservation({
    kind: 'named-container-entry',
    operationId: input.operationId,
    reservationId: input.reservationId,
    artifact,
    authorityKind: input.authorityKind,
    authorityRef: input.authorityRef,
    guarantees: input.authorityKind === 'native-container'
      ? nativeTreeGuarantees()
      : fsaTreeGuarantees(),
    entryKind,
    requestedName,
    logicalReservedName,
    physicalName,
    collisionIndex: input.collisionIndex,
  }) as Promise<NamedContainerEntryReservation>
}

export async function createManagedAtomicReservation(input: {
  readonly operationId: string
  readonly reservationId: string
  readonly artifact: ArtifactSpec
  readonly authorityRef: string
  readonly nameAuthority: 'application-chosen' | 'user-chosen'
  readonly requestedName: string
  readonly reservedName: string
  readonly collisionIndex: number
}): Promise<AtomicTargetReservation> {
  const artifact = await validateArtifactSpec(input.artifact)
  const artifactName = completeArtifactName(artifact)
  const requestedName = requireResultName(input.requestedName)
  if (input.nameAuthority === 'application-chosen' && requestedName !== artifactName) {
    throw new TypeError('application-chosen atomic target must use the artifact name')
  }
  const reservedName = requireResultName(input.reservedName)
  const expected = await collisionName(
    input.operationId,
    requestedName,
    input.collisionIndex,
    true,
  )
  if (reservedName !== expected) throw new TypeError('atomic-target collision decision is invalid')
  return createDestinationReservation({
    kind: 'atomic-target',
    operationId: input.operationId,
    reservationId: input.reservationId,
    artifact,
    authorityKind: 'managed-atomic-target',
    authorityRef: input.authorityRef,
    guarantees: managedAtomicGuarantees(input.nameAuthority),
    requestedName,
    reservedName,
    collisionIndex: input.collisionIndex,
  }) as Promise<AtomicTargetReservation>
}

type DestinationReservationInput =
  | Readonly<{
      kind: 'container-root'
      operationId: string
      reservationId: string
      artifact: ArtifactSpec
      authorityKind: 'native-container'
      authorityRef: string
      guarantees: GuaranteeSet
    }>
  | Readonly<{
      kind: 'named-container-entry'
      operationId: string
      reservationId: string
      artifact: ArtifactSpec
      authorityKind: 'native-container' | 'fsa-container'
      authorityRef: string
      guarantees: GuaranteeSet
      entryKind: 'single-file' | 'result-root'
      requestedName: string
      logicalReservedName: string
      physicalName: string
      collisionIndex: number
    }>
  | Readonly<{
      kind: 'atomic-target'
      operationId: string
      reservationId: string
      artifact: ArtifactSpec
      authorityKind: 'managed-atomic-target'
      authorityRef: string
      guarantees: GuaranteeSet
      requestedName: string
      reservedName: string
      collisionIndex: number
    }>

async function createDestinationReservation(
  input: DestinationReservationInput,
): Promise<DestinationReservation> {
  const operationId = requireIdentity(input.operationId, STABLE_IDENTITY_BYTES, 'operation')
  const reservationId = requireIdentity(
    input.reservationId,
    STABLE_IDENTITY_BYTES,
    'destination reservation',
  )
  const authorityRef = requireIdentity(
    input.authorityRef,
    AUTHORITY_REFERENCE_BYTES,
    'destination authority',
  )
  const guarantees = snapshotGuarantees(input.guarantees)
  const fields: Uint8Array[] = [
    Uint8Array.of(reservationKindByte(input.kind)),
    frame(requireIdentityBytes(operationId, STABLE_IDENTITY_BYTES, 'operation')),
    frame(requireIdentityBytes(reservationId, STABLE_IDENTITY_BYTES, 'destination reservation')),
    frame(requireIdentityBytes(input.artifact.digest, AUTHORITY_REFERENCE_BYTES, 'artifact digest')),
    frame(Uint8Array.of(authorityKindByte(input.authorityKind))),
    frame(requireIdentityBytes(authorityRef, AUTHORITY_REFERENCE_BYTES, 'destination authority')),
    frame(canonicalGuarantees(guarantees)),
  ]
  let variant: object
  switch (input.kind) {
    case 'container-root':
      variant = { kind: input.kind }
      break
    case 'named-container-entry':
      requireUint32(input.collisionIndex, 'collision index')
      fields.push(frame(Uint8Array.of(input.entryKind === 'single-file' ? 1 : 2)))
      fields.push(frame(TEXT_ENCODER.encode(input.requestedName)))
      fields.push(frame(TEXT_ENCODER.encode(input.logicalReservedName)))
      fields.push(frame(TEXT_ENCODER.encode(input.physicalName)))
      fields.push(frame(uint32(input.collisionIndex)))
      if (input.authorityKind === 'fsa-container') {
        fields.push(frame(Uint8Array.of(FSA_RESERVED_ROOT_LAYOUT_VERSION)))
      }
      variant = {
        kind: input.kind,
        entryKind: input.entryKind,
        requestedName: input.requestedName,
        logicalReservedName: input.logicalReservedName,
        physicalName: input.physicalName,
        collisionIndex: input.collisionIndex,
        ...(input.authorityKind === 'fsa-container'
          ? { fsaLayoutVersion: FSA_RESERVED_ROOT_LAYOUT_VERSION }
          : {}),
      }
      break
    case 'atomic-target':
      requireUint32(input.collisionIndex, 'collision index')
      fields.push(frame(TEXT_ENCODER.encode(input.requestedName)))
      fields.push(frame(TEXT_ENCODER.encode(input.reservedName)))
      fields.push(frame(uint32(input.collisionIndex)))
      variant = {
        kind: input.kind,
        requestedName: input.requestedName,
        reservedName: input.reservedName,
        collisionIndex: input.collisionIndex,
      }
      break
  }
  const canonicalBytes = canonicalRecord(DESTINATION_RESERVATION_DOMAIN, fields)
  return canonicalDigestValue({
    version: DESTINATION_RESERVATION_VERSION,
    ...variant,
    operationId,
    reservationId,
    artifactDigest: input.artifact.digest,
    authorityKind: input.authorityKind,
    authorityRef,
    guarantees,
  }, await digestText(canonicalBytes), canonicalBytes) as DestinationReservation
}

export async function validateDestinationReservation(
  input: DestinationReservation,
  artifactInput: ArtifactSpec,
): Promise<DestinationReservation> {
  if (input.version !== DESTINATION_RESERVATION_VERSION) {
    throw new TypeError('destination reservation version is invalid')
  }
  const artifact = await validateArtifactSpec(artifactInput)
  if (input.artifactDigest !== artifact.digest) {
    throw new TypeError('destination reservation artifact digest is invalid')
  }
  let rebuilt: DestinationReservation
  switch (input.kind) {
    case 'container-root':
      rebuilt = await createNativeContainerRootReservation({
        operationId: input.operationId,
        reservationId: input.reservationId,
        artifact,
        authorityRef: input.authorityRef,
      })
      break
    case 'named-container-entry': {
      if (input.authorityKind === 'fsa-container') {
        if (input.fsaLayoutVersion !== FSA_RESERVED_ROOT_LAYOUT_VERSION) {
          throw new TypeError('FSA reserved-root layout version is invalid')
        }
      } else if ('fsaLayoutVersion' in input) {
        throw new TypeError('native named-entry reservation cannot carry an FSA layout version')
      }
      const options = {
        operationId: input.operationId,
        reservationId: input.reservationId,
        artifact,
        authorityRef: input.authorityRef,
        logicalReservedName: input.logicalReservedName,
        physicalName: input.physicalName,
        collisionIndex: input.collisionIndex,
      }
      rebuilt = input.authorityKind === 'native-container'
        ? await createNativeNamedEntryReservation({
            operationId: options.operationId,
            reservationId: options.reservationId,
            artifact: options.artifact,
            authorityRef: options.authorityRef,
            logicalReservedName: options.logicalReservedName,
            collisionIndex: options.collisionIndex,
          })
        : await createFSANamedEntryReservation(options)
      if (input.entryKind !== rebuilt.entryKind) {
        throw new TypeError('destination reservation entry kind is invalid')
      }
      break
    }
    case 'atomic-target':
      rebuilt = await createManagedAtomicReservation({
        operationId: input.operationId,
        reservationId: input.reservationId,
        artifact,
        authorityRef: input.authorityRef,
        nameAuthority: managedNameAuthority(input.guarantees.nameAuthority),
        requestedName: input.requestedName,
        reservedName: input.reservedName,
        collisionIndex: input.collisionIndex,
      })
      break
    default:
      throw new TypeError('destination reservation kind is invalid')
  }
  if (!sameGuarantees(input.guarantees, rebuilt.guarantees) ||
      input.authorityKind !== rebuilt.authorityKind) {
    throw new TypeError('destination reservation guarantee profile is invalid')
  }
  return requireSameDigestRecord(input, rebuilt, 'destination reservation')
}

export async function createWorkspaceBinding(input: {
  readonly operationId: string
  readonly workspaceId: string
  readonly artifact: ArtifactSpec
  readonly repositoryRef: string
}): Promise<WorkspaceBinding> {
  const artifact = await validateArtifactSpec(input.artifact)
  if (artifact.kind === 'directory-tree') {
    throw new TypeError('workspace binding requires a complete artifact')
  }
  const operationId = requireIdentity(input.operationId, STABLE_IDENTITY_BYTES, 'operation')
  const workspaceId = requireIdentity(input.workspaceId, STABLE_IDENTITY_BYTES, 'workspace')
  const repositoryRef = requireIdentity(
    input.repositoryRef,
    AUTHORITY_REFERENCE_BYTES,
    'workspace repository',
  )
  const canonicalBytes = canonicalRecord(WORKSPACE_BINDING_DOMAIN, [
    frame(requireIdentityBytes(operationId, STABLE_IDENTITY_BYTES, 'operation')),
    frame(requireIdentityBytes(workspaceId, STABLE_IDENTITY_BYTES, 'workspace')),
    frame(requireIdentityBytes(artifact.digest, AUTHORITY_REFERENCE_BYTES, 'artifact digest')),
    frame(requireIdentityBytes(repositoryRef, AUTHORITY_REFERENCE_BYTES, 'workspace repository')),
    frame(Uint8Array.of(1)),
    frame(Uint8Array.of(1)),
    frame(Uint8Array.of(1)),
  ])
  return canonicalDigestValue({
    version: WORKSPACE_BINDING_VERSION,
    operationId,
    workspaceId,
    artifactDigest: artifact.digest,
    repositoryRef,
    workspaceKind: 'origin-private' as const,
    budgetPolicy: 'workspace-v1' as const,
    retentionPolicy: 'stable-24h-v1' as const,
  }, await digestText(canonicalBytes), canonicalBytes)
}

export async function validateWorkspaceBinding(
  input: WorkspaceBinding,
  artifact: ArtifactSpec,
): Promise<WorkspaceBinding> {
  if (input.version !== WORKSPACE_BINDING_VERSION ||
      input.workspaceKind !== 'origin-private' ||
      input.budgetPolicy !== 'workspace-v1' ||
      input.retentionPolicy !== 'stable-24h-v1') {
    throw new TypeError('workspace binding policy is invalid')
  }
  const rebuilt = await createWorkspaceBinding({
    operationId: input.operationId,
    workspaceId: input.workspaceId,
    artifact,
    repositoryRef: input.repositoryRef,
  })
  if (input.artifactDigest !== rebuilt.artifactDigest) {
    throw new TypeError('workspace binding artifact digest is invalid')
  }
  return requireSameDigestRecord(input, rebuilt, 'workspace binding')
}

export function unavailableDirectZipPolicyDigests(): DirectZipPolicyDigests {
  return Object.freeze({
    zipEncoding: null,
    layout: null,
    checkpoint: null,
    journalBudget: null,
    epoch: null,
  })
}

export async function createFSAOwnedFileBinding(input: {
  readonly operationId: string
  readonly artifact: ArtifactSpec
  readonly stableName: string
  readonly targetRef: string
  readonly policies: DirectZipPolicyDigests
}): Promise<FSAOwnedFileBinding> {
  const artifact = await validateArtifactSpec(input.artifact)
  if (artifact.kind !== 'zip-archive') {
    throw new TypeError('FSA owned-file binding requires a ZIP artifact')
  }
  const operationId = requireIdentity(input.operationId, STABLE_IDENTITY_BYTES, 'operation')
  const stableName = requireDirectZipStableName(input.stableName)
  const targetRef = requireIdentity(input.targetRef, AUTHORITY_REFERENCE_BYTES, 'FSA owned target')
  const policies = requireAvailableDirectZipPolicies(input.policies)
  const guarantees = fsaOwnedFileGuarantees()
  const canonicalBytes = canonicalRecord(FSA_OWNED_FILE_BINDING_DOMAIN, [
    frame(requireIdentityBytes(operationId, STABLE_IDENTITY_BYTES, 'operation')),
    frame(requireIdentityBytes(artifact.digest, AUTHORITY_REFERENCE_BYTES, 'artifact digest')),
    frame(TEXT_ENCODER.encode(stableName)),
    frame(requireIdentityBytes(targetRef, AUTHORITY_REFERENCE_BYTES, 'FSA owned target')),
    frame(canonicalGuarantees(guarantees)),
    frame(requireIdentityBytes(policies.zipEncoding, AUTHORITY_REFERENCE_BYTES, 'ZIP encoding policy')),
    frame(requireIdentityBytes(policies.layout, AUTHORITY_REFERENCE_BYTES, 'direct ZIP layout policy')),
    frame(requireIdentityBytes(policies.checkpoint, AUTHORITY_REFERENCE_BYTES, 'direct ZIP checkpoint policy')),
    frame(requireIdentityBytes(policies.journalBudget, AUTHORITY_REFERENCE_BYTES, 'direct ZIP journal budget')),
    frame(requireIdentityBytes(policies.epoch, AUTHORITY_REFERENCE_BYTES, 'direct ZIP epoch policy')),
  ])
  return canonicalDigestValue({
    version: FSA_OWNED_FILE_BINDING_VERSION,
    operationId,
    artifactDigest: artifact.digest,
    stableName,
    targetRef,
    guarantees,
    policies,
  }, await digestText(canonicalBytes), canonicalBytes)
}

export async function validateFSAOwnedFileBinding(
  input: FSAOwnedFileBinding,
  artifact: ArtifactSpec,
): Promise<FSAOwnedFileBinding> {
  if (input.version !== FSA_OWNED_FILE_BINDING_VERSION ||
      input.artifactDigest !== artifact.digest ||
      !sameGuarantees(input.guarantees, fsaOwnedFileGuarantees())) {
    throw new TypeError('FSA owned-file binding contract is invalid')
  }
  const rebuilt = await createFSAOwnedFileBinding({
    operationId: input.operationId,
    artifact,
    stableName: input.stableName,
    targetRef: input.targetRef,
    policies: input.policies,
  })
  return requireSameDigestRecord(input, rebuilt, 'FSA owned-file binding')
}

function requireAvailableDirectZipPolicies(
  input: DirectZipPolicyDigests,
): AvailableDirectZipPolicyDigests {
  if (input === null || typeof input !== 'object') {
    throw new TypeError('direct ZIP policy digests are absent')
  }
  const available = {
    zipEncoding: requirePolicyDigest(input.zipEncoding, 'ZIP encoding policy'),
    layout: requirePolicyDigest(input.layout, 'direct ZIP layout policy'),
    checkpoint: requirePolicyDigest(input.checkpoint, 'direct ZIP checkpoint policy'),
    journalBudget: requirePolicyDigest(input.journalBudget, 'direct ZIP journal budget'),
    epoch: requirePolicyDigest(input.epoch, 'direct ZIP epoch policy'),
  }
  return Object.freeze(available)
}

function requirePolicyDigest(value: string | null, label: string): string {
  if (value === null) throw new TypeError(label + ' digest is absent')
  return requireIdentity(value, AUTHORITY_REFERENCE_BYTES, label)
}

function requireDirectZipStableName(value: string): string {
  const name = requireResultName(value)
  if (!name.endsWith(ARCHIVE_EXTENSION)) throw new TypeError('direct ZIP stable name extension is invalid')
  const withoutExtension = name.slice(0, -ARCHIVE_EXTENSION.length)
  const separator = withoutExtension.lastIndexOf(DIRECT_ZIP_STABLE_NAME_INFIX)
  const token = withoutExtension.slice(separator + DIRECT_ZIP_STABLE_NAME_INFIX.length)
  if (separator <= 0 || token.length !== DIRECT_ZIP_CANDIDATE_TOKEN_LENGTH || token.includes('=')) {
    throw new TypeError('direct ZIP stable name token is invalid')
  }
  requireIdentity(token, STABLE_IDENTITY_BYTES, 'direct ZIP reservation candidate')
  return name
}

export async function createPortableBinding(input: {
  readonly operationId: string
  readonly portablePlanId: string
  readonly artifact: ArtifactSpec
}): Promise<PortableBinding> {
  const artifact = await validateArtifactSpec(input.artifact)
  if (artifact.kind === 'directory-tree') {
    throw new TypeError('portable binding requires a complete artifact')
  }
  const operationId = requireIdentity(input.operationId, STABLE_IDENTITY_BYTES, 'operation')
  const portablePlanId = requireIdentity(
    input.portablePlanId,
    STABLE_IDENTITY_BYTES,
    'portable plan',
  )
  const canonicalBytes = canonicalRecord(PORTABLE_BINDING_DOMAIN, [
    frame(requireIdentityBytes(operationId, STABLE_IDENTITY_BYTES, 'operation')),
    frame(requireIdentityBytes(portablePlanId, STABLE_IDENTITY_BYTES, 'portable plan')),
    frame(requireIdentityBytes(artifact.digest, AUTHORITY_REFERENCE_BYTES, 'artifact digest')),
    frame(uint64(DEFAULT_PORTABLE_ARTIFACT_LIMIT)),
    frame(uint64(DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES)),
    frame(uint64(DEFAULT_PORTABLE_MAXIMUM_PARTS)),
    frame(uint64(BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS)),
    frame(Uint8Array.of(2)),
  ])
  return canonicalDigestValue({
    version: PORTABLE_BINDING_VERSION,
    operationId,
    portablePlanId,
    artifactDigest: artifact.digest,
    maximumArtifactBytes: DEFAULT_PORTABLE_ARTIFACT_LIMIT,
    assemblyPartBytes: DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES,
    maximumParts: DEFAULT_PORTABLE_MAXIMUM_PARTS,
    objectUrlLeaseMilliseconds: BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
    preparation: 'exact-artifact' as const,
  }, await digestText(canonicalBytes), canonicalBytes)
}

export async function validatePortableBinding(
  input: PortableBinding,
  artifact: ArtifactSpec,
): Promise<PortableBinding> {
  if (input.version !== PORTABLE_BINDING_VERSION ||
      input.maximumArtifactBytes !== DEFAULT_PORTABLE_ARTIFACT_LIMIT ||
      input.assemblyPartBytes !== DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES ||
      input.maximumParts !== DEFAULT_PORTABLE_MAXIMUM_PARTS ||
      input.objectUrlLeaseMilliseconds !== BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS ||
      input.preparation !== 'exact-artifact') {
    throw new TypeError('portable binding policy is invalid')
  }
  const rebuilt = await createPortableBinding({
    operationId: input.operationId,
    portablePlanId: input.portablePlanId,
    artifact,
  })
  if (input.artifactDigest !== rebuilt.artifactDigest) {
    throw new TypeError('portable binding artifact digest is invalid')
  }
  return requireSameDigestRecord(input, rebuilt, 'portable binding')
}
