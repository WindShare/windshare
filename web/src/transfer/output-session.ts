import { ByteRangeSet, type ByteRange } from '../content/geometry'
import { decodeBase64Url, encodeBase64Url } from '../crypto/bytes'
import {
  FaultScope,
  isFault,
  type Fault,
} from './fault'
import type { JobOutcome } from './outcome'
import type { TransferIntent, TransferIntentDraft } from './intent'
import {
  MAXIMUM_OUTPUT_PATH_BYTES,
  OUTPUT_CATALOG_IDENTITY_BYTES,
  snapshotDirectoryAdmission,
  snapshotOutputModifiedTimeFields,
  snapshotOutputPath,
  type DirectoryAdmission,
  type DirectorySettlement,
  type OutputDirectoryAdmission,
  type OutputModifiedTime,
} from './directory-admission'

export {
  DIRECTORY_ADMISSION_SCHEMA_VERSION,
  DIRECTORY_ADMISSION_SECRET_BYTES,
  DIRECTORY_ADMISSION_TOKEN_BYTES,
  DirectoryAdmissionBindingError,
  DirectorySettlementKind,
  MAXIMUM_OUTPUT_PATH_BYTES,
  MAXIMUM_OUTPUT_PATH_SEGMENTS,
  MAXIMUM_OUTPUT_SEGMENT_BYTES,
  canonicalDirectoryAdmissionMessageV1,
  createDirectoryAdmission,
  createDirectoryAdmissionSecret,
  deriveDirectoryAdmissionToken,
  directoryAdmissionScope,
  finalizedDirectorySettlement,
  isImmediateChildPath,
  isolatedDirectorySettlement,
  sameDirectoryAdmission,
  sameDirectoryAdmissionToken,
  sameModifiedTime,
  sameOutputPath,
  snapshotDirectoryAdmission,
  snapshotDirectoryAdmissionScope,
  snapshotOutputDirectory,
  snapshotOutputDirectoryAdmission,
  snapshotOutputPath,
  validateDirectoryAdmissionBinding,
  validateDirectorySettlement,
  verifyDirectoryAdmissionToken,
} from './directory-admission'
export type {
  DirectoryAdmission,
  DirectoryAdmissionScope,
  DirectorySettlement,
  OutputDirectory,
  OutputDirectoryAdmission,
  OutputModifiedTime,
} from './directory-admission'

export type DurabilityLevel = 'None' | 'ProcessRestart' | 'PowerLoss'
export type FileRetirementDisposition = 'FileIsolated' | 'JobOutputCompromised'

export const MAXIMUM_OPEN_OUTPUT_FILES = 32
export const MAXIMUM_OUTPUT_IDENTITY_BYTES = 128
export const MAXIMUM_OWNED_FILE_IDENTITY_BYTES = MAXIMUM_OUTPUT_PATH_BYTES + 256
export const MAXIMUM_VERIFIED_DURABLE_RANGES = 16_384

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
  /** A streaming backend may still retire a failed member when no bytes were emitted. */
  retire(reason: unknown): Promise<FileRetirementDisposition>
  /** Reaches the backend's stable resumable cut without deleting owned output. */
  pause(reason: unknown): Promise<void>
}

export interface BeginOutputFileResult {
  readonly transaction: OutputFileTransaction
  readonly durableRanges: VerifiedDurableRanges
}

export const JobSettlementKind = {
  Completed: 'Completed',
  Paused: 'Paused',
  NeedsAttention: 'NeedsAttention',
} as const

export type JobSettlement =
  | Readonly<{ kind: typeof JobSettlementKind.Completed }>
  | Readonly<{
      kind: typeof JobSettlementKind.Paused
      durability: DurabilityLevel
    }>
  | Readonly<{
      kind: typeof JobSettlementKind.NeedsAttention
      fault: Fault
    }>

export const COMPLETED_JOB_SETTLEMENT: JobSettlement = Object.freeze({
  kind: JobSettlementKind.Completed,
})

export function pausedJobSettlement(durability: DurabilityLevel): JobSettlement {
  if (durability !== 'None' && durability !== 'ProcessRestart' && durability !== 'PowerLoss') {
    throw new TypeError('paused job settlement durability is invalid')
  }
  return Object.freeze({ kind: JobSettlementKind.Paused, durability })
}

export function needsAttentionJobSettlement(fault: Fault): JobSettlement {
  if (!isFault(fault) ||
      (fault.scope !== FaultScope.OutputPause && fault.scope !== FaultScope.SessionTerminal)) {
    throw new TypeError('needs-attention settlement requires a job-scoped fault')
  }
  return Object.freeze({ kind: JobSettlementKind.NeedsAttention, fault: Object.freeze({ ...fault }) })
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
  /** Seals the exact receipt, including synthetic root; settled retries return the cached value. */
  finalizeDirectory(admission: DirectoryAdmission, signal: AbortSignal): Promise<DirectorySettlement>
  /** Rejection must leave no unowned partial output because no transaction is returned. */
  beginFile(file: OutputFile, signal: AbortSignal): Promise<BeginOutputFileResult>
  /**
   * The session retains settlement ownership of every returned transaction until
   * file settlement or job settlement. A terminal transfer fault stops invoking
   * that transaction; PauseJob must then settle it without a race.
   */
  completeJob(outcome: JobOutcome, signal: AbortSignal): Promise<JobSettlement>
  /** Releases live resources at a stable cut and never acquires deletion authority. */
  pauseJob(reason: unknown): Promise<JobSettlement>
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

export class TransferPauseRequestedError extends Error {
  constructor(message = 'Transfer paused by receiver') {
    super(message)
    this.name = 'TransferPauseRequestedError'
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

export function snapshotOutputFile(file: OutputFile): OutputFile {
  if (file.exactSize < 0n) {
    throw new RangeError('output file size must not be negative')
  }
  const modifiedTime = snapshotOutputModifiedTimeFields(file)
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
  if (decoded === undefined || decoded.byteLength !== OUTPUT_CATALOG_IDENTITY_BYTES ||
      decoded.every((byte) => byte === 0) || encodeBase64Url(decoded) !== value) {
    throw new OutputSessionBindingError(
      `output source ${label} must be a canonical non-zero ${OUTPUT_CATALOG_IDENTITY_BYTES}-byte identity`,
    )
  }
  return value
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
