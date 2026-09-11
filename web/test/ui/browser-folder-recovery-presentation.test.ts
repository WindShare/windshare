import { describe, expect, it } from 'vitest'
import { summarizeBrowserDeliveries } from '../../src/output/browser-delivery/retained'
import { presentTask, retainedTaskFacts } from '../../src/ui/tasks'
import { taskFixture } from '../../src/ui/tasks/fixtures'
import { deliveryFixture } from '../output/browser-delivery-fixture'
import { presentReceiveLifecycle } from '../../src/ui/v2-lifecycle-presentation'
import { planningFixture } from './fsa-route-activation-fixture'
import type { V2RetainedReceiveOperation } from '../../src/ui/v2-receive-runtime'
import { retainedPresentationActions } from '../../src/ui/controller/retained-inventory-presentation'
import type { CompatibleNameRepairSummary } from '../../src/output/file-system-access/compatible-name/model'
import { retainedBrowserDeliveryActions } from '../../src/ui/browser-receive/fsa/retained-delivery'

describe('folder receiving and local saving presentation', () => {
  it.each(['receiving', 'staged-complete', 'discarding'] as const)(
    'keeps stopped %s child storage actionable without offering source continuation', async kind => {
      const fixture = deliveryFixture()
      const planning = await planningFixture()
      const record = kind === 'staged-complete'
        ? fixture.advance(fixture.initial, { kind, stage: fixture.stage! })
        : fixture.advance(fixture.initial, { kind, checkpoint: fixture.checkpoint('staged', 3n) })
      const summary = summarizeBrowserDeliveries(fixture.policy, [record])
      const lifecycle = {
        operationId: fixture.policy.operationId, receiveIntentDigest: fixture.policy.receiveIntentDigest, generation: 2n,
        kind: 'partial-directory' as const, reason: 'stopped' as const, successCount: 1n, failureCount: 1n, receiptDigest: fixture.policy.digest,
      }
      const actions = { receiving: 'discard-incomplete-staging', 'staged-complete': 'save-staged-files', discarding: 'cleanup-staging' } as const
      const expectedAction = actions[kind]
      const active = presentReceiveLifecycle({ state: lifecycle, artifact: planning.action.artifact,
        plan: { kind: 'direct-tree' } as Parameters<typeof presentReceiveLifecycle>[0]['plan'], browserDelivery: summary })
      expect(active.category).toBe('terminal')
      expect(active.actions).toEqual([{ kind: expectedAction, destructive: kind === 'receiving', label: expect.any(String) }])
      const operation: V2RetainedReceiveOperation = {
        operationId: lifecycle.operationId, receiveIntentDigest: lifecycle.receiveIntentDigest,
        lifecycleGeneration: lifecycle.generation, lifecycle, continuation: 'restoration-available',
        browserDelivery: summary, actions: retainedBrowserDeliveryActions(lifecycle, summary, []),
      }
      const task = presentTask(retainedTaskFacts(operation))
      expect(task.stage).toBe(kind === 'staged-complete' ? 'ready-to-save' : 'needs-action')
      expect(task.attention).toBe(true)
      const action = kind === 'receiving' ? task.destructiveActions[0] : task.primaryAction
      expect(action).toMatchObject({ id: expectedAction, disabledReason: null, destructive: kind === 'receiving' })
      expect(action?.consequence).not.toContain('available to continue')
      expect(task.details.join(' ')).not.toContain('recoverable after restart')
      expect(operation.actions).not.toContain('continue')
    },
  )

  it('keeps cleanup and explicit abandonment reachable while filename restoration is pending', () => {
    const fixture = deliveryFixture()
    const lifecycle = { operationId: fixture.policy.operationId, receiveIntentDigest: fixture.policy.receiveIntentDigest, generation: 2n,
      kind: 'partial-directory' as const, reason: 'stopped' as const, successCount: 1n, failureCount: 1n, receiptDigest: fixture.policy.digest }
    const operation: V2RetainedReceiveOperation = { operationId: lifecycle.operationId, receiveIntentDigest: lifecycle.receiveIntentDigest,
      lifecycleGeneration: lifecycle.generation, lifecycle, continuation: 'restoration-available',
      actions: ['save-staged-files', 'cleanup-staging', 'discard-incomplete-staging', 'catch-up'] }
    const summary = { terminalSettlement: 'pending' } as CompatibleNameRepairSummary
    expect(retainedPresentationActions(operation, summary)).toEqual(['cleanup-staging', 'discard-incomplete-staging', 'catch-up'])
  })

  it('offers save and cleanup independently and never removes a history record with child obligations', () => {
    const fixture = deliveryFixture()
    const summary = summarizeBrowserDeliveries(fixture.policy, [fixture.initial])
    const lifecycle = { operationId: fixture.policy.operationId, receiveIntentDigest: fixture.policy.receiveIntentDigest, generation: 2n,
      kind: 'published' as const, receiptDigest: fixture.policy.digest, cleanupState: 'clean' as const }
    expect(retainedBrowserDeliveryActions(lifecycle, { ...summary, stagedCompleteFiles: 1, cleanupPendingFiles: 1 }, ['forget']))
      .toEqual(['save-staged-files', 'cleanup-staging', 'discard-incomplete-staging'])
    expect(retainedBrowserDeliveryActions(lifecycle, { ...summary, incompleteStagedFiles: 0, stagedBytes: 0n, reservedStagingBytes: 0n }, ['forget']))
      .toEqual(['forget'])
    expect(retainedBrowserDeliveryActions(lifecycle, { ...summary, incompleteStagedFiles: 0, stagedCompleteFiles: 1,
      stagedBytes: 0n, reservedStagingBytes: 0n }, ['forget'])).toEqual(['save-staged-files'])
  })

  it('keeps paused incomplete staging for continuation without offering abandonment', () => {
    const fixture = deliveryFixture()
    const summary = summarizeBrowserDeliveries(fixture.policy, [fixture.initial])
    const lifecycle = { operationId: fixture.policy.operationId, receiveIntentDigest: fixture.policy.receiveIntentDigest, generation: 2n,
      kind: 'resumable-receive' as const, payloadKind: 'file-set' as const,
      checkpointSetDigest: fixture.policy.digest, completedFileCount: 0n, completedBytes: 0n,
      selectionFacts: { discoveredFileCount: 1n, discoveredBytes: 8n, discovery: 'failed' as const } }
    expect(retainedBrowserDeliveryActions(lifecycle, summary, ['continue'])).toEqual(['continue'])
  })

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
