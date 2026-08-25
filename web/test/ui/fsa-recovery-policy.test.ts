import { describe, expect, it } from 'vitest'

import {
  createFSAExecutionRecoveryPolicy,
} from '../../src/ui/browser-receive/fsa/recovery-policy'
import {
  snapshotMaterializationRootRelativePath,
} from '../../src/transfer/job/coordinate/direct-tree'

const COST_BUDGET = Object.freeze({
  maximumPrefixCopyBytes: 256n * 1024n * 1024n,
  maximumCumulativeWriteAmplificationBytes: 512n * 1024n * 1024n,
  maximumPeakTemporaryBytes: 256n * 1024n * 1024n,
})

describe('FSA recovery policy', () => {
  it('asks once for the bounded automatic-checkpoint policy and reuses that decision', async () => {
    const prompts: string[] = []
    const policy = createFSAExecutionRecoveryPolicy({
      pausedFile: 'preserve',
      costBudget: COST_BUDGET,
      prompt: message => {
        prompts.push(message)
        return false
      },
    })

    await expect(policy.confirmTemporarySpace?.(request('automatic-checkpoint'))).resolves.toBe(false)
    await expect(policy.confirmTemporarySpace?.(request('automatic-checkpoint'))).resolves.toBe(false)

    expect(prompts).toHaveLength(1)
    expect(prompts[0]).toContain('256.0 MiB')
  })

  it('presents the exact paused file and temporary-space estimate', async () => {
    const prompts: string[] = []
    const policy = createFSAExecutionRecoveryPolicy({
      pausedFile: 'preserve',
      costBudget: COST_BUDGET,
      prompt: message => {
        prompts.push(message)
        return true
      },
    })

    expect(await policy.confirmTemporarySpace?.(request('paused-file-recovery'))).toBe(true)
    expect(prompts).toEqual([
      'Continue “folder/payload.bin” from its verified bytes? Preserving them may temporarily use 64.0 MiB of additional destination space. Cancel keeps the task paused so you can restart its incomplete files instead.',
    ])
  })
})

function request(purpose: 'automatic-checkpoint' | 'paused-file-recovery') {
  return {
    materializationRelativePath: snapshotMaterializationRootRelativePath(['folder', 'payload.bin']),
    purpose,
    preflight: Object.freeze({
      cost: Object.freeze({
        prefixCopyBytes: 64n * 1024n * 1024n,
        cumulativeWriteAmplificationBytes: 64n * 1024n * 1024n,
        peakTemporaryBytes: 64n * 1024n * 1024n,
      }),
      space: 'requires-user-confirmation' as const,
    }),
  }
}
