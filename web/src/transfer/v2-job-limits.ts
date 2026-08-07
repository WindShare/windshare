import {
  V2_MAXIMUM_CATALOG_NODE_CLAIMS,
  V2_MAXIMUM_CONCURRENT_DIRECTORIES,
  V2_MAXIMUM_CONCURRENT_FILES,
  V2_MAXIMUM_DIRECTORY_ADMISSIONS,
  V2_MAXIMUM_PENDING_FILES,
  V2_MAXIMUM_PENDING_FILE_METADATA_BYTES,
  type TransferJobOptions,
} from './v2-job-contract'

export interface TransferJobLimits {
  readonly concurrentFiles: number
  readonly concurrentDirectories: number
  readonly pendingFiles: number
  readonly pendingFileMetadataBytes: bigint
  readonly catalogNodeClaims: number
  readonly directoryAdmissions: number
}

export function transferJobLimits(options: TransferJobOptions): TransferJobLimits {
  const concurrentFiles = boundedInteger(
    options.maximumConcurrentFiles,
    V2_MAXIMUM_CONCURRENT_FILES,
    'v2 transfer file concurrency exceeds its output-safe limit',
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
    concurrentFiles,
    concurrentDirectories,
    pendingFiles,
    pendingFileMetadataBytes,
    catalogNodeClaims,
    directoryAdmissions,
  })
}

function boundedInteger(value: number | undefined, maximum: number, message: string): number {
  const bounded = value ?? maximum
  if (!Number.isSafeInteger(bounded) || bounded <= 0 || bounded > maximum) {
    throw new RangeError(message)
  }
  return bounded
}
