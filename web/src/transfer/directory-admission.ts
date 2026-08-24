import {
  V2_CATALOG_NAME_BYTES,
  V2_CATALOG_PATH_BYTES,
  V2_CATALOG_PATH_DEPTH,
} from '../catalog/path-policy'
import { decodeBase64Url, encodeBase64Url } from '../crypto/bytes'
import {
  FaultDomain,
  FaultScope,
  OutputFaultCode,
  isFault,
  outputFault,
  type Fault,
} from './fault'
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

type CanonicalBytes = Uint8Array<ArrayBuffer>

export const DIRECTORY_ADMISSION_SCHEMA_VERSION = 2 as const
export const DIRECTORY_ADMISSION_LAYOUT_VERSION = 2 as const
export const DIRECTORY_ADMISSION_SECRET_BYTES = 32
export const DIRECTORY_ADMISSION_TOKEN_BYTES = 32
export const CATALOG_IDENTITY_BYTES = STABLE_IDENTITY_BYTES
export const MAX_MATERIALIZATION_PATH_SEGMENTS = V2_CATALOG_PATH_DEPTH
export const MAX_MATERIALIZATION_SEGMENT_BYTES = V2_CATALOG_NAME_BYTES
export const MAX_MATERIALIZATION_PATH_BYTES = V2_CATALOG_PATH_BYTES

const DIRECTORY_ADMISSION_DOMAIN = 'windshare/directory-admission/v2'
const TEXT_ENCODER = new TextEncoder()
const MAX_PORTABLE_MODIFIED_SECONDS = 9_007_199_254_740_991n
const NANOSECONDS_PER_SECOND = 1_000_000_000
const NANOSECONDS_PER_MILLISECOND = 1_000_000
const VALID_SCOPES = new WeakSet<object>()

export type DirectoryAdmissionLayout =
  | 'directory-tree-single-file'
  | 'directory-tree-result-root'
  | 'directory-tree-catalog-root'
  | 'zip-result-root'

export interface CanonicalModifiedTime {
  readonly seconds: bigint
  readonly nanoseconds: number
  readonly precision: 1 | 2 | 3
}

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
  readonly schemaVersion: typeof DIRECTORY_ADMISSION_SCHEMA_VERSION
  readonly receiveIntentDigest: string
  readonly layoutVersion: typeof DIRECTORY_ADMISSION_LAYOUT_VERSION
  readonly layout: DirectoryAdmissionLayout
  readonly token: string
  readonly directoryId: string
  readonly generation: string
  readonly path: MaterializationRootRelativePath
  readonly parentToken?: string
  readonly modifiedTime?: CanonicalModifiedTime
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
    case 'direct-atomic':
      throw new DirectoryAdmissionBindingError(
        'DirectAtomic original-file output does not use directory admission',
      )
    case 'workspace-then-publish':
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
  if (input.schemaVersion !== DIRECTORY_ADMISSION_SCHEMA_VERSION ||
      input.layoutVersion !== DIRECTORY_ADMISSION_LAYOUT_VERSION) {
    throw new DirectoryAdmissionBindingError('directory admission version is invalid')
  }
  const path = snapshotMaterializationPath(input.path)
  const modifiedTime = input.modifiedTime === undefined
    ? undefined
    : snapshotCanonicalModifiedTime(input.modifiedTime)
  return Object.freeze({
    schemaVersion: DIRECTORY_ADMISSION_SCHEMA_VERSION,
    receiveIntentDigest: requireIdentity(
      input.receiveIntentDigest,
      RECEIVE_INTENT_DIGEST_BYTES,
      'directory admission receive intent',
    ),
    layoutVersion: DIRECTORY_ADMISSION_LAYOUT_VERSION,
    layout: requireDirectoryAdmissionLayout(input.layout),
    token: requireIdentity(input.token, DIRECTORY_ADMISSION_TOKEN_BYTES, 'directory admission token'),
    directoryId: requireIdentity(input.directoryId, CATALOG_IDENTITY_BYTES, 'directory'),
    generation: requireIdentity(input.generation, CATALOG_IDENTITY_BYTES, 'directory generation'),
    path,
    ...(input.parentToken === undefined
      ? {}
      : {
          parentToken: requireIdentity(
            input.parentToken,
            DIRECTORY_ADMISSION_TOKEN_BYTES,
            'parent admission token',
          ),
        }),
    ...(modifiedTime === undefined ? {} : { modifiedTime }),
  })
}

export function createDirectoryAdmissionSecret(): CanonicalBytes {
  if (globalThis.crypto?.getRandomValues === undefined) {
    throw new DOMException(
      'Secure directory-admission secret generation is unavailable',
      'NotSupportedError',
    )
  }
  const secret = new Uint8Array(DIRECTORY_ADMISSION_SECRET_BYTES)
  globalThis.crypto.getRandomValues(secret)
  if (secret.every((byte) => byte === 0)) {
    throw new Error('Generated directory-admission secret was all zeroes')
  }
  return secret
}

export function canonicalDirectoryAdmissionMessageV2(
  inputScope: DirectoryAdmissionScope,
  inputDirectory: MaterializationDirectory,
): CanonicalBytes {
  const scope = snapshotDirectoryAdmissionScope(inputScope)
  const directory = snapshotMaterializationDirectory(inputDirectory)
  validateDirectoryAdmissionScopeBinding(scope, directory)
  const parent = directory.parentAdmission === undefined
    ? new Uint8Array()
    : requireIdentityBytes(
        directory.parentAdmission.token,
        DIRECTORY_ADMISSION_TOKEN_BYTES,
        'parent admission token',
      )
  return concat([
    TEXT_ENCODER.encode(DIRECTORY_ADMISSION_DOMAIN),
    Uint8Array.of(0, DIRECTORY_ADMISSION_SCHEMA_VERSION),
    frame(requireIdentityBytes(
      scope.receiveIntentDigest,
      RECEIVE_INTENT_DIGEST_BYTES,
      'receive intent digest',
    )),
    frame(Uint8Array.of(scope.layoutVersion)),
    frame(Uint8Array.of(directoryAdmissionLayoutByte(scope.layout))),
    ...canonicalRootExpectationFields(scope.rootExpectation),
    frame(requireIdentityBytes(directory.directoryId, CATALOG_IDENTITY_BYTES, 'directory')),
    frame(requireIdentityBytes(directory.generation, CATALOG_IDENTITY_BYTES, 'directory generation')),
    frame(parent),
    frame(canonicalDirectoryAdmissionPath(directory.path)),
    frame(canonicalModifiedTimeBytes(directory.modifiedTime)),
  ])
}

export function directoryAdmissionRetainedMetadataBytes(
  scope: DirectoryAdmissionScope,
  directory: MaterializationDirectory,
): number {
  return canonicalDirectoryAdmissionMessageV2(scope, directory).byteLength +
    DIRECTORY_ADMISSION_TOKEN_BYTES
}

export async function deriveDirectoryAdmissionToken(
  secretInput: Uint8Array<ArrayBufferLike>,
  scope: DirectoryAdmissionScope,
  directory: MaterializationDirectory,
): Promise<string> {
  const secret = snapshotAdmissionSecret(secretInput)
  const key = await importHMACKey(secret, ['sign'])
  const message = canonicalDirectoryAdmissionMessageV2(scope, directory)
  return encodeBase64Url(new Uint8Array(
    await globalThis.crypto.subtle.sign('HMAC', key, message),
  ))
}

export async function createDirectoryAdmission(
  secret: Uint8Array<ArrayBufferLike>,
  inputScope: DirectoryAdmissionScope,
  inputDirectory: MaterializationDirectory,
): Promise<DirectoryAdmission> {
  const scope = snapshotDirectoryAdmissionScope(inputScope)
  const directory = snapshotMaterializationDirectory(inputDirectory)
  validateDirectoryAdmissionScopeBinding(scope, directory)
  const token = await deriveDirectoryAdmissionToken(secret, scope, directory)
  return Object.freeze({
    schemaVersion: DIRECTORY_ADMISSION_SCHEMA_VERSION,
    receiveIntentDigest: scope.receiveIntentDigest,
    layoutVersion: scope.layoutVersion,
    layout: scope.layout,
    token,
    directoryId: directory.directoryId,
    generation: directory.generation,
    path: directory.path,
    ...(directory.parentAdmission === undefined
      ? {}
      : { parentToken: directory.parentAdmission.token }),
    ...(directory.modifiedTime === undefined ? {} : { modifiedTime: directory.modifiedTime }),
  })
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
      !sameDirectoryAdmissionToken(admission.parentToken, directory.parentAdmission?.token) ||
      !sameModifiedTime(admission, directory)) {
    throw new DirectoryAdmissionBindingError(
      'directory receipt does not match its receive intent, layout, ancestry, or generation',
    )
  }
  return admission
}

export async function verifyDirectoryAdmissionToken(
  secretInput: Uint8Array<ArrayBufferLike>,
  scope: DirectoryAdmissionScope,
  directory: MaterializationDirectory,
  token: string,
): Promise<boolean> {
  const secret = snapshotAdmissionSecret(secretInput)
  const tokenBytes = requireIdentityBytes(
    token,
    DIRECTORY_ADMISSION_TOKEN_BYTES,
    'directory admission token',
  )
  const key = await importHMACKey(secret, ['verify'])
  return globalThis.crypto.subtle.verify(
    'HMAC',
    key,
    tokenBytes,
    canonicalDirectoryAdmissionMessageV2(scope, directory),
  )
}

export function sameDirectoryAdmissionToken(
  left: string | undefined,
  right: string | undefined,
): boolean {
  if (left === undefined || right === undefined) return left === right
  const leftBytes = decodeBase64Url(left)
  const rightBytes = decodeBase64Url(right)
  if (leftBytes === undefined || rightBytes === undefined ||
      leftBytes.byteLength !== DIRECTORY_ADMISSION_TOKEN_BYTES ||
      rightBytes.byteLength !== DIRECTORY_ADMISSION_TOKEN_BYTES) return false
  let difference = 0
  for (let index = 0; index < DIRECTORY_ADMISSION_TOKEN_BYTES; index += 1) {
    difference |= leftBytes[index]! ^ rightBytes[index]!
  }
  return difference === 0
}

export function sameDirectoryAdmission(
  leftInput: DirectoryAdmission,
  rightInput: DirectoryAdmission,
): boolean {
  const left = snapshotDirectoryAdmission(leftInput)
  const right = snapshotDirectoryAdmission(rightInput)
  return left.schemaVersion === right.schemaVersion &&
    left.receiveIntentDigest === right.receiveIntentDigest &&
    left.layoutVersion === right.layoutVersion &&
    left.layout === right.layout &&
    sameDirectoryAdmissionToken(left.token, right.token) &&
    left.directoryId === right.directoryId &&
    left.generation === right.generation &&
    sameMaterializationPath(left.path, right.path) &&
    sameDirectoryAdmissionToken(left.parentToken, right.parentToken) &&
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

export function snapshotCanonicalModifiedTime(
  input: CanonicalModifiedTime,
): CanonicalModifiedTime {
  if (typeof input.seconds !== 'bigint' ||
      input.seconds < -MAX_PORTABLE_MODIFIED_SECONDS ||
      input.seconds > MAX_PORTABLE_MODIFIED_SECONDS ||
      !Number.isInteger(input.nanoseconds) ||
      input.nanoseconds < 0 ||
      input.nanoseconds >= NANOSECONDS_PER_SECOND ||
      (input.precision !== 1 && input.precision !== 2 && input.precision !== 3) ||
      (input.precision === 1 && input.nanoseconds !== 0) ||
      (input.precision === 2 && input.nanoseconds % NANOSECONDS_PER_MILLISECOND !== 0)) {
    throw new TypeError('modified time violates the canonical portable representation')
  }
  return Object.freeze({
    seconds: input.seconds,
    nanoseconds: input.nanoseconds,
    precision: input.precision,
  })
}

export function sameModifiedTime(
  left: { readonly modifiedTime?: CanonicalModifiedTime },
  right: { readonly modifiedTime?: CanonicalModifiedTime },
): boolean {
  if (left.modifiedTime === undefined || right.modifiedTime === undefined) {
    return left.modifiedTime === right.modifiedTime
  }
  return left.modifiedTime.seconds === right.modifiedTime.seconds &&
    left.modifiedTime.nanoseconds === right.modifiedTime.nanoseconds &&
    left.modifiedTime.precision === right.modifiedTime.precision
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

function canonicalRootExpectationFields(
  input: DirectTreeRootExpectation,
): readonly CanonicalBytes[] {
  const root = snapshotRootExpectation(input)
  if (root.kind === 'none') {
    return Object.freeze([
      frame(Uint8Array.of(1)),
      frame(new Uint8Array()),
      frame(new Uint8Array()),
    ])
  }
  return Object.freeze([
    frame(Uint8Array.of(directoryRootAnchorByte(root.anchorKind))),
    frame(requireIdentityBytes(root.directoryId, CATALOG_IDENTITY_BYTES, 'expected root directory')),
    frame(canonicalDirectoryAdmissionPath(root.relativePath)),
  ])
}

function directoryRootAnchorByte(
  anchorKind: 'directory' | 'synthetic-root' | 'catalog-root',
): number {
  switch (anchorKind) {
    case 'directory': return 2
    case 'synthetic-root': return 3
    case 'catalog-root': return 4
  }
}

function canonicalDirectoryAdmissionPath(path: readonly string[]): CanonicalBytes {
  if (path.length === 0) return Uint8Array.of(1)
  return concat([
    Uint8Array.of(2),
    frame(canonicalPathBytes(path)),
  ])
}

function canonicalPathBytes(pathInput: readonly string[]): CanonicalBytes {
  const path = snapshotMaterializationPath(pathInput)
  if (path.length === 0) throw new TypeError('canonical child path cannot be empty')
  return concat([
    uint64(BigInt(path.length)),
    ...path.map((segment) => frame(TEXT_ENCODER.encode(segment))),
  ])
}

function canonicalModifiedTimeBytes(
  input: CanonicalModifiedTime | undefined,
): CanonicalBytes {
  if (input === undefined) return Uint8Array.of(1)
  const modified = snapshotCanonicalModifiedTime(input)
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

function directoryAdmissionLayoutByte(value: DirectoryAdmissionLayout): number {
  switch (value) {
    case 'directory-tree-single-file': return 1
    case 'directory-tree-result-root': return 2
    case 'directory-tree-catalog-root': return 3
    case 'zip-result-root': return 4
  }
}

function snapshotAdmissionSecret(input: Uint8Array<ArrayBufferLike>): CanonicalBytes {
  if (!(input instanceof Uint8Array) ||
      input.byteLength !== DIRECTORY_ADMISSION_SECRET_BYTES ||
      input.every((byte) => byte === 0)) {
    throw new DirectoryAdmissionBindingError(
      'directory admission secret must be a non-zero 32-byte value',
    )
  }
  return Uint8Array.from(input)
}

async function importHMACKey(
  secret: Uint8Array<ArrayBuffer>,
  usage: readonly ('sign' | 'verify')[],
): Promise<CryptoKey> {
  if (globalThis.crypto?.subtle === undefined) {
    throw new DOMException('WebCrypto HMAC is unavailable', 'NotSupportedError')
  }
  return globalThis.crypto.subtle.importKey(
    'raw',
    secret,
    { name: 'HMAC', hash: 'SHA-256' },
    false,
    usage,
  )
}

function requireIdentity(value: string, width: number, label: string): string {
  return encodeBase64Url(requireIdentityBytes(value, width, label))
}

function requireIdentityBytes(value: string, width: number, label: string): CanonicalBytes {
  if (typeof value !== 'string') throw new TypeError(label + ' must be a canonical base64url identity')
  const decoded = decodeBase64Url(value)
  if (decoded === undefined || decoded.byteLength !== width ||
      decoded.every((byte) => byte === 0) || encodeBase64Url(decoded) !== value) {
    throw new TypeError(label + ' must be a non-zero canonical ' + width + '-byte identity')
  }
  return Uint8Array.from(decoded)
}

function frame(value: Uint8Array): CanonicalBytes {
  return concat([uint64(BigInt(value.byteLength)), value])
}

function uint64(value: bigint): CanonicalBytes {
  if (value < 0n || value > 0xffff_ffff_ffff_ffffn) throw new RangeError('u64 is outside its range')
  const output = new Uint8Array(8)
  new DataView(output.buffer).setBigUint64(0, value)
  return output
}

function concat(parts: readonly Uint8Array[]): CanonicalBytes {
  const total = parts.reduce((sum, part) => sum + part.byteLength, 0)
  const output = new Uint8Array(total)
  let offset = 0
  for (const part of parts) {
    output.set(part, offset)
    offset += part.byteLength
  }
  return output
}
