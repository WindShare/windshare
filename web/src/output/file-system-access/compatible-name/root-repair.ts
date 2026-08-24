import { catalogNameCollisionKey } from '../../../catalog/path-policy'
import { snapshotMaterializationRootRelativePath } from '../../../transfer/job/coordinate/direct-tree'
import {
  PathComponentRejectedError,
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
  type CompatibleNamePairPlacement,
} from './model'
import {
  COMPATIBLE_NAME_COLLISION_RETRY_LIMIT,
  COMPATIBLE_NAME_SUFFIX,
  compatibleNameCandidate,
  generateCompatibleNamePrimaryToken,
} from './naming'
import {
  LogicalSiblingNamespaceAuthority,
  PhysicalPathResolver,
} from './resolver'
import {
  provenRestorationTemplateProvider,
  type RestorationTemplate,
} from './restoration-template'
import type {
  CompatibleNameRejectedComponentInput,
  CompatibleNameRootRepairPreparationOptions,
  CompatibleNameRootRepairRequest,
  PreparedCompatibleNameRootRepair,
} from './path-authority'

export const RESTORATION_PAIR_READABLE_PREFIX = 'restore'
export const RESTORATION_SIDECAR_EXTENSION = '.data'
const RESTORATION_PAIR_HANDLE_DOMAIN = 'windshare/fsa-compatible-name-pair/v1'

export async function prepareCompatibleNameRootRepair(
  input: CompatibleNameRootRepairRequest,
  options: CompatibleNameRootRepairPreparationOptions,
): Promise<PreparedCompatibleNameRootRepair> {
  assertRootRejection(input)
  const template = (options.templateProvider ?? provenRestorationTemplateProvider)
    .select(options.platform)
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
  const scriptCandidate = await allocateCandidate({
    parent: input.parent,
    operationId: input.operationId,
    logicalPath: [`${RESTORATION_PAIR_READABLE_PREFIX}${template.scriptFileExtension}`],
    entryKind: 'file',
    primaryToken,
    claims,
    physicalName: candidate => pairPhysicalName(candidate.token, template.scriptFileExtension),
    diagnosticStage: 'fsa.file.entry.inspect',
  })
  const sidecarCandidate = await allocateCandidate({
    parent: input.parent,
    operationId: input.operationId,
    logicalPath: [`${RESTORATION_PAIR_READABLE_PREFIX}${RESTORATION_SIDECAR_EXTENSION}`],
    entryKind: 'file',
    primaryToken,
    claims,
    physicalName: candidate => pairPhysicalName(candidate.token, RESTORATION_SIDECAR_EXTENSION),
    diagnosticStage: 'fsa.file.entry.inspect',
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
    templateId: template.id,
    pairPlacement,
    pair: {
      script: pairIdentity(input.operationId, 'script', scriptCandidate, randomOwnedObjectId),
      sidecar: pairIdentity(input.operationId, 'sidecar', sidecarCandidate, randomOwnedObjectId),
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
  candidate: AllocatedPhysicalCandidate,
  randomOwnedObjectId: () => string,
) {
  return Object.freeze({
    physicalName: candidate.physicalComponent,
    handleId: pairHandleId(operationId, pairKind),
    ownedObjectId: requireOwnedObjectId(randomOwnedObjectId()),
    ownershipState: 'claimed' as const,
  })
}

export function selectMatchingTemplate(
  options: CompatibleNameRootRepairPreparationOptions,
  expectedId: string,
): RestorationTemplate {
  const template = (options.templateProvider ?? provenRestorationTemplateProvider)
    .select(options.platform)
  if (template.id !== expectedId) {
    throw new DOMException('Compatible-name restoration template changed on reopen', 'InvalidStateError')
  }
  return template
}

function assertRootRejection(input: CompatibleNameRootRepairRequest): void {
  if (!(input.rejection instanceof PathComponentRejectedError) || !input.rejection.preMutation ||
      input.rejection.stage !== 'fsa.root.entry.inspect' ||
      input.rejection.canonicalComponent !== input.logicalReservedName ||
      input.rejection.expectedKind !== input.entryKind) {
    throw new TypeError('compatible-name root repair requires the exact classified root refusal')
  }
}

export function assertDescendantRejection(input: CompatibleNameRejectedComponentInput): void {
  const expectedStage = input.entryKind === 'directory'
    ? 'fsa.directory.entry.inspect'
    : 'fsa.file.entry.inspect'
  const canonicalPath = snapshotMaterializationRootRelativePath(input.artifactPath)
  if (!(input.rejection instanceof PathComponentRejectedError) || !input.rejection.preMutation ||
      input.rejection.stage !== expectedStage ||
      input.rejection.canonicalComponent !== canonicalPath.at(-1) ||
      input.rejection.expectedKind !== input.entryKind) {
    throw new TypeError('compatible-name descendant repair requires the exact classified refusal')
  }
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
      snapshot.header.templateId !== bootstrap.header.templateId ||
      snapshot.header.pairPlacement !== bootstrap.header.pairPlacement ||
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

export function pairPhysicalName(token: string, extension: string): string {
  return `${RESTORATION_PAIR_READABLE_PREFIX}${COMPATIBLE_NAME_SUFFIX}${token}${extension}`
}

function pairHandleId(operationId: string, pairKind: CompatibleNamePairKind): string {
  return `${RESTORATION_PAIR_HANDLE_DOMAIN}/${operationId}/${pairKind}`
}
