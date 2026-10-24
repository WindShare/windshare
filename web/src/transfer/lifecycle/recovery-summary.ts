/** Verified recovery costs; only the output authority can derive these from committed checkpoints. */

export interface RecoverySummary {
  readonly lifecycleGeneration: bigint
  readonly checkpointSetDigest: string
  readonly discoveredFileCount: bigint
  readonly discoveredBytes: bigint
  readonly discovery: 'complete' | 'known-so-far'
  readonly completedFileCount: bigint
  readonly completedBytes: bigint
  readonly incompleteFileCount: bigint
  readonly verifiedPartialFileCount: bigint
  readonly verifiedPartialBytes: bigint
  readonly unstartedFileCount: bigint
  readonly unstartedBytes: bigint
  readonly preservingRemainingBytes: bigint
  readonly restartRemainingBytes: bigint
  readonly restartRedownloadBytes: bigint
  readonly maximumPreservingTemporaryBytes: bigint
}
