import type {
  PersistentFileRecoveryPolicy,
  PersistentPausedFileRecovery,
  PersistentTemporarySpacePurpose,
  PersistentWriterPreflight,
} from '../../output/persistent-tree/contracts'
import type { OutputCheckpointCostBudget } from '../output-session'
import type { MaterializationRootRelativePath } from '../job/coordinate/direct-tree'

export type PersistentExecutionRecoveryPolicy = Readonly<{
  readonly pausedFile: PersistentPausedFileRecovery
  readonly costBudget?: OutputCheckpointCostBudget
  readonly confirmTemporarySpace?: (input: Readonly<{
    materializationRelativePath: MaterializationRootRelativePath
    preflight: PersistentWriterPreflight
    purpose: PersistentTemporarySpacePurpose
  }>) => boolean | Promise<boolean>
}>

export function bindPersistentTemporarySpaceConfirmation(
  policy: PersistentExecutionRecoveryPolicy,
  materializationRelativePath: MaterializationRootRelativePath,
): PersistentFileRecoveryPolicy['confirmTemporarySpace'] {
  const confirm = policy.confirmTemporarySpace
  if (confirm === undefined) return undefined
  return (preflight, purpose) => confirm({ materializationRelativePath, preflight, purpose })
}
