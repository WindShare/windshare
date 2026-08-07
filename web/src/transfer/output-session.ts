import { ByteRangeSet, type ByteRange } from '../content/geometry'
import {
  snapshotPortableCatalogPath,
  V2_CATALOG_NAME_BYTES,
  V2_CATALOG_PATH_BYTES,
  V2_CATALOG_PATH_DEPTH,
} from '../catalog/path-policy'
import { decodeBase64Url, encodeBase64Url } from '../crypto/bytes'
import { sha256 } from '../crypto/digest'
import type { JobOutcome } from './outcome'
import type { TransferIntent, TransferIntentDraft } from './intent'

export type DurabilityLevel = 'None' | 'ProcessRestart' | 'PowerLoss'
export type FileAbortDisposition = 'FileIsolated' | 'JobOutputCompromised'

export const MAXIMUM_OUTPUT_PATH_SEGMENTS = V2_CATALOG_PATH_DEPTH
export const MAXIMUM_OUTPUT_SEGMENT_BYTES = V2_CATALOG_NAME_BYTES
export const MAXIMUM_OUTPUT_PATH_BYTES = V2_CATALOG_PATH_BYTES
export const MAXIMUM_OPEN_OUTPUT_FILES = 32
export const MAXIMUM_OUTPUT_IDENTITY_BYTES = 128
export const MAXIMUM_OWNED_FILE_IDENTITY_BYTES = MAXIMUM_OUTPUT_PATH_BYTES + 256
export const MAXIMUM_VERIFIED_DURABLE_RANGES = 16_384
export const DIRECTORY_ADMISSION_SECRET_BYTES = 32
export const DIRECTORY_ADMISSION_TOKEN_BYTES = 32
const CATALOG_IDENTITY_BYTES = 16

const DIRECTORY_ADMISSION_DOMAIN = new TextEncoder().encode(
  'windshare/directory-admission/session-v1\0',
)

/** The catalog's portable timestamp representation is part of directory identity. */
export interface OutputModifiedTime {
  readonly seconds: bigint
  readonly nanoseconds: number
  readonly precision: 1 | 2 | 3
  readonly milliseconds: bigint
}

const MAXIMUM_PORTABLE_MODIFIED_SECONDS = 9_007_199_254_740_991n
const NANOSECONDS_PER_SECOND = 1_000_000_000
const NANOSECONDS_PER_MILLISECOND_NUMBER = 1_000_000
const NANOSECONDS_PER_MILLISECOND = 1_000_000n

export interface OutputCapabilities {
  readonly durability: DurabilityLevel
  readonly randomWrite: boolean
  readonly fileFailureIsolation: boolean
  readonly modificationTime: boolean
}

export interface OutputSourceIdentity {
  readonly shareInstance: string
  readonly fileId: string
  readonly fileRevision: string
}

export interface OutputSessionIdentity {
  readonly backend: string
  readonly outputSessionId: string
}

export interface OutputFileOwnership extends OutputSessionIdentity {
  readonly canonicalPath: readonly string[]
  /** Prevents a journal-owned path from matching a pre-existing file at the same path. */
  readonly ownedFileIdentity: string
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

/**
 * An admission is intentionally an object produced by the output backend. The
 * token is never accepted from a caller as a substitute for the bound fields;
 * this prevents a copied path from being used with a different generation.
 */
export interface DirectoryAdmission {
  readonly token: string
  readonly directoryId: string
  readonly generation: string
  readonly path: readonly string[]
  readonly parentToken?: string
  readonly modifiedTime?: OutputModifiedTime
}

export interface OutputFile {
  readonly source: OutputSourceIdentity
  readonly path: readonly string[]
  readonly exactSize: bigint
  /**
   * Optional in the input shape so the output boundary can reject malformed
   * callers explicitly. Every production BeginFile call requires this proof.
   */
  readonly parentAdmission?: DirectoryAdmission
  readonly modifiedTime?: OutputModifiedTime
}

/** Only a backend may return this value after reopening and validating output. */
export class VerifiedDurableRanges {
  readonly ownership: OutputFileOwnership
  readonly source: OutputSourceIdentity
  readonly #ranges: ByteRangeSet

  constructor(
    ownership: OutputFileOwnership,
    source: OutputSourceIdentity,
    fileSize: bigint,
    ranges: readonly ByteRange[],
  ) {
    this.ownership = snapshotOutputOwnership(ownership)
    this.source = snapshotOutputSource(source)
    this.#ranges = verifiedDurableRangeSet(fileSize, ranges)
  }

  get fileSize(): bigint {
    return this.#ranges.fileSize
  }

  get ranges(): readonly ByteRange[] {
    return this.#ranges.ranges
  }

  covers(range: ByteRange): boolean {
    return this.#ranges.covers(range)
  }

  asRangeSet(): ByteRangeSet {
    return new ByteRangeSet(this.fileSize, this.ranges)
  }
}

export interface OutputFileTransaction {
  writeRange(offset: bigint, data: Uint8Array, signal: AbortSignal): Promise<void>
  /** Durable sessions report journaled ranges; transient streams retain an empty range set. */
  checkpoint(signal: AbortSignal): Promise<VerifiedDurableRanges>
  commit(signal: AbortSignal): Promise<void>
  /** A streaming backend may still isolate a failure when no bytes were emitted. */
  abort(reason: unknown): Promise<FileAbortDisposition>
}

export interface BeginOutputFileResult {
  readonly transaction: OutputFileTransaction
  readonly durableRanges: VerifiedDurableRanges
}

/**
 * Transfer orchestration consumes this narrow interface and cannot infer that a
 * completed write is durable. Backend implementations own validation and journals.
 */
export interface OutputSession {
  readonly identity: OutputSessionIdentity
  /** Exact final representation authorized by TransferIntent. */
  readonly format: TransferIntent['output']['format']
  readonly capabilities: OutputCapabilities
  /**
   * Admit one authenticated, terminal catalog generation. Implementations may
   * reject the call when the parent token or generation binding is invalid.
   */
  admitDirectory(directory: OutputDirectoryAdmission, signal: AbortSignal): Promise<DirectoryAdmission>
  finalizeDirectory(directory: OutputDirectory, signal: AbortSignal): Promise<void>
  /** Rejection must leave no unowned partial output because no transaction is returned. */
  beginFile(file: OutputFile, signal: AbortSignal): Promise<BeginOutputFileResult>
  /**
   * The session retains settlement ownership of every returned transaction until
   * file settlement or job settlement. A terminal transfer fault stops invoking
   * that transaction; suspendJob/abortJob must then settle it without a race.
   */
  finishJob(outcome: JobOutcome, signal: AbortSignal): Promise<void>
  abortJob(reason: unknown): Promise<void>
  /** Durable backends retain verified ranges while releasing live browser resources. */
  suspendJob?(reason: unknown): Promise<void>
}

export class OutputSessionBindingError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'OutputSessionBindingError'
  }
}

/** Backend-authored classification for a directory mutation that may be isolated safely. */
export class OutputDirectoryMutationError extends Error {
  readonly sessionCompromised: boolean

  constructor(message: string, sessionCompromised: boolean, options?: ErrorOptions) {
    super(message, options)
    this.name = 'OutputDirectoryMutationError'
    this.sessionCompromised = sessionCompromised
  }
}

/** The backend cannot prove cleanup after acquiring mutable output authority. */
export class OutputSessionCompromisedError extends Error {
  constructor(message: string, options: ErrorOptions) {
    super(message, options)
    this.name = 'OutputSessionCompromisedError'
  }
}

/** Rejects an adapter that opens a backend or representation other than the frozen intent. */
export function validateOutputSessionBinding(
  intent: TransferIntent,
  session: OutputSession,
): OutputSession {
  const identity = outputSessionIdentity(session.identity)
  const capabilities = outputCapabilities(session.capabilities)
  if (identity.backend !== intent.output.backend || session.format !== intent.output.format) {
    throw new OutputSessionBindingError(
      'output session backend or format does not match the frozen transfer intent',
    )
  }
  const directStream = capabilities.durability === 'None' &&
    !capabilities.randomWrite &&
    !capabilities.fileFailureIsolation
  const restartableStaging = capabilities.durability === 'ProcessRestart' &&
    capabilities.randomWrite &&
    capabilities.fileFailureIsolation
  if ((session.format === 'single-file' && !directStream) ||
      (session.format === 'zip' && !directStream && !restartableStaging)) {
    throw new OutputSessionBindingError(
      'stream output capabilities contradict the frozen output format',
    )
  }
  return session
}

/** Picker confirmation freezes the durable namespace before generation-scoped admission begins. */
export interface V2OutputAuthority {
  /**
   * Picker-backed authorities resolve the draft only after the user confirms a
   * destination. The returned intent is therefore the first durable identity
   * that may cross into the output session.
   */
  confirmOutput(
    draft: TransferIntentDraft,
    signal: AbortSignal,
  ): Promise<{
    readonly intent: TransferIntent
    readonly session: OutputSession
  }>
  /** Picker-confirmed intent is the only input that can open a durable session. */
  openOutput(intent: TransferIntent, signal: AbortSignal): Promise<OutputSession>
  abort(reason: unknown): Promise<void>
}

export class OutputSessionSuspendedError extends Error {
  constructor() {
    super('Output session suspended for receiver restart')
    this.name = 'OutputSessionSuspendedError'
  }
}

/**
 * A finite output-operation budget was exhausted before the backend accepted
 * more state. Jobs treat this as resumable policy pressure, not corrupt output.
 */
export class OutputBudgetExceededError extends Error {
  readonly budget: string
  readonly limit: bigint
  readonly attempted: bigint

  constructor(budget: string, limit: bigint, attempted: bigint) {
    super(`Output budget ${budget} permits ${limit.toString()}, attempted ${attempted.toString()}`)
    this.name = 'OutputBudgetExceededError'
    this.budget = budget
    this.limit = limit
    this.attempted = attempted
  }
}

export class DirectoryAdmissionBindingError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'DirectoryAdmissionBindingError'
  }
}

interface ModifiedTimeCarrier {
  readonly modifiedTime?: OutputModifiedTime
}

function snapshotModifiedTimeFields(
  input: ModifiedTimeCarrier,
): { readonly modifiedTime?: OutputModifiedTime } {
  const modifiedTime = input.modifiedTime === undefined
    ? undefined
    : snapshotOutputModifiedTime(input.modifiedTime)
  return modifiedTime === undefined ? {} : { modifiedTime }
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
      (input.precision === 2 && input.nanoseconds % NANOSECONDS_PER_MILLISECOND_NUMBER !== 0)) {
    throw new DirectoryAdmissionBindingError('modified-time tuple violates its declared precision')
  }
  const milliseconds = input.seconds * 1_000n +
    BigInt(Math.trunc(input.nanoseconds / Number(NANOSECONDS_PER_MILLISECOND)))
  if (input.milliseconds !== milliseconds) {
    throw new DirectoryAdmissionBindingError('modified-time milliseconds do not match its seconds and nanoseconds')
  }
  return Object.freeze({
    seconds: input.seconds,
    nanoseconds: input.nanoseconds,
    precision: input.precision,
    milliseconds,
  })
}

export function outputCapabilities(
  capabilities: OutputCapabilities,
): OutputCapabilities {
  if ((capabilities.durability !== 'None' &&
       capabilities.durability !== 'ProcessRestart' &&
       capabilities.durability !== 'PowerLoss') ||
      typeof capabilities.randomWrite !== 'boolean' ||
      typeof capabilities.fileFailureIsolation !== 'boolean' ||
      typeof capabilities.modificationTime !== 'boolean') {
    throw new OutputSessionBindingError('output session reported malformed capabilities')
  }
  return Object.freeze({ ...capabilities })
}

export function outputSessionIdentity(identity: OutputSessionIdentity): OutputSessionIdentity {
  return Object.freeze({
    backend: requireIdentityPart(identity.backend, 'output backend', MAXIMUM_OUTPUT_IDENTITY_BYTES),
    outputSessionId: requireIdentityPart(
      identity.outputSessionId,
      'output session',
      MAXIMUM_OUTPUT_IDENTITY_BYTES,
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
  // The synthetic root is represented by an empty path and is admitted in
  // memory; ordinary output paths still use the stricter output path policy.
  const path = directory.path.length === 0 ? Object.freeze([]) : snapshotOutputPath(directory.path)
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
  const modifiedTime = snapshotModifiedTimeFields(directory)
  return Object.freeze({
    directoryId: requireOpaqueIdentity(directory.directoryId, CATALOG_IDENTITY_BYTES, 'directory'),
    generation: requireOpaqueIdentity(directory.generation, CATALOG_IDENTITY_BYTES, 'directory generation'),
    path,
    ...(parentAdmission === undefined
      ? {}
      : { parentAdmission }),
    ...modifiedTime,
  })
}

export function snapshotDirectoryAdmission(admission: DirectoryAdmission): DirectoryAdmission {
  const path = admission.path.length === 0 ? Object.freeze([]) : snapshotOutputPath(admission.path)
  if (path.length === 0 && admission.parentToken !== undefined) {
    throw new DirectoryAdmissionBindingError('synthetic root proof must not have a parent token')
  }
  if (path.length > 0 && admission.parentToken === undefined) {
    throw new DirectoryAdmissionBindingError('child directory proof requires a parent token')
  }
  const modifiedTime = snapshotModifiedTimeFields(admission)
  return Object.freeze({
    token: requireOpaqueIdentity(admission.token, DIRECTORY_ADMISSION_TOKEN_BYTES, 'directory admission'),
    directoryId: requireOpaqueIdentity(admission.directoryId, CATALOG_IDENTITY_BYTES, 'directory'),
    generation: requireOpaqueIdentity(admission.generation, CATALOG_IDENTITY_BYTES, 'directory generation'),
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
 * Creates the session-scoped secret used by the admission codec.  The secret is
 * retained only by the output backend's admission ledger; callers receive no derivation
 * authority unless they explicitly inject a test secret.
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

/**
 * Returns the exact bytes hashed by Go's NewDirectoryAdmissionWithSecret.
 * Keeping this codec in the production transfer module lets vector tests and
 * backend ledgers exercise the same framing rather than a test-only duplicate.
 */
export function canonicalDirectoryAdmissionBytes(
  secret: Uint8Array<ArrayBufferLike>,
  input: OutputDirectoryAdmission,
): Uint8Array<ArrayBuffer> {
  const request = snapshotOutputDirectoryAdmission(input)
  const secretBytes = requireRawBytes(secret, DIRECTORY_ADMISSION_SECRET_BYTES, 'directory admission secret')
  const directoryId = requireOpaqueBytes(request.directoryId, CATALOG_IDENTITY_BYTES, 'directory')
  const generation = requireOpaqueBytes(request.generation, CATALOG_IDENTITY_BYTES, 'directory generation')
  const path = new TextEncoder().encode(request.path.join('/'))
  const parent = request.parentAdmission === undefined
    ? undefined
    : requireOpaqueBytes(
        request.parentAdmission.token,
        DIRECTORY_ADMISSION_TOKEN_BYTES,
        'parent admission',
      )
  const modified = canonicalModifiedTimeBytes(request.modifiedTime)
  return concatOutputBytes([
    DIRECTORY_ADMISSION_DOMAIN,
    secretBytes,
    directoryId,
    generation,
    path,
    modified,
    ...(parent === undefined ? [] : [parent]),
  ])
}

/** Derives the URL-safe, unpadded 32-byte admission token used by Go. */
export async function deriveDirectoryAdmissionToken(
  secret: Uint8Array<ArrayBufferLike>,
  input: OutputDirectoryAdmission,
): Promise<string> {
  return encodeBase64Url(await sha256(canonicalDirectoryAdmissionBytes(secret, input)))
}

/**
 * Mints a complete admission proof.  Production wrappers pass their private
 * per-session secret; tests and protocol vectors may inject a deterministic one.
 */
export async function createDirectoryAdmission(
  input: OutputDirectoryAdmission,
  secret: Uint8Array<ArrayBufferLike> = createDirectoryAdmissionSecret(),
): Promise<DirectoryAdmission> {
  const request = snapshotOutputDirectoryAdmission(input)
  const token = await deriveDirectoryAdmissionToken(secret, request)
  return Object.freeze({
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

/**
 * Treats a backend response as untrusted until every committed-generation
 * binding is echoed exactly. A token alone is insufficient because an adapter
 * could accidentally return a proof minted for another directory or metadata
 * version.
 */
export function validateDirectoryAdmissionBinding(
  input: OutputDirectoryAdmission,
  admission: DirectoryAdmission,
): DirectoryAdmission {
  const request = snapshotOutputDirectoryAdmission(input)
  let proof: DirectoryAdmission
  try {
    proof = snapshotDirectoryAdmission(admission)
  } catch (cause) {
    throw new DirectoryAdmissionBindingError('output backend returned a malformed directory admission', { cause })
  }
  if (proof.directoryId !== request.directoryId ||
      proof.generation !== request.generation ||
      !sameOutputPath(proof.path, request.path) ||
      proof.parentToken !== request.parentAdmission?.token ||
      !sameModifiedTime(proof, request)) {
    throw new DirectoryAdmissionBindingError(
      'output backend returned a directory admission for a different committed generation',
    )
  }
  return proof
}

export function sameModifiedTime(left: ModifiedTimeCarrier, right: ModifiedTimeCarrier): boolean {
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

export function snapshotOutputFile(file: OutputFile): OutputFile {
  if (file.exactSize < 0n) {
    throw new RangeError('output file size must not be negative')
  }
  const modifiedTime = snapshotModifiedTimeFields(file)
  return Object.freeze({
    source: snapshotOutputSource(file.source),
    path: snapshotOutputPath(file.path),
    exactSize: file.exactSize,
    ...(file.parentAdmission === undefined ? {} : { parentAdmission: snapshotDirectoryAdmission(file.parentAdmission) }),
    ...modifiedTime,
  })
}

function snapshotOutputSource(source: OutputSourceIdentity): OutputSourceIdentity {
  return Object.freeze({
    shareInstance: requireSourceIdentity(source.shareInstance, 'shareInstance'),
    fileId: requireSourceIdentity(source.fileId, 'fileId'),
    fileRevision: requireSourceIdentity(source.fileRevision, 'fileRevision'),
  })
}

function snapshotOutputOwnership(ownership: OutputFileOwnership): OutputFileOwnership {
  const identity = outputSessionIdentity(ownership)
  return Object.freeze({
    ...identity,
    canonicalPath: snapshotOutputPath(ownership.canonicalPath),
    ownedFileIdentity: requireIdentityPart(
      ownership.ownedFileIdentity,
      'owned output file',
      MAXIMUM_OWNED_FILE_IDENTITY_BYTES,
    ),
  })
}

function requireIdentityPart(value: string, label: string, maximumBytes: number): string {
  if (typeof value !== 'string' || value.length === 0 ||
      new TextEncoder().encode(value).byteLength > maximumBytes) {
    throw new TypeError(`${label} identity must contain at most ${maximumBytes} bytes`)
  }
  return value
}

function requireSourceIdentity(value: string, label: string): string {
  const decoded = decodeBase64Url(value)
  if (decoded === undefined || decoded.byteLength !== CATALOG_IDENTITY_BYTES ||
      decoded.every((byte) => byte === 0) || encodeBase64Url(decoded) !== value) {
    throw new OutputSessionBindingError(
      `output source ${label} must be a canonical non-zero ${CATALOG_IDENTITY_BYTES}-byte identity`,
    )
  }
  return value
}

function requireOpaqueIdentity(value: string, byteLength: number, label: string): string {
  const decoded = decodeBase64Url(value)
  if (decoded === undefined || decoded.byteLength !== byteLength || decoded.every((byte) => byte === 0) ||
      encodeBase64Url(decoded) !== value) {
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
    throw new DirectoryAdmissionBindingError(
      `${label} must be exactly ${byteLength} bytes`,
    )
  }
  return Uint8Array.from(value)
}

function requireOpaqueBytes(value: string, byteLength: number, label: string): Uint8Array<ArrayBuffer> {
  const decoded = decodeBase64Url(value)
  if (decoded === undefined || decoded.byteLength !== byteLength || decoded.every((byte) => byte === 0) ||
      encodeBase64Url(decoded) !== value) {
    throw new DirectoryAdmissionBindingError(
      `${label} must be a non-zero ${byteLength}-byte base64url identity`,
    )
  }
  return Uint8Array.from(decoded)
}

function canonicalModifiedTimeBytes(modifiedTime: OutputModifiedTime | undefined): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(1 + 8 + 4 + 1)
  const view = new DataView(bytes.buffer)
  if (modifiedTime === undefined) return bytes
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

export function snapshotOutputPath(path: readonly string[]): readonly string[] {
  return snapshotPortableCatalogPath(path)
}

function verifiedDurableRangeSet(fileSize: bigint, ranges: readonly ByteRange[]): ByteRangeSet {
  if (!Array.isArray(ranges) || ranges.length > MAXIMUM_VERIFIED_DURABLE_RANGES) {
    throw new OutputSessionBindingError('output durable ranges exceed their canonical count limit')
  }
  let previousEnd: bigint | undefined
  for (const range of ranges) {
    if (typeof range?.start !== 'bigint' || typeof range.end !== 'bigint' ||
        range.start < 0n || range.start >= range.end || range.end > fileSize ||
        (previousEnd !== undefined && range.start <= previousEnd)) {
      throw new OutputSessionBindingError(
        'output durable ranges must be sorted, non-overlapping, non-adjacent, and within the file',
      )
    }
    previousEnd = range.end
  }
  return new ByteRangeSet(fileSize, ranges)
}
