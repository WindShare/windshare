import { sha256 } from '../../crypto/digest'
import {
  DESTINATION_RESERVATION_DOMAIN,
  NAME_COLLISION_DOMAIN,
  PORTABLE_BINDING_DOMAIN,
  TEXT_ENCODER,
  WORKSPACE_BINDING_DOMAIN,
  canonicalDigestValue,
  canonicalRecord,
  concat,
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
  MAX_RESULT_COMPONENT_BYTES,
  PORTABLE_BINDING_VERSION,
  STABLE_IDENTITY_BYTES,
  WORKSPACE_BINDING_VERSION,
  type ArtifactSpec,
  type AtomicTargetReservation,
  type CommitVisibility,
  type ContainerRootReservation,
  type DestinationReservation,
  type GuaranteeSet,
  type NameAuthority,
  type NamedContainerEntryReservation,
  type PortableBinding,
  type ReplacementGuarantee,
  type WorkspaceBinding,
} from './model'
import {
  completeArtifactName,
  requireResultName,
  validateArtifactSpec,
} from './selection'

export function nativeTreeGuarantees(): GuaranteeSet {
  return guaranteeSet({
    profile: 'native-tree',
    nameAuthority: 'application-chosen',
    replacement: 'atomic-no-replace',
    delivery: 'managed-target',
    visibility: 'prefix-visible',
    rollback: 'none',
  })
}

export function fsaTreeGuarantees(): GuaranteeSet {
  return guaranteeSet({
    profile: 'fsa-tree',
    nameAuthority: 'application-chosen',
    replacement: 'coordinated-no-replace',
    delivery: 'managed-target',
    visibility: 'prefix-visible',
    rollback: 'none',
  })
}

export function managedAtomicGuarantees(
  nameAuthority: 'application-chosen' | 'user-chosen',
): GuaranteeSet {
  if (nameAuthority !== 'application-chosen' && nameAuthority !== 'user-chosen') {
    throw new TypeError('managed atomic name authority is invalid')
  }
  return guaranteeSet({
    profile: 'managed-atomic',
    nameAuthority,
    replacement: 'atomic-no-replace',
    delivery: 'managed-target',
    visibility: 'atomic-commit',
    rollback: 'to-absent',
  })
}

export function browserHandoffGuarantees(): GuaranteeSet {
  return guaranteeSet({
    profile: 'browser-handoff',
    nameAuthority: 'browser-chosen',
    replacement: 'unknown',
    delivery: 'browser-handoff',
    visibility: 'unobservable',
    rollback: 'none',
  })
}

function guaranteeSet(value: GuaranteeSet): GuaranteeSet {
  return Object.freeze({ ...value })
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
  readonly reservedName: string
  readonly collisionIndex: number
}): Promise<NamedContainerEntryReservation> {
  return createNamedEntryReservation({ ...input, authorityKind: 'native-container' })
}

export async function createFSANamedEntryReservation(input: {
  readonly operationId: string
  readonly reservationId: string
  readonly artifact: ArtifactSpec
  readonly authorityRef: string
  readonly reservedName: string
  readonly collisionIndex: number
}): Promise<NamedContainerEntryReservation> {
  return createNamedEntryReservation({ ...input, authorityKind: 'fsa-container' })
}

async function createNamedEntryReservation(input: {
  readonly operationId: string
  readonly reservationId: string
  readonly artifact: ArtifactSpec
  readonly authorityRef: string
  readonly authorityKind: 'native-container' | 'fsa-container'
  readonly reservedName: string
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
  const reservedName = requireResultName(input.reservedName)
  const expected = await collisionName(
    input.operationId,
    requestedName,
    input.collisionIndex,
    entryKind === 'single-file',
  )
  if (reservedName !== expected) throw new TypeError('named-entry collision decision is invalid')
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
    reservedName,
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
      reservedName: string
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
      fields.push(frame(TEXT_ENCODER.encode(input.reservedName)))
      fields.push(frame(uint32(input.collisionIndex)))
      variant = {
        kind: input.kind,
        entryKind: input.entryKind,
        requestedName: input.requestedName,
        reservedName: input.reservedName,
        collisionIndex: input.collisionIndex,
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
      const options = {
        operationId: input.operationId,
        reservationId: input.reservationId,
        artifact,
        authorityRef: input.authorityRef,
        reservedName: input.reservedName,
        collisionIndex: input.collisionIndex,
      }
      rebuilt = input.authorityKind === 'native-container'
        ? await createNativeNamedEntryReservation(options)
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

export function snapshotGuarantees(input: GuaranteeSet): GuaranteeSet {
  let expected: GuaranteeSet
  switch (input.profile) {
    case 'native-tree':
      expected = nativeTreeGuarantees()
      break
    case 'fsa-tree':
      expected = fsaTreeGuarantees()
      break
    case 'managed-atomic':
      expected = managedAtomicGuarantees(managedNameAuthority(input.nameAuthority))
      break
    case 'browser-handoff':
      expected = browserHandoffGuarantees()
      break
    default:
      throw new TypeError('guarantee profile is invalid')
  }
  if (!sameGuarantees(input, expected)) throw new TypeError('guarantee profile fields are invalid')
  return expected
}

export function sameGuarantees(left: GuaranteeSet, right: GuaranteeSet): boolean {
  return left.profile === right.profile &&
    left.nameAuthority === right.nameAuthority &&
    left.replacement === right.replacement &&
    left.delivery === right.delivery &&
    left.visibility === right.visibility &&
    left.rollback === right.rollback
}

export function canonicalGuarantees(value: GuaranteeSet): Uint8Array<ArrayBuffer> {
  return concat([
    frame(Uint8Array.of(nameAuthorityByte(value.nameAuthority))),
    frame(Uint8Array.of(replacementGuaranteeByte(value.replacement))),
    frame(Uint8Array.of(value.delivery === 'managed-target' ? 1 : 2)),
    frame(Uint8Array.of(commitVisibilityByte(value.visibility))),
    frame(Uint8Array.of(value.rollback === 'to-absent' ? 1 : 2)),
  ])
}

export function managedNameAuthority(
  value: NameAuthority,
): 'application-chosen' | 'user-chosen' {
  if (value !== 'application-chosen' && value !== 'user-chosen') {
    throw new TypeError('managed atomic name authority is invalid')
  }
  return value
}

export function reservationKindByte(value: DestinationReservation['kind']): number {
  switch (value) {
    case 'container-root': return 1
    case 'named-container-entry': return 2
    case 'atomic-target': return 3
  }
}

export function authorityKindByte(value: DestinationReservation['authorityKind']): number {
  switch (value) {
    case 'native-container': return 1
    case 'fsa-container': return 2
    case 'managed-atomic-target': return 3
  }
}

function nameAuthorityByte(value: NameAuthority): number {
  switch (value) {
    case 'application-chosen': return 1
    case 'user-chosen': return 2
    case 'browser-chosen': return 3
  }
}

function replacementGuaranteeByte(value: ReplacementGuarantee): number {
  switch (value) {
    case 'atomic-no-replace': return 1
    case 'coordinated-no-replace': return 2
    case 'user-authorized-replace': return 3
    case 'unknown': return 4
  }
}

function commitVisibilityByte(value: CommitVisibility): number {
  switch (value) {
    case 'atomic-commit': return 1
    case 'prefix-visible': return 2
    case 'unobservable': return 3
  }
}
