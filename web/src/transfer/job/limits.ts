import {
  MAXIMUM_CATALOG_NODE_CLAIMS,
  MAXIMUM_CONCURRENT_DIRECTORIES,
  MAXIMUM_CONCURRENT_FILES,
  MAXIMUM_DIRECTORY_ADMISSIONS,
  MAXIMUM_PENDING_FILES,
  MAXIMUM_PENDING_FILE_METADATA_BYTES,
  type TransferJobOptions,
} from './contract'
import {
  outputExecutionProfile,
  type OutputExecutionProfile,
} from '../output/file'

import {
  MAXIMUM_PENDING_GENERATIONS,
  MAXIMUM_PENDING_GENERATION_METADATA_BYTES,
} from '../discovery/directory-replay'

export interface TransferJobLimits {
  readonly requestedConcurrentFiles?: number
  readonly pendingGenerations: number
  readonly pendingGenerationMetadataBytes: bigint
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
    MAXIMUM_CONCURRENT_FILES,
    'v2 transfer file concurrency exceeds its absolute safety limit',
  )
  const concurrentDirectories = boundedInteger(
    options.maximumConcurrentDirectories,
    MAXIMUM_CONCURRENT_DIRECTORIES,
    'v2 transfer directory concurrency exceeds its catalog-safe limit',
  )
  const pendingFiles = boundedInteger(
    options.maximumPendingFiles,
    MAXIMUM_PENDING_FILES,
    'v2 transfer pending-file queue exceeds its admission limit',
  )
  const pendingFileMetadataBytes = options.maximumPendingFileMetadataBytes ??
    MAXIMUM_PENDING_FILE_METADATA_BYTES
  if (pendingFileMetadataBytes <= 0n || pendingFileMetadataBytes > MAXIMUM_PENDING_FILE_METADATA_BYTES) {
    throw new RangeError('v2 transfer pending-file metadata queue exceeds its admission limit')
  }
  const pendingGenerations = boundedInteger(
    options.maximumPendingGenerations,
    MAXIMUM_PENDING_GENERATIONS,
    'v2 transfer pending-generation queue exceeds its admission limit',
  )
  const pendingGenerationMetadataBytes = options.maximumPendingGenerationMetadataBytes ??
    MAXIMUM_PENDING_GENERATION_METADATA_BYTES
  if (pendingGenerationMetadataBytes <= 0n ||
      pendingGenerationMetadataBytes > MAXIMUM_PENDING_GENERATION_METADATA_BYTES) {
    throw new RangeError('v2 transfer pending-generation metadata exceeds its admission limit')
  }
  const catalogNodeClaims = boundedInteger(
    options.maximumNodeClaims,
    MAXIMUM_CATALOG_NODE_CLAIMS,
    'v2 transfer catalog-node budget exceeds its identity limit',
  )
  const directoryAdmissions = boundedInteger(
    options.maximumDirectoryAdmissions,
    MAXIMUM_DIRECTORY_ADMISSIONS,
    'v2 transfer directory-admission budget exceeds its authority limit',
  )
  return Object.freeze({
    ...(requestedConcurrentFiles === undefined ? {} : { requestedConcurrentFiles }),
    concurrentDirectories,
    pendingGenerations,
    pendingGenerationMetadataBytes,
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
    limits.requestedConcurrentFiles ?? MAXIMUM_CONCURRENT_FILES,
    validated.maximumConcurrentFilePipelines,
    MAXIMUM_CONCURRENT_FILES,
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
