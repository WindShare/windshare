import {
  V2_MAXIMUM_CATALOG_NODE_CLAIMS,
  V2_MAXIMUM_CONCURRENT_DIRECTORIES,
  V2_MAXIMUM_CONCURRENT_FILES,
  V2_MAXIMUM_DIRECTORY_ADMISSIONS,
  V2_MAXIMUM_PENDING_FILES,
  V2_MAXIMUM_PENDING_FILE_METADATA_BYTES,
  type TransferJobOptions,
} from './contract'
import {
  outputExecutionProfile,
  type OutputExecutionProfile,
} from '../output-file-contract'

export interface TransferJobLimits {
  readonly requestedConcurrentFiles?: number
  readonly concurrentDirectories: number
  readonly pendingFiles: number
  readonly pendingFileMetadataBytes: bigint
  readonly catalogNodeClaims: number
  readonly directoryAdmissions: number
}

export interface TransferExecutionLimits {
  readonly concurrentFiles: number
  readonly maximumOutstandingWriteBytes: bigint
  readonly maximumBufferedBytes: bigint
}

export function transferJobLimits(options: TransferJobOptions): TransferJobLimits {
  const requestedConcurrentFiles = optionalBoundedInteger(
    options.maximumConcurrentFiles,
    V2_MAXIMUM_CONCURRENT_FILES,
    'v2 transfer file concurrency exceeds its absolute safety limit',
  )
  const concurrentDirectories = boundedInteger(
    options.maximumConcurrentDirectories,
    V2_MAXIMUM_CONCURRENT_DIRECTORIES,
    'v2 transfer directory concurrency exceeds its catalog-safe limit',
  )
  const pendingFiles = boundedInteger(
    options.maximumPendingFiles,
    V2_MAXIMUM_PENDING_FILES,
    'v2 transfer pending-file queue exceeds its admission limit',
  )
  const pendingFileMetadataBytes = options.maximumPendingFileMetadataBytes ??
    V2_MAXIMUM_PENDING_FILE_METADATA_BYTES
  if (pendingFileMetadataBytes <= 0n || pendingFileMetadataBytes > V2_MAXIMUM_PENDING_FILE_METADATA_BYTES) {
    throw new RangeError('v2 transfer pending-file metadata queue exceeds its admission limit')
  }
  const catalogNodeClaims = boundedInteger(
    options.maximumNodeClaims,
    V2_MAXIMUM_CATALOG_NODE_CLAIMS,
    'v2 transfer catalog-node budget exceeds its identity limit',
  )
  const directoryAdmissions = boundedInteger(
    options.maximumDirectoryAdmissions,
    V2_MAXIMUM_DIRECTORY_ADMISSIONS,
    'v2 transfer directory-admission budget exceeds its authority limit',
  )
  return Object.freeze({
    ...(requestedConcurrentFiles === undefined ? {} : { requestedConcurrentFiles }),
    concurrentDirectories,
    pendingFiles,
    pendingFileMetadataBytes,
    catalogNodeClaims,
    directoryAdmissions,
  })
}

/** Resolves output policy only after plan execution validation has established its authority. */
export function bindTransferExecutionLimits(
  limits: TransferJobLimits,
  profile: OutputExecutionProfile,
): TransferExecutionLimits {
  const validated = outputExecutionProfile(profile)
  const concurrentFiles = Math.min(
    limits.requestedConcurrentFiles ?? V2_MAXIMUM_CONCURRENT_FILES,
    validated.maximumConcurrentFilePipelines,
    V2_MAXIMUM_CONCURRENT_FILES,
  )
  return Object.freeze({
    concurrentFiles,
    maximumOutstandingWriteBytes: validated.maximumOutstandingWriteBytes,
    maximumBufferedBytes: validated.maximumBufferedBytes,
  })
}

function boundedInteger(value: number | undefined, maximum: number, message: string): number {
  const bounded = value ?? maximum
  if (!Number.isSafeInteger(bounded) || bounded <= 0 || bounded > maximum) {
    throw new RangeError(message)
  }
  return bounded
}

function optionalBoundedInteger(
  value: number | undefined,
  maximum: number,
  message: string,
): number | undefined {
  if (value === undefined) return undefined
  return boundedInteger(value, maximum, message)
}
