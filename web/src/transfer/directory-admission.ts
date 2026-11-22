import {
  CATALOG_NAME_BYTES,
  CATALOG_PATH_BYTES,
  CATALOG_PATH_DEPTH,
} from '../catalog/path-policy'
import { decodeBase64Url, encodeBase64Url } from '../crypto/bytes'
import {
  FaultDomain,
  FaultScope,
  OutputFaultCode,
  isFault,
  outputFault,
  type Fault,
} from './failure/fault'
import {
  RECEIVE_INTENT_DIGEST_BYTES,
  STABLE_IDENTITY_BYTES,
  validateReceiveIntent,
  type ReceiveIntent,
} from './intent'
import {
  createDirectTreeCoordinateContract,
  snapshotMaterializationRootRelativePath,
  type DirectTreeRootExpectation,
  type MaterializationRootRelativePath,
} from './job/coordinate/direct-tree'

import {
  snapshotCanonicalModifiedTime,
  sameModifiedTime,
  type CanonicalModifiedTime,
} from './metadata/modified-time'

export {
  snapshotCanonicalModifiedTime,
  sameModifiedTime,
  type CanonicalModifiedTime,
} from './metadata/modified-time'

export const DIRECTORY_ADMISSION_LAYOUT_VERSION = 2 as const
export const CATALOG_IDENTITY_BYTES = STABLE_IDENTITY_BYTES
export const MAX_MATERIALIZATION_PATH_SEGMENTS = CATALOG_PATH_DEPTH
export const MAX_MATERIALIZATION_SEGMENT_BYTES = CATALOG_NAME_BYTES
export const MAX_MATERIALIZATION_PATH_BYTES = CATALOG_PATH_BYTES

const DIRECTORY_ADMISSION_STRUCTURAL_METADATA_BYTES = 128
const DIRECTORY_ADMISSION_MODIFIED_TIME_BYTES = 24
const MAX_ADMISSION_ID_BYTES = 64
const TEXT_ENCODER = new TextEncoder()
const VALID_SCOPES = new WeakSet<object>()
// WeakMap ties each frozen receipt to its issuing in-tab authority without
// retaining a ledger of past receipts (GC reclaims dropped traversal branches).
const ISSUED_ADMISSIONS = new WeakMap<DirectoryAdmission, symbol>()

export type DirectoryAdmissionLayout =
  | 'directory-tree-single-file'
  | 'directory-tree-result-root'
  | 'directory-tree-catalog-root'
  | 'zip-result-root'

export interface MaterializationDirectory {
  readonly directoryId: string
  readonly generation: string
  readonly path: MaterializationRootRelativePath
  readonly parentAdmission?: DirectoryAdmission
  readonly modifiedTime?: CanonicalModifiedTime
}

export interface DirectoryAdmissionScope {
  readonly receiveIntentDigest: string
  readonly layoutVersion: typeof DIRECTORY_ADMISSION_LAYOUT_VERSION
  readonly layout: DirectoryAdmissionLayout
  readonly rootExpectation: DirectTreeRootExpectation
}

export interface DirectoryAdmission {
  readonly receiveIntentDigest: string
  readonly layoutVersion: typeof DIRECTORY_ADMISSION_LAYOUT_VERSION
  readonly layout: DirectoryAdmissionLayout
  readonly admissionId: string
  readonly directoryId: string
  readonly generation: string
  readonly path: MaterializationRootRelativePath
  readonly parentAdmissionId?: string
  readonly modifiedTime?: CanonicalModifiedTime
}

export interface DirectoryAdmissionAuthority {
  readonly scope: DirectoryAdmissionScope
  issue(directory: MaterializationDirectory): DirectoryAdmission
  verify(admission: DirectoryAdmission): boolean
}

export const DirectorySettlementKind = {
  Finalized: 'finalized',
  IsolatedFailure: 'isolated-failure',
} as const

export type DirectorySettlement =
  | Readonly<{
      kind: typeof DirectorySettlementKind.Finalized
      admission: DirectoryAdmission
    }>
  | Readonly<{
      kind: typeof DirectorySettlementKind.IsolatedFailure
      admission: DirectoryAdmission
      fault: Fault
    }>

export class DirectoryAdmissionBindingError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'DirectoryAdmissionBindingError'
  }
}

export async function createDirectoryAdmissionScope(
  input: ReceiveIntent,
): Promise<DirectoryAdmissionScope> {
  const intent = await validateReceiveIntent(input)
  let layout: DirectoryAdmissionLayout
  let rootExpectation: DirectTreeRootExpectation
  switch (intent.plan.kind) {
    case 'direct-tree': {
      if (intent.artifact.kind !== 'directory-tree') {
        throw new DirectoryAdmissionBindingError('direct-tree intent has no directory-tree artifact')
      }
      switch (intent.artifact.layout.kind) {
        case 'single-file':
          layout = 'directory-tree-single-file'
          break
        case 'result-root':
          layout = 'directory-tree-result-root'
          break
        case 'catalog-root':
          layout = 'directory-tree-catalog-root'
          break
      }
      rootExpectation = (await createDirectTreeCoordinateContract(intent)).rootExpectation
      break
    }
    case 'direct-resumable-zip': {
      if (intent.artifact.kind !== 'zip-archive') {
        throw new DirectoryAdmissionBindingError('Direct ZIP directory admission requires a ZIP')
      }
      layout = 'zip-result-root'
      const anchor = intent.artifact.layout.anchor
      rootExpectation = materializedRootExpectation(
        anchor.kind,
        anchor.kind === 'directory' ? anchor.directoryId : intent.syntheticRoot,
        [],
      )
      break
    }
    case 'workspace-then-publish': {
      if (intent.artifact.kind !== 'zip-archive') {
        throw new DirectoryAdmissionBindingError('Original files do not use directory admission')
      }
      const anchor = intent.artifact.layout.anchor
      layout = 'zip-result-root'
      rootExpectation = materializedRootExpectation(
        anchor.kind,
        anchor.kind === 'directory' ? anchor.directoryId : intent.syntheticRoot,
        [intent.artifact.layout.name],
      )
      break
    }
    case 'portable-handoff':
      throw new DirectoryAdmissionBindingError(
        'prepared materialization uses its sealed manifest rather than directory admission',
      )
  }
  const scope = Object.freeze({
    receiveIntentDigest: requireIdentity(
      intent.digest,
      RECEIVE_INTENT_DIGEST_BYTES,
      'receive intent digest',
    ),
    layoutVersion: DIRECTORY_ADMISSION_LAYOUT_VERSION,
    layout,
    rootExpectation: snapshotRootExpectation(rootExpectation),
  })
  VALID_SCOPES.add(scope)
  return scope
}

export function snapshotDirectoryAdmissionScope(
  input: DirectoryAdmissionScope,
): DirectoryAdmissionScope {
  if (!VALID_SCOPES.has(input)) {
    throw new DirectoryAdmissionBindingError(
      'directory admission scope must be derived from a validated receive intent',
    )
  }
  return input
}

export function createDirectoryAdmissionAuthority(
  inputScope: DirectoryAdmissionScope,
): DirectoryAdmissionAuthority {
  const scope = snapshotDirectoryAdmissionScope(inputScope)
  const authorityToken = Symbol('directory-admission-authority')
  let sequence = 0

  return Object.freeze({
    scope,
    issue(inputDirectory: MaterializationDirectory): DirectoryAdmission {
      const directory = snapshotMaterializationDirectory(inputDirectory)
      validateDirectoryAdmissionScopeBinding(scope, directory)
      if (directory.parentAdmission !== undefined &&
          ISSUED_ADMISSIONS.get(directory.parentAdmission) !== authorityToken) {
        throw new DirectoryAdmissionBindingError(
          'directory parent receipt was not issued by this admission authority',
        )
      }
      sequence += 1
      const admission: DirectoryAdmission = Object.freeze({
        receiveIntentDigest: scope.receiveIntentDigest,
        layoutVersion: scope.layoutVersion,
        layout: scope.layout,
        admissionId: `dir:${sequence}`,
        directoryId: directory.directoryId,
        generation: directory.generation,
        path: directory.path,
        ...(directory.parentAdmission === undefined
          ? {}
          : { parentAdmissionId: directory.parentAdmission.admissionId }),
        ...(directory.modifiedTime === undefined ? {} : { modifiedTime: directory.modifiedTime }),
      })
      ISSUED_ADMISSIONS.set(admission, authorityToken)
      return admission
    },
    verify(admission: DirectoryAdmission): boolean {
      return ISSUED_ADMISSIONS.get(admission) === authorityToken
    },
  })
}

export function snapshotMaterializationDirectory(
  input: MaterializationDirectory,
): MaterializationDirectory {
  const path = snapshotMaterializationPath(input.path)
  const parentAdmission = input.parentAdmission === undefined
    ? undefined
    : snapshotDirectoryAdmission(input.parentAdmission)
  if (parentAdmission !== undefined && !isImmediateChildPath(parentAdmission.path, path)) {
    throw new DirectoryAdmissionBindingError('directory is not an immediate child of its parent receipt')
  }
  const modifiedTime = input.modifiedTime === undefined
    ? undefined
    : snapshotCanonicalModifiedTime(input.modifiedTime)
  return Object.freeze({
    directoryId: requireIdentity(input.directoryId, CATALOG_IDENTITY_BYTES, 'directory'),
    generation: requireIdentity(input.generation, CATALOG_IDENTITY_BYTES, 'directory generation'),
    path,
    ...(parentAdmission === undefined ? {} : { parentAdmission }),
    ...(modifiedTime === undefined ? {} : { modifiedTime }),
  })
}

export function snapshotDirectoryAdmission(input: DirectoryAdmission): DirectoryAdmission {
  // Issued receipts are already validated and deeply frozen; returning the same
  // reference preserves authority ownership through boundary snapshot helpers.
  if (ISSUED_ADMISSIONS.has(input)) return input
  if (input.layoutVersion !== DIRECTORY_ADMISSION_LAYOUT_VERSION) {
    throw new DirectoryAdmissionBindingError('directory admission version is invalid')
  }
  const path = snapshotMaterializationPath(input.path)
  const modifiedTime = input.modifiedTime === undefined
    ? undefined
    : snapshotCanonicalModifiedTime(input.modifiedTime)
  return Object.freeze({
    receiveIntentDigest: requireIdentity(
      input.receiveIntentDigest,
      RECEIVE_INTENT_DIGEST_BYTES,
      'directory admission receive intent',
    ),
    layoutVersion: DIRECTORY_ADMISSION_LAYOUT_VERSION,
    layout: requireDirectoryAdmissionLayout(input.layout),
    admissionId: requireAdmissionId(input.admissionId, 'directory admission ID'),
    directoryId: requireIdentity(input.directoryId, CATALOG_IDENTITY_BYTES, 'directory'),
    generation: requireIdentity(input.generation, CATALOG_IDENTITY_BYTES, 'directory generation'),
    path,
    ...(input.parentAdmissionId === undefined
      ? {}
      : { parentAdmissionId: requireAdmissionId(input.parentAdmissionId, 'parent admission ID') }),
    ...(modifiedTime === undefined ? {} : { modifiedTime }),
  })
}

export function directoryAdmissionRetainedMetadataBytes(
  directory: MaterializationDirectory,
): number {
  let bytes = DIRECTORY_ADMISSION_STRUCTURAL_METADATA_BYTES +
    TEXT_ENCODER.encode(directory.directoryId).byteLength +
    TEXT_ENCODER.encode(directory.generation).byteLength
  if (directory.parentAdmission !== undefined) {
    bytes += TEXT_ENCODER.encode(directory.parentAdmission.admissionId).byteLength
  }
  for (const segment of directory.path) {
    bytes += TEXT_ENCODER.encode(segment).byteLength
  }
  if (directory.modifiedTime !== undefined) bytes += DIRECTORY_ADMISSION_MODIFIED_TIME_BYTES
  return bytes
}

export function validateDirectoryAdmissionBinding(
  inputScope: DirectoryAdmissionScope,
  inputDirectory: MaterializationDirectory,
  inputAdmission: DirectoryAdmission,
): DirectoryAdmission {
  const scope = snapshotDirectoryAdmissionScope(inputScope)
  const directory = snapshotMaterializationDirectory(inputDirectory)
  validateDirectoryAdmissionScopeBinding(scope, directory)
  let admission: DirectoryAdmission
  try {
    admission = snapshotDirectoryAdmission(inputAdmission)
  } catch (cause) {
    throw new DirectoryAdmissionBindingError('materializer returned a malformed directory receipt', {
      cause,
    })
  }
  if (admission.receiveIntentDigest !== scope.receiveIntentDigest ||
      admission.layoutVersion !== scope.layoutVersion ||
      admission.layout !== scope.layout ||
      admission.directoryId !== directory.directoryId ||
      admission.generation !== directory.generation ||
      !sameMaterializationPath(admission.path, directory.path) ||
      admission.parentAdmissionId !== directory.parentAdmission?.admissionId ||
      !sameModifiedTime(admission, directory)) {
    throw new DirectoryAdmissionBindingError(
      'directory receipt does not match its receive intent, layout, ancestry, or generation',
    )
  }
  return admission
}

export function sameDirectoryAdmission(
  leftInput: DirectoryAdmission,
  rightInput: DirectoryAdmission,
): boolean {
  const left = snapshotDirectoryAdmission(leftInput)
  const right = snapshotDirectoryAdmission(rightInput)
  return left.receiveIntentDigest === right.receiveIntentDigest &&
    left.layoutVersion === right.layoutVersion &&
    left.layout === right.layout &&
    left.admissionId === right.admissionId &&
    left.directoryId === right.directoryId &&
    left.generation === right.generation &&
    sameMaterializationPath(left.path, right.path) &&
    left.parentAdmissionId === right.parentAdmissionId &&
    sameModifiedTime(left, right)
}

export function finalizedDirectorySettlement(
  admission: DirectoryAdmission,
): DirectorySettlement {
  return Object.freeze({
    kind: DirectorySettlementKind.Finalized,
    admission: snapshotDirectoryAdmission(admission),
  })
}

export function isolatedDirectorySettlement(
  admission: DirectoryAdmission,
  failure: Fault,
): DirectorySettlement {
  if (!isFault(failure) || failure.domain !== FaultDomain.Output ||
      failure.scope !== FaultScope.DirectoryLocal ||
      failure.code !== OutputFaultCode.DirectoryMetadata) {
    throw new TypeError('isolated directory settlement requires a directory-local metadata fault')
  }
  return Object.freeze({
    kind: DirectorySettlementKind.IsolatedFailure,
    admission: snapshotDirectoryAdmission(admission),
    fault: outputFault(FaultScope.DirectoryLocal, OutputFaultCode.DirectoryMetadata),
  })
}

export function validateDirectorySettlement(
  expectedAdmission: DirectoryAdmission,
  input: DirectorySettlement,
): DirectorySettlement {
  const expected = snapshotDirectoryAdmission(expectedAdmission)
  let settlement: DirectorySettlement
  switch (input.kind) {
    case DirectorySettlementKind.Finalized:
      settlement = finalizedDirectorySettlement(input.admission)
      break
    case DirectorySettlementKind.IsolatedFailure:
      settlement = isolatedDirectorySettlement(input.admission, input.fault)
      break
    default:
      throw new TypeError('directory settlement kind is invalid')
  }
  if (!sameDirectoryAdmission(expected, settlement.admission)) {
    throw new DirectoryAdmissionBindingError('directory settlement belongs to another receipt')
  }
  return settlement
}

export function snapshotMaterializationPath(
  input: readonly string[],
): MaterializationRootRelativePath {
  return snapshotMaterializationRootRelativePath(input)
}

export function sameMaterializationPath(
  left: readonly string[],
  right: readonly string[],
): boolean {
  return left.length === right.length && left.every((segment, index) => segment === right[index])
}

export function isImmediateChildPath(
  parent: readonly string[],
  child: readonly string[],
): boolean {
  return child.length === parent.length + 1 &&
    parent.every((segment, index) => segment === child[index])
}

function validateDirectoryAdmissionScopeBinding(
  scope: DirectoryAdmissionScope,
  directory: MaterializationDirectory,
): void {
  const parent = directory.parentAdmission
  if (parent === undefined) {
    const root = scope.rootExpectation
    if (root.kind !== 'materialized-directory' ||
        directory.directoryId !== root.directoryId ||
        !sameMaterializationPath(directory.path, root.relativePath)) {
      throw new DirectoryAdmissionBindingError(
        'parentless directory does not match the expected materialized root',
      )
    }
    return
  }
  if (scope.rootExpectation.kind === 'none' ||
      parent.receiveIntentDigest !== scope.receiveIntentDigest ||
      parent.layoutVersion !== scope.layoutVersion ||
      parent.layout !== scope.layout ||
      !isImmediateChildPath(parent.path, directory.path)) {
    throw new DirectoryAdmissionBindingError('child directory receipt is outside its frozen scope')
  }
}

function materializedRootExpectation(
  anchorKind: 'directory' | 'synthetic-root' | 'catalog-root',
  directoryId: string,
  relativePath: readonly string[],
): DirectTreeRootExpectation {
  return snapshotRootExpectation({
    kind: 'materialized-directory',
    anchorKind,
    directoryId,
    relativePath: snapshotMaterializationRootRelativePath(relativePath),
  })
}

function snapshotRootExpectation(
  input: DirectTreeRootExpectation,
): DirectTreeRootExpectation {
  if (input.kind === 'none') {
    if (input.anchorKind !== 'single-file') {
      throw new DirectoryAdmissionBindingError('directory root expectation is invalid')
    }
    return Object.freeze({ kind: 'none', anchorKind: 'single-file' })
  }
  if (input.anchorKind !== 'directory' &&
      input.anchorKind !== 'synthetic-root' &&
      input.anchorKind !== 'catalog-root') {
    throw new DirectoryAdmissionBindingError('directory root anchor kind is invalid')
  }
  return Object.freeze({
    kind: 'materialized-directory',
    anchorKind: input.anchorKind,
    directoryId: requireIdentity(input.directoryId, CATALOG_IDENTITY_BYTES, 'expected root directory'),
    relativePath: snapshotMaterializationRootRelativePath(input.relativePath),
  })
}

function requireDirectoryAdmissionLayout(value: string): DirectoryAdmissionLayout {
  switch (value) {
    case 'directory-tree-single-file':
    case 'directory-tree-result-root':
    case 'directory-tree-catalog-root':
    case 'zip-result-root':
      return value
    default:
      throw new DirectoryAdmissionBindingError('directory admission layout is invalid')
  }
}

function requireAdmissionId(value: string, label: string): string {
  if (typeof value !== 'string' || value.length === 0 ||
      TEXT_ENCODER.encode(value).byteLength > MAX_ADMISSION_ID_BYTES) {
    throw new DirectoryAdmissionBindingError(`${label} must be a bounded non-empty identifier`)
  }
  return value
}

function requireIdentity(value: string, width: number, label: string): string {
  if (typeof value !== 'string') throw new TypeError(label + ' must be a canonical base64url identity')
  const decoded = decodeBase64Url(value)
  if (decoded === undefined || decoded.byteLength !== width ||
      decoded.every((byte) => byte === 0) || encodeBase64Url(decoded) !== value) {
    throw new TypeError(label + ' must be a non-zero canonical ' + width + '-byte identity')
  }
  return value
}
