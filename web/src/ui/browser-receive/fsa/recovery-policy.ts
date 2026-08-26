import type {
  PersistentExecutionRecoveryPolicy,
} from '../../../transfer/settlement/persistent-execution'
import type { PersistentPausedFileRecovery } from '../../../output/persistent-tree/contracts'

/** Preserve/restart is chosen once at the operation pause boundary. */
export function createFSAExecutionRecoveryPolicy(input: Readonly<{
  pausedFile: PersistentPausedFileRecovery
}>): PersistentExecutionRecoveryPolicy {
  return Object.freeze({ pausedFile: input.pausedFile })
}
