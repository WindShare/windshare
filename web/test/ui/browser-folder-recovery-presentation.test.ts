import { describe, expect, it } from 'vitest'
import { summarizeBrowserDeliveries } from '../../src/output/browser-delivery/retained'
import { presentTask, retainedTaskFacts } from '../../src/ui/tasks'
import { taskFixture } from '../../src/ui/tasks/fixtures'
import { deliveryFixture } from '../output/browser-delivery-fixture'
import { presentReceiveLifecycle } from '../../src/ui/v2-lifecycle-presentation'
import { planningFixture } from './fsa-route-activation-fixture'
import type { V2RetainedReceiveOperation } from '../../src/ui/v2-receive-runtime'

describe('folder receiving and local saving presentation', () => {
  it('offers immediate local retry on an active paused folder before source recovery facts are available', async () => {
    const fixture = deliveryFixture()
    const planning = await planningFixture()
    const complete = fixture.advance(fixture.initial, { kind: 'staged-complete', stage: fixture.stage! })
    const summary = summarizeBrowserDeliveries(fixture.policy, [complete])
    const lifecycle = {
      operationId: fixture.policy.operationId, receiveIntentDigest: fixture.policy.receiveIntentDigest, generation: 2n,
      kind: 'resumable-receive' as const, payloadKind: 'file-set' as const,
      checkpointSetDigest: fixture.policy.digest, completedFileCount: 0n, completedBytes: 0n,
      selectionFacts: { discoveredFileCount: 1n, discoveredBytes: 8n, discovery: 'failed' as const },
    }
    const presentation = presentReceiveLifecycle({
      state: lifecycle, artifact: planning.action.artifact,
      plan: { kind: 'direct-tree' } as Parameters<typeof presentReceiveLifecycle>[0]['plan'],
      browserDelivery: summary, recoverySummary: null,
    })
    expect(presentation.actions).toEqual([
      { kind: 'save-staged-files', label: 'Save received files to folder', destructive: false },
    ])
    const task = presentTask(taskFixture({ lifecycle, browserDelivery: summary }))
    expect(task.headline).toBe('Received files are ready to save')
    expect(presentReceiveLifecycle({
      state: { ...lifecycle, receiveIntentDigest: `${lifecycle.receiveIntentDigest}changed` },
      artifact: planning.action.artifact, plan: { kind: 'direct-tree' } as Parameters<typeof presentReceiveLifecycle>[0]['plan'],
      browserDelivery: summary,
    }).actions).toEqual([])
  })

  it('shows actual verified recovery bytes independently from received progress and target saving', () => {
    const fixture = deliveryFixture()
    const receiving = fixture.advance(fixture.initial, { kind: 'receiving', checkpoint: fixture.checkpoint('staged', 3n) })
    const summary = summarizeBrowserDeliveries(fixture.policy, [receiving])
    const task = presentTask(taskFixture({ browserDelivery: summary }))
    expect(task.progress?.details).toContain('3 B verified and recoverable after restart.')
    expect(task.progress?.details).toContain('0 B saved to the chosen folder.')
    expect(task.progress?.details).toContain('3 B retained in browser staging.')
    expect(task.stage).toBe('downloading')
  })

  it('shows local copying even while sender reconnection is in progress', () => {
    const fixture = deliveryFixture()
    const complete = fixture.advance(fixture.initial, { kind: 'staged-complete', stage: fixture.stage! })
    const copying = fixture.advance(complete, { kind: 'copying', stage: fixture.stage!, attempt: { attemptId: 'copy-attempt' } })
    const task = presentTask(taskFixture({
      browserDelivery: summarizeBrowserDeliveries(fixture.policy, [copying]),
      blocking: { kind: 'reconnecting' },
    }))
    expect(task.headline).toBe('Saving received files to folder')
    expect(task.publication).toBe('unpublished')
    expect(task.progress?.details).toContain('1 received files still need local saving.')
  })

  it('keeps the offline local save action enabled while unfinished receive requires the original link', () => {
    const fixture = deliveryFixture()
    const complete = fixture.advance(fixture.initial, { kind: 'staged-complete', stage: fixture.stage! })
    const summary = summarizeBrowserDeliveries(fixture.policy, [complete])
    const lifecycle = {
      operationId: fixture.policy.operationId, receiveIntentDigest: fixture.policy.receiveIntentDigest, generation: 2n,
      kind: 'resumable-receive' as const, payloadKind: 'file-set' as const,
      checkpointSetDigest: fixture.policy.digest, completedFileCount: 0n, completedBytes: 0n,
      selectionFacts: { discoveredFileCount: 1n, discoveredBytes: 8n, discovery: 'failed' as const },
    }
    const operation: V2RetainedReceiveOperation = {
      operationId: lifecycle.operationId, receiveIntentDigest: lifecycle.receiveIntentDigest,
      lifecycleGeneration: lifecycle.generation, lifecycle, continuation: 'resume-receive',
      actions: ['continue', 'save-staged-files'], browserDelivery: summary,
    }
    const task = presentTask(retainedTaskFacts(operation))
    expect(task.headline).toBe('Received files are ready to save')
    expect(task.primaryAction).toMatchObject({ id: 'save-staged-files', disabledReason: null })
    expect(task.secondaryActions.find(action => action.id === 'continue')?.disabledReason).toContain('original share link')
    expect(task.details).toContain('8 B verified and recoverable after restart.')
    expect(task.details).toContain('0 B saved to folder')
  })
})
