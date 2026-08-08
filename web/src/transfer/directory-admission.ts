import {
  snapshotPortableCatalogPath,
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
import type { TransferIntent } from './intent'

export const MAXIMUM_OUTPUT_PATH_SEGMENTS = V2_CATALOG_PATH_DEPTH
export const MAXIMUM_OUTPUT_SEGMENT_BYTES = V2_CATALOG_NAME_BYTES
export const MAXIMUM_OUTPUT_PATH_BYTES = V2_CATALOG_PATH_BYTES
export const DIRECTORY_ADMISSION_SECRET_BYTES = 32
export const DIRECTORY_ADMISSION_TOKEN_BYTES = 32
export const DIRECTORY_ADMISSION_SCHEMA_VERSION = 1 as const

export const OUTPUT_CATALOG_IDENTITY_BYTES = 16
const TRANSFER_INTENT_DIGEST_BYTES = 32
const MAXIMUM_PORTABLE_MODIFIED_SECONDS = 9_007_199_254_740_991n
const NANOSECONDS_PER_SECOND = 1_000_000_000
const NANOSECONDS_PER_MILLISECOND_NUMBER = 1_000_000
const NANOSECONDS_PER_MILLISECOND = 1_000_000n

const DIRECTORY_ADMISSION_DOMAIN = new TextEncoder().encode(
  'windshare/directory-admission',
)

/** The catalog's portable timestamp representation is part of directory identity. */
export interface OutputModifiedTime {
  readonly seconds: bigint
  readonly nanoseconds: number
  readonly precision: 1 | 2 | 3
  readonly milliseconds: bigint
}

export interface OutputDirectory {
  readonly path: readonly string[]
  /** Authenticated catalog identity for this generation. */
  readonly directoryId: string
  readonly generation: string
  /** Finalization applies only to non-root directories admitted beneath this proof. */
  readonly parentAdmission: DirectoryAdmission
  readonly modifiedTime?: OutputModifiedTime
}

/** Input to the per-generation output admission boundary. Root paths may be empty. */
export interface OutputDirectoryAdmission {
  readonly directoryId: string
  readonly generation: string
  readonly path: readonly string[]
  readonly parentAdmission?: DirectoryAdmission
  readonly modifiedTime?: OutputModifiedTime
}

/** Runtime receipt scope frozen only after TransferIntent and output authority validate. */
export interface DirectoryAdmissionScope {
  readonly transferIntentDigest: string
  readonly syntheticRoot: string
}

/**
 * The token is never accepted as a substitute for its bound fields, so copying
 * a path cannot authorize another committed generation.
 */
export interface DirectoryAdmission {
  readonly schemaVersion: typeof DIRECTORY_ADMISSION_SCHEMA_VERSION
  readonly transferIntentDigest: string
  readonly token: string
  readonly directoryId: string
  readonly generation: string
  readonly path: readonly string[]
  readonly parentToken?: string
  readonly modifiedTime?: OutputModifiedTime
}

export const DirectorySettlementKind = {
  Finalized: 'Finalized',
  IsolatedFailure: 'IsolatedFailure',
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

export function finalizedDirectorySettlement(admission: DirectoryAdmission): DirectorySettlement {
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
    throw new TypeError('Isolated directory settlement requires a directory-local metadata fault')
  }
  return Object.freeze({
    kind: DirectorySettlementKind.IsolatedFailure,
    admission: snapshotDirectoryAdmission(admission),
    fault: outputFault(FaultScope.DirectoryLocal, OutputFaultCode.DirectoryMetadata),
  })
}

export function validateDirectorySettlement(
  expectedAdmission: DirectoryAdmission,
  settlement: DirectorySettlement,
): DirectorySettlement {
  const expected = snapshotDirectoryAdmission(expectedAdmission)
  let snapshot: DirectorySettlement
  if (settlement.kind === DirectorySettlementKind.Finalized) {
    snapshot = finalizedDirectorySettlement(settlement.admission)
  } else if (settlement.kind === DirectorySettlementKind.IsolatedFailure) {
    snapshot = isolatedDirectorySettlement(settlement.admission, settlement.fault)
  } else {
    throw new TypeError('Directory settlement kind is invalid')
  }
  if (!sameDirectoryAdmission(expected, snapshot.admission)) {
    throw new DirectoryAdmissionBindingError('directory settlement belongs to another admission')
  }
  return snapshot
}

export function directoryAdmissionScope(
  intent: Pick<TransferIntent, 'digest' | 'syntheticRoot'>,
): DirectoryAdmissionScope {
  return snapshotDirectoryAdmissionScope({
    transferIntentDigest: intent.digest,
    syntheticRoot: intent.syntheticRoot,
  })
}

export function snapshotDirectoryAdmissionScope(
  scope: DirectoryAdmissionScope,
): DirectoryAdmissionScope {
  return Object.freeze({
    transferIntentDigest: requireOpaqueIdentity(
      scope.transferIntentDigest,
      TRANSFER_INTENT_DIGEST_BYTES,
      'transfer intent digest',
    ),
    syntheticRoot: requireOpaqueIdentity(
      scope.syntheticRoot,
      OUTPUT_CATALOG_IDENTITY_BYTES,
      'synthetic root',
    ),
  })
}

export function snapshotOutputDirectory(directory: OutputDirectory): OutputDirectory {
  const request = snapshotOutputDirectoryAdmission(directory)
  if (request.path.length === 0 || request.parentAdmission === undefined) {
    throw new DirectoryAdmissionBindingError(
      'output directory finalization requires a non-root admitted generation',
    )
  }
  return Object.freeze({
    directoryId: request.directoryId,
    generation: request.generation,
    path: request.path,
    parentAdmission: request.parentAdmission,
    ...(request.modifiedTime === undefined ? {} : { modifiedTime: request.modifiedTime }),
  })
}

export function snapshotOutputDirectoryAdmission(
  directory: OutputDirectoryAdmission,
): OutputDirectoryAdmission {
  if (directory.path.length > MAXIMUM_OUTPUT_PATH_SEGMENTS) {
    throw new TypeError('output admission path exceeds its segment limit')
  }
  // The synthetic root has an empty in-memory path; ordinary output paths still
  // use the stricter portable path policy.
  const path = directory.path.length === 0
    ? Object.freeze([])
    : snapshotOutputPath(directory.path)
  const parentAdmission = directory.parentAdmission === undefined
    ? undefined
    : snapshotDirectoryAdmission(directory.parentAdmission)
  if (path.length === 0 && parentAdmission !== undefined) {
    throw new DirectoryAdmissionBindingError('synthetic root admission must not have a parent')
  }
  if (path.length > 0 && parentAdmission === undefined) {
    throw new DirectoryAdmissionBindingError('child directory admission requires its parent admission')
  }
  if (parentAdmission !== undefined && !isImmediateChildPath(parentAdmission.path, path)) {
    throw new DirectoryAdmissionBindingError(
      'child directory path does not descend directly from its parent admission',
    )
  }
  const modifiedTime = snapshotOutputModifiedTimeFields(directory)
  return Object.freeze({
    directoryId: requireOpaqueIdentity(
      directory.directoryId,
      OUTPUT_CATALOG_IDENTITY_BYTES,
      'directory',
    ),
    generation: requireOpaqueIdentity(
      directory.generation,
      OUTPUT_CATALOG_IDENTITY_BYTES,
      'directory generation',
    ),
    path,
    ...(parentAdmission === undefined ? {} : { parentAdmission }),
    ...modifiedTime,
  })
}

export function snapshotDirectoryAdmission(admission: DirectoryAdmission): DirectoryAdmission {
  if (admission.schemaVersion !== DIRECTORY_ADMISSION_SCHEMA_VERSION) {
    throw new DirectoryAdmissionBindingError('directory admission schema version is invalid')
  }
  const path = admission.path.length === 0
    ? Object.freeze([])
    : snapshotOutputPath(admission.path)
  if (path.length === 0 && admission.parentToken !== undefined) {
    throw new DirectoryAdmissionBindingError('synthetic root proof must not have a parent token')
  }
  if (path.length > 0 && admission.parentToken === undefined) {
    throw new DirectoryAdmissionBindingError('child directory proof requires a parent token')
  }
  const modifiedTime = snapshotOutputModifiedTimeFields(admission)
  return Object.freeze({
    schemaVersion: DIRECTORY_ADMISSION_SCHEMA_VERSION,
    transferIntentDigest: requireOpaqueIdentity(
      admission.transferIntentDigest,
      TRANSFER_INTENT_DIGEST_BYTES,
      'directory admission intent digest',
    ),
    token: requireOpaqueIdentity(
      admission.token,
      DIRECTORY_ADMISSION_TOKEN_BYTES,
      'directory admission',
    ),
    directoryId: requireOpaqueIdentity(
      admission.directoryId,
      OUTPUT_CATALOG_IDENTITY_BYTES,
      'directory',
    ),
    generation: requireOpaqueIdentity(
      admission.generation,
      OUTPUT_CATALOG_IDENTITY_BYTES,
      'directory generation',
    ),
    path,
    ...(admission.parentToken === undefined
      ? {}
      : {
          parentToken: requireOpaqueIdentity(
            admission.parentToken,
            DIRECTORY_ADMISSION_TOKEN_BYTES,
            'parent admission',
          ),
        }),
    ...modifiedTime,
  })
}

/**
 * The per-session secret stays in the admission ledger; callers receive no
 * derivation authority unless a deterministic vector secret is injected.
 */
export function createDirectoryAdmissionSecret(): Uint8Array<ArrayBuffer> {
  const secret = new Uint8Array(DIRECTORY_ADMISSION_SECRET_BYTES)
  if (globalThis.crypto?.getRandomValues === undefined) {
    throw new DOMException(
      'Secure directory-admission secret generation is unavailable',
      'NotSupportedError',
    )
  }
  globalThis.crypto.getRandomValues(secret)
  if (secret.every((value) => value === 0)) {
    throw new Error('Generated directory-admission secret was all zeroes')
  }
  return secret
}

/** Returns the exact HMAC message encoded by Go's DirectoryAdmission V1 codec. */
export function canonicalDirectoryAdmissionMessageV1(
  inputScope: DirectoryAdmissionScope,
  input: OutputDirectoryAdmission,
): Uint8Array<ArrayBuffer> {
  const scope = snapshotDirectoryAdmissionScope(inputScope)
  const request = snapshotOutputDirectoryAdmission(input)
  validateDirectoryAdmissionScopeBinding(scope, request)
  const directoryId = requireOpaqueBytes(
    request.directoryId,
    OUTPUT_CATALOG_IDENTITY_BYTES,
    'directory',
  )
  const generation = requireOpaqueBytes(
    request.generation,
    OUTPUT_CATALOG_IDENTITY_BYTES,
    'directory generation',
  )
  const path = new TextEncoder().encode(request.path.join('/'))
  const parent = request.parentAdmission === undefined
    ? new Uint8Array()
    : requireOpaqueBytes(
        request.parentAdmission.token,
        DIRECTORY_ADMISSION_TOKEN_BYTES,
        'parent admission',
      )
  const modified = canonicalModifiedTimeBytes(request.modifiedTime)
  return concatOutputBytes([
    frameDirectoryAdmissionField(DIRECTORY_ADMISSION_DOMAIN),
    uint16DirectoryAdmissionVersion(),
    frameDirectoryAdmissionField(requireOpaqueBytes(
      scope.transferIntentDigest,
      TRANSFER_INTENT_DIGEST_BYTES,
      'transfer intent digest',
    )),
    frameDirectoryAdmissionField(directoryId),
    frameDirectoryAdmissionField(generation),
    frameDirectoryAdmissionField(parent),
    frameDirectoryAdmissionField(path),
    frameDirectoryAdmissionField(modified),
  ])
}

/** Derives the URL-safe, unpadded 32-byte admission token used by Go. */
export async function deriveDirectoryAdmissionToken(
  secret: Uint8Array<ArrayBufferLike>,
  scope: DirectoryAdmissionScope,
  input: OutputDirectoryAdmission,
): Promise<string> {
  const message = canonicalDirectoryAdmissionMessageV1(scope, input)
  const key = await importDirectoryAdmissionHmacKey(secret, ['sign'])
  return encodeBase64Url(new Uint8Array(
    await globalThis.crypto.subtle.sign('HMAC', key, message),
  ))
}

/** Tests and protocol vectors may inject a deterministic secret; production does not. */
export async function createDirectoryAdmission(
  secret: Uint8Array<ArrayBufferLike>,
  inputScope: DirectoryAdmissionScope,
  input: OutputDirectoryAdmission,
): Promise<DirectoryAdmission> {
  const scope = snapshotDirectoryAdmissionScope(inputScope)
  const request = snapshotOutputDirectoryAdmission(input)
  validateDirectoryAdmissionScopeBinding(scope, request)
  const token = await deriveDirectoryAdmissionToken(secret, scope, request)
  return Object.freeze({
    schemaVersion: DIRECTORY_ADMISSION_SCHEMA_VERSION,
    transferIntentDigest: scope.transferIntentDigest,
    token,
    directoryId: request.directoryId,
    generation: request.generation,
    path: request.path,
    ...(request.parentAdmission === undefined
      ? {}
      : { parentToken: request.parentAdmission.token }),
    ...(request.modifiedTime === undefined ? {} : { modifiedTime: request.modifiedTime }),
  })
}

/** A receipt remains untrusted until every committed-generation field is exact. */
export function validateDirectoryAdmissionBinding(
  inputScope: DirectoryAdmissionScope,
  input: OutputDirectoryAdmission,
  admission: DirectoryAdmission,
): DirectoryAdmission {
  const scope = snapshotDirectoryAdmissionScope(inputScope)
  const request = snapshotOutputDirectoryAdmission(input)
  validateDirectoryAdmissionScopeBinding(scope, request)
  let proof: DirectoryAdmission
  try {
    proof = snapshotDirectoryAdmission(admission)
  } catch (cause) {
    throw new DirectoryAdmissionBindingError(
      'output backend returned a malformed directory admission',
      { cause },
    )
  }
  if (proof.transferIntentDigest !== scope.transferIntentDigest ||
      proof.directoryId !== request.directoryId ||
      proof.generation !== request.generation ||
      !sameOutputPath(proof.path, request.path) ||
      !sameDirectoryAdmissionToken(proof.parentToken, request.parentAdmission?.token) ||
      !sameModifiedTime(proof, request)) {
    throw new DirectoryAdmissionBindingError(
      'output backend returned a directory admission for a different committed generation',
    )
  }
  return proof
}

/** WebCrypto verifies the HMAC without a data-dependent byte-prefix comparison. */
export async function verifyDirectoryAdmissionToken(
  secret: Uint8Array<ArrayBufferLike>,
  scope: DirectoryAdmissionScope,
  input: OutputDirectoryAdmission,
  token: string,
): Promise<boolean> {
  const tokenBytes = requireOpaqueBytes(
    token,
    DIRECTORY_ADMISSION_TOKEN_BYTES,
    'directory admission',
  )
  const message = canonicalDirectoryAdmissionMessageV1(scope, input)
  const key = await importDirectoryAdmissionHmacKey(secret, ['verify'])
  return globalThis.crypto.subtle.verify('HMAC', key, tokenBytes, message)
}

export function sameDirectoryAdmissionToken(
  left: string | undefined,
  right: string | undefined,
): boolean {
  if (left === undefined || right === undefined) {
    return left === undefined && right === undefined
  }
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
  left: DirectoryAdmission,
  right: DirectoryAdmission,
): boolean {
  const leftSnapshot = snapshotDirectoryAdmission(left)
  const rightSnapshot = snapshotDirectoryAdmission(right)
  return leftSnapshot.schemaVersion === rightSnapshot.schemaVersion &&
    leftSnapshot.transferIntentDigest === rightSnapshot.transferIntentDigest &&
    sameDirectoryAdmissionToken(leftSnapshot.token, rightSnapshot.token) &&
    leftSnapshot.directoryId === rightSnapshot.directoryId &&
    leftSnapshot.generation === rightSnapshot.generation &&
    sameOutputPath(leftSnapshot.path, rightSnapshot.path) &&
    sameDirectoryAdmissionToken(leftSnapshot.parentToken, rightSnapshot.parentToken) &&
    sameModifiedTime(leftSnapshot, rightSnapshot)
}

export interface OutputModifiedTimeCarrier {
  readonly modifiedTime?: OutputModifiedTime
}

export function snapshotOutputModifiedTimeFields(
  input: OutputModifiedTimeCarrier,
): { readonly modifiedTime?: OutputModifiedTime } {
  const modifiedTime = input.modifiedTime === undefined
    ? undefined
    : snapshotOutputModifiedTime(input.modifiedTime)
  return modifiedTime === undefined ? {} : { modifiedTime }
}

export function sameModifiedTime(
  left: OutputModifiedTimeCarrier,
  right: OutputModifiedTimeCarrier,
): boolean {
  if (left.modifiedTime === undefined || right.modifiedTime === undefined) {
    return left.modifiedTime === undefined && right.modifiedTime === undefined
  }
  return left.modifiedTime.seconds === right.modifiedTime.seconds &&
    left.modifiedTime.nanoseconds === right.modifiedTime.nanoseconds &&
    left.modifiedTime.precision === right.modifiedTime.precision &&
    left.modifiedTime.milliseconds === right.modifiedTime.milliseconds
}

export function sameOutputPath(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((segment, index) => segment === right[index])
}

export function isImmediateChildPath(parent: readonly string[], child: readonly string[]): boolean {
  return child.length === parent.length + 1 && sameOutputPath(parent, child.slice(0, -1))
}

export function snapshotOutputPath(path: readonly string[]): readonly string[] {
  return snapshotPortableCatalogPath(path)
}

function snapshotOutputModifiedTime(input: OutputModifiedTime): OutputModifiedTime {
  if (typeof input.seconds !== 'bigint' ||
      input.seconds < -MAXIMUM_PORTABLE_MODIFIED_SECONDS ||
      input.seconds > MAXIMUM_PORTABLE_MODIFIED_SECONDS ||
      !Number.isSafeInteger(input.nanoseconds) ||
      input.nanoseconds < 0 ||
      input.nanoseconds >= NANOSECONDS_PER_SECOND ||
      (input.precision !== 1 && input.precision !== 2 && input.precision !== 3)) {
    throw new DirectoryAdmissionBindingError('modified-time tuple is outside the portable range')
  }
  if ((input.precision === 1 && input.nanoseconds !== 0) ||
      (input.precision === 2 &&
       input.nanoseconds % NANOSECONDS_PER_MILLISECOND_NUMBER !== 0)) {
    throw new DirectoryAdmissionBindingError('modified-time tuple violates its declared precision')
  }
  const milliseconds = input.seconds * 1_000n +
    BigInt(Math.trunc(input.nanoseconds / Number(NANOSECONDS_PER_MILLISECOND)))
  if (input.milliseconds !== milliseconds) {
    throw new DirectoryAdmissionBindingError(
      'modified-time milliseconds do not match its seconds and nanoseconds',
    )
  }
  return Object.freeze({
    seconds: input.seconds,
    nanoseconds: input.nanoseconds,
    precision: input.precision,
    milliseconds,
  })
}

function validateDirectoryAdmissionScopeBinding(
  scope: DirectoryAdmissionScope,
  request: OutputDirectoryAdmission,
): void {
  if (request.path.length === 0) {
    if (request.directoryId !== scope.syntheticRoot || request.parentAdmission !== undefined) {
      throw new DirectoryAdmissionBindingError(
        'synthetic root admission must match the frozen intent root and have no parent',
      )
    }
    return
  }
  if (request.parentAdmission?.transferIntentDigest !== scope.transferIntentDigest) {
    throw new DirectoryAdmissionBindingError(
      'child directory admission belongs to another transfer intent',
    )
  }
}

async function importDirectoryAdmissionHmacKey(
  input: Uint8Array<ArrayBufferLike>,
  usages: readonly KeyUsage[],
): Promise<CryptoKey> {
  if (globalThis.crypto?.subtle === undefined) {
    throw new DOMException('Directory-admission HMAC is unavailable', 'NotSupportedError')
  }
  const secret = requireRawBytes(
    input,
    DIRECTORY_ADMISSION_SECRET_BYTES,
    'directory admission secret',
  )
  if (secret.every((value) => value === 0)) {
    throw new DirectoryAdmissionBindingError('directory admission secret must not be all zeroes')
  }
  return globalThis.crypto.subtle.importKey(
    'raw',
    secret,
    { name: 'HMAC', hash: 'SHA-256' },
    false,
    usages,
  )
}

function uint16DirectoryAdmissionVersion(): Uint8Array<ArrayBuffer> {
  const encoded = new Uint8Array(2)
  new DataView(encoded.buffer).setUint16(0, DIRECTORY_ADMISSION_SCHEMA_VERSION, false)
  return encoded
}

function frameDirectoryAdmissionField(value: Uint8Array): Uint8Array<ArrayBuffer> {
  const encoded = new Uint8Array(4 + value.byteLength)
  new DataView(encoded.buffer).setUint32(0, value.byteLength, false)
  encoded.set(value, 4)
  return encoded
}

function requireOpaqueIdentity(value: string, byteLength: number, label: string): string {
  const decoded = decodeBase64Url(value)
  if (decoded === undefined || decoded.byteLength !== byteLength ||
      decoded.every((byte) => byte === 0) || encodeBase64Url(decoded) !== value) {
    throw new DirectoryAdmissionBindingError(
      `${label} must be a non-zero ${byteLength}-byte base64url identity`,
    )
  }
  return encodeBase64Url(decoded)
}

function requireRawBytes(
  value: Uint8Array<ArrayBufferLike>,
  byteLength: number,
  label: string,
): Uint8Array<ArrayBuffer> {
  if (!(value instanceof Uint8Array) || value.byteLength !== byteLength) {
    throw new DirectoryAdmissionBindingError(`${label} must be exactly ${byteLength} bytes`)
  }
  return Uint8Array.from(value)
}

function requireOpaqueBytes(
  value: string,
  byteLength: number,
  label: string,
): Uint8Array<ArrayBuffer> {
  const decoded = decodeBase64Url(value)
  if (decoded === undefined || decoded.byteLength !== byteLength ||
      decoded.every((byte) => byte === 0) || encodeBase64Url(decoded) !== value) {
    throw new DirectoryAdmissionBindingError(
      `${label} must be a non-zero ${byteLength}-byte base64url identity`,
    )
  }
  return Uint8Array.from(decoded)
}

function canonicalModifiedTimeBytes(
  modifiedTime: OutputModifiedTime | undefined,
): Uint8Array<ArrayBuffer> {
  if (modifiedTime === undefined) return Uint8Array.of(0)
  const bytes = new Uint8Array(1 + 8 + 4 + 1)
  const view = new DataView(bytes.buffer)
  bytes[0] = 1
  view.setBigUint64(1, BigInt.asUintN(64, modifiedTime.seconds), false)
  view.setUint32(9, modifiedTime.nanoseconds, false)
  bytes[13] = modifiedTime.precision
  return bytes
}

function concatOutputBytes(parts: readonly Uint8Array[]): Uint8Array<ArrayBuffer> {
  const result = new Uint8Array(parts.reduce((total, part) => total + part.byteLength, 0))
  let offset = 0
  for (const part of parts) {
    result.set(part, offset)
    offset += part.byteLength
  }
  return result
}
