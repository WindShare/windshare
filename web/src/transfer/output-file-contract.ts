import { ByteRangeSet, type ByteRange } from '../content/geometry'
import { decodeBase64Url, encodeBase64Url } from '../crypto/bytes'
import {
  MAX_MATERIALIZATION_PATH_BYTES,
  snapshotCanonicalModifiedTime,
  snapshotDirectoryAdmission,
  snapshotMaterializationPath,
  type CanonicalModifiedTime,
  type DirectoryAdmission,
} from './directory-admission'
import {
  snapshotLogicalArtifactPath,
  snapshotMaterializationRootRelativePath,
  snapshotSourceAuthenticationPath,
  type DirectTreeCoordinateContract,
  type LogicalArtifactPath,
  type MaterializationRootRelativePath,
  type SourceAuthenticationPath,
} from './job/coordinate/direct-tree'
import type { PerformanceFilePipelineObservation } from '../output/diagnostics/performance-runtime-observations'

export type DurabilityLevel = 'None' | 'ProcessRestart' | 'PowerLoss'
export type FileRetirementDisposition = 'FileIsolated' | 'JobOutputCompromised'

export const MAXIMUM_OPEN_OUTPUT_FILES = 32
export const MAXIMUM_OUTPUT_IDENTITY_BYTES = 128
export const MAXIMUM_OWNED_FILE_IDENTITY_BYTES = MAX_MATERIALIZATION_PATH_BYTES + 256
export const MAXIMUM_VERIFIED_DURABLE_RANGES = 16_384
export const DEFAULT_OUTPUT_WRITE_BUDGET_BYTES = 8n * 1024n * 1024n
const OUTPUT_CATALOG_IDENTITY_BYTES = 16

export interface OutputCapabilities {
  readonly durability: DurabilityLevel
  readonly randomWrite: boolean
  readonly fileFailureIsolation: boolean
  readonly modificationTime: boolean
}

export interface OutputCheckpointCost {
  readonly prefixCopyBytes: bigint
  readonly cumulativeWriteAmplificationBytes: bigint
  readonly peakTemporaryBytes: bigint
}

export interface OutputCheckpointCostBudget {
  readonly maximumPrefixCopyBytes: bigint
  readonly maximumCumulativeWriteAmplificationBytes: bigint
  readonly maximumPeakTemporaryBytes: bigint
}

export interface OutputExecutionProfileBoundedCheckpoint {
  readonly kind: 'bounded'
  readonly trigger: Readonly<{
    readonly pendingBytes: bigint
    readonly pendingMilliseconds: number
  }>
  readonly costBudget: OutputCheckpointCostBudget
}

export interface OutputExecutionProfile {
  readonly maximumConcurrentFilePipelines: number
  readonly maximumOutstandingWriteBytes: bigint
  readonly maximumBufferedBytes: bigint
  readonly automaticCheckpoint:
    | Readonly<{ readonly kind: 'disabled' }>
    | OutputExecutionProfileBoundedCheckpoint
}

export type AutomaticCheckpointTrigger = 'pending-bytes' | 'pending-time'

export type AutomaticCheckpointResult =
  | Readonly<{
      readonly kind: 'advanced'
      readonly durable: VerifiedDurableRanges
      readonly cost: OutputCheckpointCost
    }>
  | Readonly<{
      readonly kind: 'declined'
      readonly reason:
        | 'prefix-copy-budget'
        | 'cumulative-write-amplification-budget'
        | 'peak-temporary-space-budget'
        | 'temporary-space-confirmation-required'
        | 'cost-evidence-unavailable'
      readonly estimate: OutputCheckpointCost
    }>

export interface OutputCatalogFileIdentity {
  readonly shareInstance: string
  readonly fileId: string
}

export interface OutputSourceIdentity extends OutputCatalogFileIdentity {
  readonly fileRevision: string
}

export interface OpenedOutputRevision extends OutputSourceIdentity {
  readonly exactSize: bigint
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

/**
 * The adapter must invoke openRevision and await it before creating or opening an
 * output object. This callback boundary keeps failed preparation/revision opens
 * from leaving placeholders in a destination namespace.
 */
export interface OutputFileRequest {
  readonly source: OutputCatalogFileIdentity
  readonly sourceAuthenticationPath: SourceAuthenticationPath
  readonly logicalArtifactPath: LogicalArtifactPath
  readonly materializationRelativePath: MaterializationRootRelativePath
  readonly expectedSize: bigint
  readonly parentAdmission?: DirectoryAdmission
  readonly modifiedTime?: CanonicalModifiedTime
  readonly performancePipeline?: PerformanceFilePipelineObservation
  readonly openRevision: (signal: AbortSignal) => Promise<OpenedOutputRevision>
}

export interface OutputFile {
  readonly source: OutputSourceIdentity
  readonly sourceAuthenticationPath: SourceAuthenticationPath
  readonly logicalArtifactPath: LogicalArtifactPath
  readonly materializationRelativePath: MaterializationRootRelativePath
  readonly exactSize: bigint
  readonly parentAdmission?: DirectoryAdmission
  readonly modifiedTime?: CanonicalModifiedTime
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

  get fileSize(): bigint { return this.#ranges.fileSize }
  get ranges(): readonly ByteRange[] { return this.#ranges.ranges }
  covers(range: ByteRange): boolean { return this.#ranges.covers(range) }
  asRangeSet(): ByteRangeSet { return new ByteRangeSet(this.fileSize, this.ranges) }
}

/** Terminal evidence is deliberately distinct from restart-range evidence. */
export class VerifiedFinalOutputFile {
  readonly ownership: OutputFileOwnership
  readonly source: OutputSourceIdentity
  readonly fileSize: bigint

  constructor(
    ownership: OutputFileOwnership,
    source: OutputSourceIdentity,
    fileSize: bigint,
  ) {
    if (typeof fileSize !== 'bigint' || fileSize < 0n) {
      throw new OutputSessionBindingError('final output file size must not be negative')
    }
    this.ownership = snapshotOutputOwnership(ownership)
    this.source = snapshotOutputSource(source)
    this.fileSize = fileSize
    Object.freeze(this)
  }
}

export interface OutputFileTransaction {
  writeRange(offset: bigint, data: Uint8Array, signal: AbortSignal): Promise<void>
  automaticCheckpoint(
    trigger: AutomaticCheckpointTrigger,
    budget: OutputCheckpointCostBudget,
    signal: AbortSignal,
  ): Promise<AutomaticCheckpointResult>
  commit(signal: AbortSignal): Promise<VerifiedFinalOutputFile>
  /** A streaming backend may still retire a failed member when no bytes were emitted. */
  retire(reason: unknown): Promise<FileRetirementDisposition>
  /** Durable output must include every accepted pending range before pause resolves. */
  pause(reason: unknown): Promise<VerifiedDurableRanges>
}

export interface BeginOutputFileResult {
  readonly revision: OpenedOutputRevision
  readonly transaction: OutputFileTransaction
  readonly durableRanges: VerifiedDurableRanges
}

/** A materializer owns bytes and checkpoints only; artifact semantics stay in the plan execution. */
export interface OutputSession {
  readonly identity: OutputSessionIdentity
  readonly capabilities: OutputCapabilities
  readonly executionProfile: OutputExecutionProfile
  beginFile(file: OutputFileRequest, signal: AbortSignal): Promise<BeginOutputFileResult>
}

export class OutputSessionBindingError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'OutputSessionBindingError'
  }
}

/** A finite output-operation budget was exhausted before accepting more state. */
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

export function outputCapabilities(capabilities: OutputCapabilities): OutputCapabilities {
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

export function outputExecutionProfile(profile: OutputExecutionProfile): OutputExecutionProfile {
  if (!Number.isInteger(profile?.maximumConcurrentFilePipelines) ||
      profile.maximumConcurrentFilePipelines <= 0 ||
      profile.maximumConcurrentFilePipelines > MAXIMUM_OPEN_OUTPUT_FILES) {
    throw new OutputSessionBindingError(
      'output execution profile reported an invalid concurrent file-pipeline limit',
    )
  }
  requirePositiveBudget(profile.maximumOutstandingWriteBytes, 'outstanding write')
  requirePositiveBudget(profile.maximumBufferedBytes, 'buffered output')
  const checkpoint = profile.automaticCheckpoint
  if (checkpoint?.kind === 'disabled') {
    return Object.freeze({
      maximumConcurrentFilePipelines: profile.maximumConcurrentFilePipelines,
      maximumOutstandingWriteBytes: profile.maximumOutstandingWriteBytes,
      maximumBufferedBytes: profile.maximumBufferedBytes,
      automaticCheckpoint: Object.freeze({ kind: 'disabled' as const }),
    })
  }
  if (checkpoint?.kind !== 'bounded' ||
      typeof checkpoint.trigger?.pendingBytes !== 'bigint' || checkpoint.trigger.pendingBytes <= 0n ||
      !Number.isSafeInteger(checkpoint.trigger.pendingMilliseconds) ||
      checkpoint.trigger.pendingMilliseconds <= 0) {
    throw new OutputSessionBindingError('output execution profile reported an invalid checkpoint trigger')
  }
  const costBudget = outputCheckpointCostBudget(checkpoint.costBudget)
  return Object.freeze({
    maximumConcurrentFilePipelines: profile.maximumConcurrentFilePipelines,
    maximumOutstandingWriteBytes: profile.maximumOutstandingWriteBytes,
    maximumBufferedBytes: profile.maximumBufferedBytes,
    automaticCheckpoint: Object.freeze({
      kind: 'bounded' as const,
      trigger: Object.freeze({ ...checkpoint.trigger }),
      costBudget,
    }),
  })
}

export function outputCheckpointCost(cost: OutputCheckpointCost): OutputCheckpointCost {
  return Object.freeze({
    prefixCopyBytes: requireNonNegativeCost(cost?.prefixCopyBytes, 'prefix copy'),
    cumulativeWriteAmplificationBytes: requireNonNegativeCost(
      cost?.cumulativeWriteAmplificationBytes,
      'cumulative write amplification',
    ),
    peakTemporaryBytes: requireNonNegativeCost(cost?.peakTemporaryBytes, 'peak temporary space'),
  })
}

export function disabledOutputExecutionProfile(
  maximumConcurrentFilePipelines: number,
): OutputExecutionProfile {
  return outputExecutionProfile({
    maximumConcurrentFilePipelines,
    maximumOutstandingWriteBytes: DEFAULT_OUTPUT_WRITE_BUDGET_BYTES,
    maximumBufferedBytes: DEFAULT_OUTPUT_WRITE_BUDGET_BYTES,
    automaticCheckpoint: { kind: 'disabled' },
  })
}

export function outputCheckpointCostBudget(
  budget: OutputCheckpointCostBudget,
): OutputCheckpointCostBudget {
  return Object.freeze({
    maximumPrefixCopyBytes: requireNonNegativeCost(
      budget?.maximumPrefixCopyBytes,
      'maximum prefix copy',
    ),
    maximumCumulativeWriteAmplificationBytes: requireNonNegativeCost(
      budget?.maximumCumulativeWriteAmplificationBytes,
      'maximum cumulative write amplification',
    ),
    maximumPeakTemporaryBytes: requireNonNegativeCost(
      budget?.maximumPeakTemporaryBytes,
      'maximum peak temporary space',
    ),
  })
}

function requirePositiveBudget(value: bigint, label: string): bigint {
  if (typeof value !== 'bigint' || value <= 0n) {
    throw new OutputSessionBindingError(`${label} budget must be positive`)
  }
  return value
}

function requireNonNegativeCost(value: bigint, label: string): bigint {
  if (typeof value !== 'bigint' || value < 0n) {
    throw new OutputSessionBindingError(`${label} cost must not be negative`)
  }
  return value
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

export function snapshotOutputFileRequest(
  file: OutputFileRequest,
  directTreeCoordinates?: DirectTreeCoordinateContract,
): OutputFileRequest {
  if (typeof file.expectedSize !== 'bigint' || file.expectedSize < 0n) {
    throw new RangeError('expected output file size must not be negative')
  }
  if (typeof file.openRevision !== 'function') {
    throw new TypeError('output file request requires an authenticated revision callback')
  }
  const sourceAuthenticationPath = snapshotSourceAuthenticationPath(file.sourceAuthenticationPath)
  const logicalArtifactPath = snapshotLogicalArtifactPath(file.logicalArtifactPath)
  const materializationRelativePath = snapshotMaterializationRootRelativePath(
    file.materializationRelativePath,
  )
  if (sourceAuthenticationPath.length === 0 ||
      logicalArtifactPath.length === 0 ||
      !materializationCoordinateIdentifiesFile(
        file.source,
        sourceAuthenticationPath,
        logicalArtifactPath,
        materializationRelativePath,
        directTreeCoordinates,
      )) {
    throw new TypeError('output file coordinates must identify a file')
  }
  return Object.freeze({
    source: snapshotOutputCatalogFileIdentity(file.source),
    sourceAuthenticationPath,
    logicalArtifactPath,
    materializationRelativePath,
    expectedSize: file.expectedSize,
    ...(file.parentAdmission === undefined
      ? {}
      : { parentAdmission: snapshotDirectoryAdmission(file.parentAdmission) }),
    ...(file.modifiedTime === undefined
      ? {}
      : { modifiedTime: snapshotCanonicalModifiedTime(file.modifiedTime) }),
    ...(file.performancePipeline === undefined
      ? {}
      : { performancePipeline: file.performancePipeline }),
    openRevision: file.openRevision,
  })
}

export function snapshotOutputFile(
  file: OutputFile,
  directTreeCoordinates?: DirectTreeCoordinateContract,
): OutputFile {
  if (typeof file.exactSize !== 'bigint' || file.exactSize < 0n) {
    throw new RangeError('output file size must not be negative')
  }
  const sourceAuthenticationPath = snapshotSourceAuthenticationPath(file.sourceAuthenticationPath)
  const logicalArtifactPath = snapshotLogicalArtifactPath(file.logicalArtifactPath)
  const materializationRelativePath = snapshotMaterializationRootRelativePath(
    file.materializationRelativePath,
  )
  if (sourceAuthenticationPath.length === 0 ||
      logicalArtifactPath.length === 0 ||
      !materializationCoordinateIdentifiesFile(
        file.source,
        sourceAuthenticationPath,
        logicalArtifactPath,
        materializationRelativePath,
        directTreeCoordinates,
      )) {
    throw new TypeError('output file coordinates must identify a file')
  }
  return Object.freeze({
    source: snapshotOutputSource(file.source),
    sourceAuthenticationPath,
    logicalArtifactPath,
    materializationRelativePath,
    exactSize: file.exactSize,
    ...(file.parentAdmission === undefined
      ? {}
      : { parentAdmission: snapshotDirectoryAdmission(file.parentAdmission) }),
    ...(file.modifiedTime === undefined
      ? {}
      : { modifiedTime: snapshotCanonicalModifiedTime(file.modifiedTime) }),
  })
}

// An FSA named-file reservation makes the file itself the materialization root.
// Requiring the validated projector here keeps [] unavailable to portable and
// native outputs, where an empty coordinate would mean a missing file name.
function materializationCoordinateIdentifiesFile(
  source: OutputCatalogFileIdentity,
  sourceAuthenticationPath: SourceAuthenticationPath,
  logicalArtifactPath: LogicalArtifactPath,
  materializationRelativePath: MaterializationRootRelativePath,
  directTreeCoordinates: DirectTreeCoordinateContract | undefined,
): boolean {
  if (materializationRelativePath.length !== 0) return true
  if (directTreeCoordinates?.coordinate !== 'fsa-reserved-root-relative' ||
      directTreeCoordinates.intent.artifact.layout.kind !== 'single-file' ||
      source.shareInstance !== directTreeCoordinates.intent.shareInstance ||
      source.fileId !== directTreeCoordinates.intent.artifact.layout.fileId) {
    return false
  }
  const projection = directTreeCoordinates.projectFile(sourceAuthenticationPath)
  return projection.relativePath.length === 0 &&
    sameCoordinate(projection.logicalArtifactPath, logicalArtifactPath)
}

function sameCoordinate(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((segment, index) => segment === right[index])
}

export function snapshotOpenedOutputRevision(revision: OpenedOutputRevision): OpenedOutputRevision {
  if (typeof revision.exactSize !== 'bigint' || revision.exactSize < 0n) {
    throw new TypeError('opened output revision size is invalid')
  }
  return Object.freeze({
    ...snapshotOutputSource(revision),
    exactSize: revision.exactSize,
  })
}

function snapshotOutputCatalogFileIdentity(source: OutputCatalogFileIdentity): OutputCatalogFileIdentity {
  return Object.freeze({
    shareInstance: requireSourceIdentity(source.shareInstance, 'shareInstance'),
    fileId: requireSourceIdentity(source.fileId, 'fileId'),
  })
}

function snapshotOutputSource(source: OutputSourceIdentity): OutputSourceIdentity {
  return Object.freeze({
    ...snapshotOutputCatalogFileIdentity(source),
    fileRevision: requireSourceIdentity(source.fileRevision, 'fileRevision'),
  })
}

function snapshotOutputOwnership(ownership: OutputFileOwnership): OutputFileOwnership {
  const identity = outputSessionIdentity(ownership)
  return Object.freeze({
    ...identity,
    canonicalPath: snapshotMaterializationPath(ownership.canonicalPath),
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
