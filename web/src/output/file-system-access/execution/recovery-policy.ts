import type {
  PersistentExecutionRecoveryPolicy,
} from '../../execution/persistent-execution'
import type { PersistentPausedFileRecovery } from '../../persistent-tree/contracts'

/** Preserve/restart is chosen once at the operation pause boundary. */
export function createFSAExecutionRecoveryPolicy(input: Readonly<{
  pausedFile: PersistentPausedFileRecovery
}>): PersistentExecutionRecoveryPolicy {
  return Object.freeze({ pausedFile: input.pausedFile })
}
