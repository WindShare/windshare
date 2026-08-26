import type { PersistentPausedFileRecovery } from '../../output/persistent-tree/contracts'

/** The explicit pause action is the complete recovery policy for an attempt. */
export type PersistentExecutionRecoveryPolicy = Readonly<{
  readonly pausedFile: PersistentPausedFileRecovery
}>
