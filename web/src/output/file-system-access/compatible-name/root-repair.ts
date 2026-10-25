import { catalogNameCollisionKey } from '../../../catalog/path-policy'
import { snapshotMaterializationRootRelativePath } from '../../../transfer/job/coordinate/direct-tree'
import {
  PathComponentRejectedError,
  descendantInspectionStage,
  inspectFileSystemComponent,
} from '../../browser/filesystem-component-inspection'
import { createOwnedObjectId, requireOwnedObjectId } from '../../browser/filesystem-file-record'
import {
  compatibleNameMappingV1,
  compatibleNameOperationBootstrapV1,
  compatibleNameOperationHeaderV1,
  type CompatibleNameEntryKind,
  type CompatibleNameMappingV1,
  type CompatibleNameOperationBootstrapV1,
  type CompatibleNameOperationSnapshotV1,
  type CompatibleNamePairKind,
} from './model'
import {
  type CompatibleNamePairPlacement,
} from '../../../transfer/lifecycle/name-repair'
import {
  COMPATIBLE_NAME_COLLISION_RETRY_LIMIT,
  compatibleNameCandidate,
  compatibleNameRestorationPairCandidate,
  generateCompatibleNamePrimaryToken,
} from './naming'
import {
  LogicalSiblingNamespaceAuthority,
  PhysicalPathResolver,
} from './resolver'
import { selectRestorationTemplate } from './restoration-publication'
import type {
  CompatibleNameRejectedComponentInput,
  CompatibleNameRootRepairPreparationOptions,
  CompatibleNameRootRepairRequest,
  PreparedCompatibleNameRootRepair,
} from './path-authority'

const RESTORATION_PAIR_HANDLE_DOMAIN = 'windshare/fsa-compatible-name-pair/v1'

export async function prepareCompatibleNameRootRepair(
  input: CompatibleNameRootRepairRequest,
  options: CompatibleNameRootRepairPreparationOptions,
): Promise<PreparedCompatibleNameRootRepair> {
  assertRootRejection(input)
  const template = selectRestorationTemplate(options)
  const primaryToken = generateCompatibleNamePrimaryToken(options.randomBits)
  const randomOwnedObjectId = options.randomOwnedObjectId ?? createOwnedObjectId
  const claims = new WeakMap<FileSystemDirectoryHandle, Set<string>>()
  const rootCandidate = await allocateCandidate({
    parent: input.parent,
    operationId: input.operationId,
    logicalPath: [input.logicalReservedName],
    entryKind: input.entryKind,
    primaryToken,
    claims,
    physicalName: candidate => candidate.physicalComponent,
    diagnosticStage: 'fsa.root.entry.inspect',
  })
  const pairCandidate = await allocateRestorationPair({
    parent: input.parent, operationId: input.operationId, primaryToken, claims,
  })
  const pairPlacement: CompatibleNamePairPlacement = 'beside-mapped-root'
  const header = compatibleNameOperationHeaderV1({
    operationId: input.operationId,
    primaryToken,
    authorityRef: input.authorityRef,
    root: {
      logicalName: input.logicalReservedName,
      physicalName: rootCandidate.physicalComponent,
    },
    pairPlacement,
    pair: {
      token: pairCandidate.token,
      script: pairIdentity(input.operationId, 'script', { physicalComponent: pairCandidate.script }, randomOwnedObjectId),
      sidecar: pairIdentity(input.operationId, 'sidecar', { physicalComponent: pairCandidate.sidecar }, randomOwnedObjectId),
    },
    activationState: 'prepared',
  })
  const initialMapping = compatibleNameMappingV1({
    operationId: input.operationId,
    logicalPath: [input.logicalReservedName],
    entryKind: input.entryKind,
    physicalComponent: rootCandidate.physicalComponent,
    attempt: rootCandidate.attempt,
    token: rootCandidate.token,
    ownershipState: 'selected',
    commitState: 'uncommitted',
  })
  const bootstrap = compatibleNameOperationBootstrapV1({ header, initialMapping })
  return Object.freeze({ bootstrap, template, parent: input.parent, rejection: input.rejection })
}

interface AllocatedPhysicalCandidate {
  readonly physicalComponent: string
  readonly attempt: number
  readonly token: string
}

export async function allocateCandidate(input: Readonly<{
  parent: FileSystemDirectoryHandle
  operationId: string
  logicalPath: readonly string[]
  entryKind: CompatibleNameEntryKind
  primaryToken: string
  claims: WeakMap<FileSystemDirectoryHandle, Set<string>>
  resolver?: PhysicalPathResolver
  membership?: ReturnType<LogicalSiblingNamespaceAuthority['membership']>
  physicalName: (candidate: Awaited<ReturnType<typeof compatibleNameCandidate>>) => string
  diagnosticStage: 'fsa.root.entry.inspect' | 'fsa.directory.entry.inspect' | 'fsa.file.entry.inspect'
}>): Promise<AllocatedPhysicalCandidate> {
  for (let attempt = 0; attempt <= COMPATIBLE_NAME_COLLISION_RETRY_LIMIT; attempt += 1) {
    const candidate = await compatibleNameCandidate({
      operationId: input.operationId,
      logicalPath: input.logicalPath,
      entryKind: input.entryKind,
      primaryToken: input.primaryToken,
      attempt,
    })
    const physicalComponent = input.physicalName(candidate)
    const claimKey = catalogNameCollisionKey(physicalComponent)
    let parentClaims = input.claims.get(input.parent)
    if (parentClaims === undefined) {
      parentClaims = new Set<string>()
      input.claims.set(input.parent, parentClaims)
    }
    if (parentClaims.has(claimKey) || input.resolver?.hasClaimedPhysicalComponent(
      input.logicalPath.slice(0, -1),
      physicalComponent,
    )) {
      continue
    }
    if (await input.membership?.hasCommittedName(physicalComponent) === true) continue
    const state = await inspectFileSystemComponent({
      verifiedParent: input.parent,
      component: physicalComponent,
      expectedKind: input.entryKind,
      stage: input.diagnosticStage,
      mode: 'diagnostic',
    })
    if (state === 'occupied') continue
    parentClaims.add(claimKey)
    return Object.freeze({ physicalComponent, attempt: candidate.attempt, token: candidate.token })
  }
  throw new DOMException('The compatible-name collision namespace is exhausted', 'InvalidStateError')
}

export function pairIdentity(
  operationId: string,
  pairKind: CompatibleNamePairKind,
  candidate: Pick<AllocatedPhysicalCandidate, 'physicalComponent'>,
  randomOwnedObjectId: () => string,
) {
  return Object.freeze({
    physicalName: candidate.physicalComponent,
    handleId: pairHandleId(operationId, pairKind),
    ownedObjectId: requireOwnedObjectId(randomOwnedObjectId()),
    ownershipState: 'claimed' as const,
  })
}

function assertRootRejection(input: CompatibleNameRootRepairRequest): void {
  if (!(input.rejection instanceof PathComponentRejectedError) || !input.rejection.preMutation ||
      input.rejection.evidenceKind !== 'non-creating-lookup' ||
      input.rejection.stage !== 'fsa.root.entry.inspect' ||
      input.rejection.canonicalComponent !== input.logicalReservedName ||
      input.rejection.expectedKind !== input.entryKind) {
    throw new TypeError('compatible-name root repair requires the exact classified root refusal')
  }
}

export function assertDescendantRejection(input: CompatibleNameRejectedComponentInput): void {
  const canonicalPath = snapshotMaterializationRootRelativePath(input.artifactPath)
  if (!(input.rejection instanceof PathComponentRejectedError) || !input.rejection.preMutation ||
      !descendantEvidenceMatches(input.rejection, input.entryKind) ||
      input.rejection.canonicalComponent !== canonicalPath.at(-1) ||
      input.rejection.expectedKind !== input.entryKind) {
    throw new TypeError('compatible-name descendant repair requires the exact classified refusal')
  }
}

/**
 * Only directory creation has a Chromium write-path policy; file creation and writes are
 * unrestricted. A file refusal must therefore still come from the non-creating lookup.
 */
function descendantEvidenceMatches(
  rejection: PathComponentRejectedError,
  entryKind: CompatibleNameEntryKind,
): boolean {
  if (rejection.evidenceKind === 'non-creating-lookup') {
    return rejection.stage === descendantInspectionStage(entryKind)
  }
  return entryKind === 'directory' && rejection.stage === 'fsa.directory.entry.create'
}

export function assertPreparedSnapshot(
  bootstrap: CompatibleNameOperationBootstrapV1,
  snapshot: CompatibleNameOperationSnapshotV1,
): void {
  const mapping = snapshot.mappings.find(value => value.id === bootstrap.initialMapping.id)
  if (snapshot.mappings.length !== 1 ||
      snapshot.header.operationId !== bootstrap.header.operationId ||
      snapshot.header.primaryToken !== bootstrap.header.primaryToken ||
      snapshot.header.authorityRef !== bootstrap.header.authorityRef ||
      snapshot.header.root.logicalName !== bootstrap.header.root.logicalName ||
      snapshot.header.root.physicalName !== bootstrap.header.root.physicalName ||
      snapshot.header.pairPlacement !== bootstrap.header.pairPlacement ||
      snapshot.header.pair.token !== bootstrap.header.pair.token ||
      snapshot.header.pair.script.physicalName !== bootstrap.header.pair.script.physicalName ||
      snapshot.header.pair.script.handleId !== bootstrap.header.pair.script.handleId ||
      snapshot.header.pair.script.ownedObjectId !== bootstrap.header.pair.script.ownedObjectId ||
      snapshot.header.pair.sidecar.physicalName !== bootstrap.header.pair.sidecar.physicalName ||
      snapshot.header.pair.sidecar.handleId !== bootstrap.header.pair.sidecar.handleId ||
      snapshot.header.pair.sidecar.ownedObjectId !== bootstrap.header.pair.sidecar.ownedObjectId ||
      snapshot.header.pair.script.ownershipState !== 'claimed' ||
      snapshot.header.pair.sidecar.ownershipState !== 'claimed' ||
      snapshot.header.activationState !== 'prepared' ||
      snapshot.header.pendingTerminalOutcome !== undefined ||
      snapshot.header.repairSummary !== undefined || mapping === undefined ||
      !sameMappingSelection(mapping, bootstrap.initialMapping)) {
    throw new DOMException('Durable compatible-name bootstrap changed before pair creation', 'InvalidStateError')
  }
}

function sameMappingSelection(left: CompatibleNameMappingV1, right: CompatibleNameMappingV1): boolean {
  return left.id === right.id && left.operationId === right.operationId &&
    left.entryKind === right.entryKind && left.physicalComponent === right.physicalComponent &&
    left.attempt === right.attempt && left.token === right.token &&
    left.ownershipState === 'selected' && left.commitState === 'uncommitted' &&
    left.logicalPath.length === right.logicalPath.length &&
    left.logicalPath.every((component, index) => component === right.logicalPath[index])
}

function pairHandleId(operationId: string, pairKind: CompatibleNamePairKind): string {
  return `${RESTORATION_PAIR_HANDLE_DOMAIN}/${operationId}/${pairKind}`
}

/** The report is required; script collisions prefer another pair but cannot prevent saving. */
export async function allocateRestorationPair(input: Readonly<{
  parent: FileSystemDirectoryHandle
  operationId: string
  primaryToken: string
  claims: WeakMap<FileSystemDirectoryHandle, Set<string>>
  membership?: ReturnType<LogicalSiblingNamespaceAuthority['membership']>
}>): Promise<Awaited<ReturnType<typeof compatibleNameRestorationPairCandidate>>> {
  let parentClaims = input.claims.get(input.parent)
  if (parentClaims === undefined) {
    parentClaims = new Set<string>()
    input.claims.set(input.parent, parentClaims)
  }
  const occupied = async (component: string) => parentClaims.has(catalogNameCollisionKey(component)) ||
    await input.membership?.hasCommittedName(component) === true ||
    await inspectFileSystemComponent({
      verifiedParent: input.parent, component, expectedKind: 'file',
      stage: 'fsa.file.entry.inspect', mode: 'diagnostic',
    }) === 'occupied'
  type Pair = Awaited<ReturnType<typeof compatibleNameRestorationPairCandidate>>
  const claim = (pair: Pair) => {
    for (const name of [pair.script, pair.sidecar]) parentClaims.add(catalogNameCollisionKey(name))
    return pair
  }
  let reportCandidate: Pair | undefined
  for (let attempt = 0; attempt <= COMPATIBLE_NAME_COLLISION_RETRY_LIMIT; attempt += 1) {
    const pair = await compatibleNameRestorationPairCandidate({ ...input, attempt })
    if (await occupied(pair.sidecar)) continue
    reportCandidate ??= pair
    try {
      if (await occupied(pair.script)) continue
    } catch {
      // Some filesystems reject script names. Publication will remain optional and ownership-checked.
    }
    return claim(pair)
  }
  if (reportCandidate !== undefined) return claim(reportCandidate)
  throw new DOMException('The restoration report collision namespace is exhausted', 'InvalidStateError')
}
