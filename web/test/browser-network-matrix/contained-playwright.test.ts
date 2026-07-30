import { describe, expect, it, vi } from 'vitest'

import {
  ContainedPlaywrightNetworkMatrixSampleExecutor,
  type NetworkMatrixContainedPlaywrightProcessBroker,
} from '../../scripts/browser-network-matrix/contained-playwright.ts'
import {
  completedOwnedOperation,
  settleOwnedOperation,
} from '../../scripts/browser-network-matrix/owned-operation.ts'
import type { NetworkMatrixSampleExecutionContext } from '../../scripts/browser-network-matrix/runner.ts'
import type { NetworkMatrixAttemptEvidence } from '../../scripts/browser-network-matrix/attempt-evidence.ts'
import { loadRegistry, matchedAttemptEvidence } from './fixtures.ts'

describe('contained Playwright matrix sample adapter', () => {
  it('returns only parent-collected two-ended evidence from one contained OS process', async () => {
    const context = await sampleContext()
    const broker: NetworkMatrixContainedPlaywrightProcessBroker = {
      start: vi.fn().mockReturnValue(completedOwnedOperation({
        processInstanceId: 'contained-browser-process',
        attemptEvidence: matchedAttemptEvidence(context.identity, context.runId),
      })),
    }
    const executor = new ContainedPlaywrightNetworkMatrixSampleExecutor(broker)

    await expect(executor.execute(context).result).resolves.toEqual({
      processInstanceId: 'contained-browser-process',
      observation: {
        sampleOutcome: 'observed',
        attemptEvidence: matchedAttemptEvidence(context.identity, context.runId),
      },
    })
    expect(broker.start).toHaveBeenCalledOnce()
  })

  it('forwards forced subtree reaping when joined attempt evidence rejects', async () => {
    const context = await sampleContext()
    const forceTerminateAndWait = vi.fn().mockResolvedValue(undefined)
    const broker: NetworkMatrixContainedPlaywrightProcessBroker = {
      start: () => ({
        result: Promise.resolve({
          processInstanceId: 'contained-browser-process',
          attemptEvidence: {
           ...matchedAttemptEvidence(context.identity, context.runId),
            pionAuthority: 'shared-namespace-control',
          } as unknown as NetworkMatrixAttemptEvidence,
        }),
        forceTerminateAndWait,
      }),
    }
    const executor = new ContainedPlaywrightNetworkMatrixSampleExecutor(broker)

    await expect(settleOwnedOperation(
      executor.execute(context),
      'sample-execute',
      10,
      { schedule: () => ({ elapsed: new Promise<void>(() => undefined), cancel: () => undefined }) },
    )).rejects.toMatchObject({ failureCode: 'evidence-collection-failed' })
    expect(forceTerminateAndWait).toHaveBeenCalledWith('sample-execute')
  })
})

async function sampleContext(): Promise<NetworkMatrixSampleExecutionContext> {
  const registry = await loadRegistry()
  const profile = registry.profiles.find(({ profileId }) => profileId === 'scheduled-public-stun')
  if (profile === undefined) throw new Error('test profile is absent')
  return {
    runId: 'contained-playwright-run',
    manifestSha256: registry.manifestSha256,
    identity: {
      profileId: 'scheduled-public-stun',
      browser: 'chromium',
      sampleOrdinal: 1,
    },
    profile,
    authority: {
      profileId: 'scheduled-public-stun',
      runtimeKind: 'external-fixture',
    },
    operationId: 'contained-playwright-operation',
  }
}
