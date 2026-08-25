import type { OutputCheckpointCostBudget } from '../../../transfer/output-session'
import type {
  PersistentExecutionRecoveryPolicy,
} from '../../../transfer/settlement/persistent-execution'
import type { PersistentPausedFileRecovery } from '../../../output/persistent-tree/contracts'
import { formatBytes } from '../../v2-progress-presentation'

export type FSARecoverySpacePrompt = (message: string) => boolean | Promise<boolean>

export function browserFSARecoverySpacePrompt(message: string): boolean {
  return globalThis.confirm(message)
}

/** One explicit grant covers the bounded automatic-checkpoint policy for this attempt. */
export function createFSAExecutionRecoveryPolicy(input: Readonly<{
  pausedFile: PersistentPausedFileRecovery
  costBudget: OutputCheckpointCostBudget
  prompt: FSARecoverySpacePrompt
}>): PersistentExecutionRecoveryPolicy {
  let automaticCheckpointConsent: Promise<boolean> | undefined
  return Object.freeze({
    pausedFile: input.pausedFile,
    costBudget: input.costBudget,
    confirmTemporarySpace: request => {
      if (request.purpose === 'automatic-checkpoint') {
        automaticCheckpointConsent ??= Promise.resolve(input.prompt(
          'Allow automatic recovery checkpoints for this receive task? ' +
          `They may temporarily use up to ${formatBytes(
            input.costBudget.maximumPeakTemporaryBytes,
          )} of additional destination space.`,
        ))
        return automaticCheckpointConsent
      }
      return input.prompt(
        `Continue ${displayPath(request.materializationRelativePath)} from its verified bytes? ` +
        `Preserving them may temporarily use ${formatBytes(
          request.preflight.cost.peakTemporaryBytes,
        )} of additional destination space. Cancel keeps the task paused so you can ` +
        'restart its incomplete files instead.',
      )
    },
  })
}

function displayPath(path: readonly string[]): string {
  return path.length === 0 ? 'the selected file' : `“${path.join('/')}”`
}
