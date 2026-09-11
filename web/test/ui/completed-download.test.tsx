import { renderToString } from 'react-dom/server'
import { afterEach, describe, expect, it } from 'vitest'
import type { ReceiveLifecycleState } from '../../src/output/workspace'
import { V2ReceiverApp } from '../../src/ui/V2ReceiverApp'
import type { V2ReceiverTraceEvent } from '../../src/ui/v2-controller'
import { projectV2ReceiverTraceEvent } from '../../src/ui/v2-production-trace'
import { validateTraceEventPayloadV1 } from '../../src/diagnostics/export/trace-event-payload-v1'
import { composeTasks } from '../../src/ui/experience/task-composition'
import { summarizeBrowserDeliveries } from '../../src/output/browser-delivery/retained'
import { presentTask, retainedTaskFacts } from '../../src/ui/tasks'
import { EMPTY_V2_OUTPUT_PRESENTATION } from '../../src/ui/v2-output'
import { EMPTY_V2_PROGRESS, EMPTY_V2_RETAINED_INVENTORY } from '../../src/ui/v2-model'
import type { V2RetainedReceiveOperation } from '../../src/ui/v2-receive-runtime'
import { deliveryFixture } from '../output/browser-delivery-fixture'
import { experienceSnapshot } from './receiver-experience-fixture'
import {
  FakeJoinedShare, FakeReceiveComposition, MANAGED_ENVIRONMENT, WORKSPACE_ENVIRONMENT,
  controllerFor, deferred, identityText, next, resetOrchestrationTestEnvironment,
  stableLifecycle, startTransfer, turns, waitFor,
} from './v2-receiver-orchestration-fixture'

afterEach(resetOrchestrationTestEnvironment)

function published(state: ReceiveLifecycleState): ReceiveLifecycleState {
  return next(state, { kind: 'published', receiptDigest: identityText(93, 32), cleanupState: 'clean' })
}

describe('completed download ownership and presentation', () => {
  it('keeps completed file and size metrics when detached portable history refreshes its actions', () => {
    const lifecycle: ReceiveLifecycleState = { operationId: identityText(10), receiveIntentDigest: identityText(11, 32),
      generation: 2n, kind: 'download-started', attemptKind: 'portable', attemptId: identityText(12) }
    const operation: V2RetainedReceiveOperation = { operationId: lifecycle.operationId,
      receiveIntentDigest: lifecycle.receiveIntentDigest, lifecycleGeneration: lifecycle.generation,
      lifecycle, continuation: 'history-only', actions: ['forget'] }
    const snapshot = experienceSnapshot({ activeReceiveOperationId: null,
      output: { ...EMPTY_V2_OUTPUT_PRESENTATION, lifecycle },
      progress: { ...EMPTY_V2_PROGRESS, transferJobId: identityText(13), discoveredFiles: 1,
        discoveredBytes: 8n, completedFiles: 1, completedBytes: 8n, materializedBytes: 8n, writtenBytes: 8n, discovery: 'complete' },
      retained: { ...EMPTY_V2_RETAINED_INVENTORY, operations: [operation] } })
    const current = composeTasks(snapshot).current!
    expect(current.stage).toBe('handed-to-browser')
    expect(current.details).toContain('1 files · 8 B total')
    expect(current.details).toContain('8 B in completed files.')
    expect(current.secondaryActions[0]?.target).toMatchObject({ kind: 'retained', action: 'forget' })
    expect(current.primaryAction).toBeNull()
  })

  it.each(['published', 'download-started'] as const)('keeps the %s result and starts the next picker in the click stack', async kind => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const events: V2ReceiverTraceEvent[] = []
    const controller = controllerFor(joined, receive, undefined, event => events.push(event))
    await startTransfer(controller, joined)
    const runtime = receive.startedAuthorities[0]!.runtime!
    const lifecycle = kind === 'published' ? published(runtime.lifecycle) : stableLifecycle(runtime.lifecycle, kind)
    joined.transferRuns[0]!.resolve(lifecycle)
    await waitFor(() => controller.getSnapshot().startAdmission.allowed &&
      controller.getSnapshot().output.offers?.kind === 'artifact-actions')

    expect(runtime.detachments).toEqual(['detached'])
    expect(controller.getSnapshot().output.lifecycle).toEqual(lifecycle)
    expect(controller.getSnapshot().activeReceiveOperationId).toBeNull()
    const releaseEvents = events.filter(event => event.name === 'receiver_experience' && event.transition === 'ownership')
    expect(releaseEvents).toMatchObject([
      { state: 'releasing', operationId: runtime.intent.operationId, lifecycleKind: kind },
      { state: 'released', operationId: runtime.intent.operationId, lifecycleKind: kind },
    ])
    for (const event of releaseEvents) {
      const exported = projectV2ReceiverTraceEvent(event)
      expect(() => validateTraceEventPayloadV1(exported.eventName, exported.payload)).not.toThrow()
    }
    const html = renderToString(<V2ReceiverApp controller={controller} />)
    expect(html).toContain(kind === 'published' ? '>Saved<' : 'Download started — check browser downloads')
    expect(html).toContain('Download again')
    expect(html).not.toContain('Start another download')
    expect(html).not.toContain('release the current task')
    expect(html).not.toContain('saving-controls')
    expect(html).not.toContain('<progress')

    const offers = controller.getSnapshot().output.offers
    if (offers?.kind !== 'artifact-actions') throw new Error('saving offers missing')
    let inClick = true
    receive.clickStack = () => inClick
    controller.chooseArtifact(offers.primary.choice.choiceId)
    controller.chooseArtifact(offers.primary.choice.choiceId)
    inClick = false
    expect(receive.startedAuthorities).toHaveLength(2)
    expect(receive.authorityStartStacks).toEqual([true, true])
    await waitFor(() => joined.transferRuns.length === 2)
    const nextRuntime = receive.startedAuthorities[1]!.runtime!
    expect(nextRuntime).not.toBe(runtime)
    expect(joined.transferRuns[1]!.plans).toBe(nextRuntime.plans)
    expect(controller.getSnapshot().output.receiveIntent).toEqual(nextRuntime.intent)
    expect(controller.getSnapshot().output.transferResultPresentation).toBeNull()
    await controller.dispose()
  })

  it('keeps download admission closed until output detachment finishes', async () => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const controller = controllerFor(joined, receive)
    await startTransfer(controller, joined)
    const runtime = receive.startedAuthorities[0]!.runtime!
    const detached = deferred<void>()
    runtime.detach = () => {
      runtime.detachments.push('started')
      return detached.promise
    }
    joined.transferRuns[0]!.resolve(published(runtime.lifecycle))
    await waitFor(() => runtime.detachments.length === 1)
    expect(controller.getSnapshot().startAdmission.allowed).toBe(false)
    expect(controller.getSnapshot().activeReceiveOperationId).toBe(runtime.intent.operationId)
    const offers = controller.getSnapshot().output.offers
    if (offers?.kind !== 'artifact-actions') throw new Error('saving offers missing')
    controller.chooseArtifact(offers.primary.choice.choiceId)
    expect(receive.startedAuthorities).toHaveLength(1)
    detached.resolve()
    await waitFor(() => controller.getSnapshot().startAdmission.allowed)
    await controller.dispose()
  })

  it('retains ready-to-save ownership until an explicit save completes', async () => {
    const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const controller = controllerFor(joined, receive)
    await startTransfer(controller, joined)
    const runtime = receive.startedAuthorities[0]!.runtime!
    joined.transferRuns[0]!.resolve(stableLifecycle(runtime.lifecycle, 'waiting-to-save'))
    await waitFor(() => controller.getSnapshot().output.lifecycle?.kind === 'waiting-to-save')
    await turns()
    expect(controller.getSnapshot().startAdmission.allowed).toBe(false)
    expect(runtime.detachments).toEqual([])
    runtime.nextLifecycleAction = (_action, state) => ({ lifecycle: stableLifecycle(state, 'download-started') })
    controller.performLifecycleAction('save')
    await waitFor(() => controller.getSnapshot().startAdmission.allowed)
    expect(runtime.lifecycleActions.map(action => action.action)).toEqual(['save'])
    expect(runtime.detachments).toEqual(['detached'])
    expect(controller.getSnapshot().output.lifecycle?.kind).toBe('download-started')
    await controller.dispose()
  })

  it('keeps paused ownership and blocks replacement even when all discovered bytes are present', async () => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const controller = controllerFor(joined, receive)
    await startTransfer(controller, joined)
    const runtime = receive.startedAuthorities[0]!.runtime!
    joined.transferRuns[0]!.resolve(stableLifecycle(runtime.lifecycle, 'resumable-receive'))
    await waitFor(() => controller.getSnapshot().output.lifecycle?.kind === 'resumable-receive')
    await turns()
    expect(controller.getSnapshot().startAdmission.allowed).toBe(false)
    expect(controller.getSnapshot().activeReceiveOperationId).toBe(runtime.intent.operationId)
    expect(runtime.detachments).toEqual([])
    await controller.dispose()
  })

  it('routes completed workspace actions through retained inventory after releasing the runtime', async () => {
    const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const controller = controllerFor(joined, receive)
    await startTransfer(controller, joined)
    const runtime = receive.startedAuthorities[0]!.runtime!
    const lifecycle = stableLifecycle(runtime.lifecycle, 'download-started')
    receive.retainedOperations = [{
      operationId: runtime.intent.operationId, receiveIntentDigest: runtime.intent.digest,
      lifecycleGeneration: lifecycle.generation, lifecycle, continuation: 'retry-download',
      actions: ['redownload', 'delete'],
    }]
    joined.transferRuns[0]!.resolve(lifecycle)
    await waitFor(() => controller.getSnapshot().retained.operations.length === 1)
    const { current, tasks } = composeTasks(controller.getSnapshot())
    expect(current?.primaryAction?.target.kind).toBe('retained')
    expect(tasks).toHaveLength(1)
    expect(runtime.detachments).toEqual(['detached'])
    expect(controller.getSnapshot().startAdmission.allowed).toBe(true)
    const action = current!.primaryAction!
    if (action.target.kind !== 'retained') throw new Error('retained action missing')
    controller.performRetainedAction(action.target.operation, action.target.action)
    expect(receive.retainedActionCalls[0]?.action).toBe('redownload')
    expect(runtime.lifecycleActions).toEqual([])
    await controller.dispose()
  })

  it('does not regress a delivered result to an older inventory generation, and respects removal from Downloads', async () => {
    const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const controller = controllerFor(joined, receive)
    await startTransfer(controller, joined)
    const runtime = receive.startedAuthorities[0]!.runtime!
    const earlier = stableLifecycle(runtime.lifecycle, 'waiting-to-save')
    const lifecycle = published(earlier)
    receive.retainedOperations = [{
      operationId: runtime.intent.operationId, receiveIntentDigest: runtime.intent.digest,
      lifecycleGeneration: earlier.generation, lifecycle: earlier, continuation: 'save-artifact',
      actions: ['forget'],
    }]
    joined.transferRuns[0]!.resolve(lifecycle)
    await waitFor(() => controller.getSnapshot().retained.operations.length === 1)
    const current = composeTasks(controller.getSnapshot()).current
    expect(current?.stage).toBe('saved')
    expect(current?.primaryAction).toBeNull()

    const retained = controller.getSnapshot().retained.operations[0]!
    receive.retainedOperations = []
    controller.performRetainedAction(retained, 'forget')
    await waitFor(() => controller.getSnapshot().output.lifecycle === null &&
      controller.getSnapshot().retained.kind === 'ready' && controller.getSnapshot().retained.operations.length === 0)
    expect(composeTasks(controller.getSnapshot()).tasks).toEqual([])
    await controller.dispose()
  })

  it('preserves the delivered result and blocks replacement when detachment fails', async () => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const controller = controllerFor(joined, receive)
    await startTransfer(controller, joined)
    const runtime = receive.startedAuthorities[0]!.runtime!
    runtime.detachFailure = new Error('Output resources could not be closed')
    joined.transferRuns[0]!.resolve(published(runtime.lifecycle))
    await waitFor(() => controller.getSnapshot().error !== null)
    expect(controller.getSnapshot().output.lifecycle?.kind).toBe('published')
    expect(controller.getSnapshot().startAdmission.allowed).toBe(false)
    expect(runtime.detachments).toEqual(['detached'])
    await controller.dispose()
  })
})

describe('stopped child obligation refresh integration', () => {
  it.each(['receiving', 'discarding', 'staged-complete'] as const)('refreshes displayed %s child facts when local recovery preserves parent generation', kind => {
    const f = deliveryFixture()
    const before = kind === 'staged-complete'
      ? f.advance(f.initial, { kind, stage: f.stage! })
      : f.advance(f.initial, { kind, checkpoint: f.checkpoint('staged', 3n) })
    let after = before
    if (kind === 'staged-complete') {
      after = f.advance(after, { kind: 'copying', stage: f.stage!, attempt: { attemptId: 'copy' } })
      after = f.advance(after, { kind: 'target-saved', target: f.target, stage: f.stage! })
      after = f.advance(after, { kind: 'cleanup-pending', target: f.target, stage: f.stage! })
      after = f.advance(after, { kind: 'cleaned', target: f.target })
    } else {
      if (kind === 'receiving') after = f.advance(after, { kind: 'discarding', checkpoint: f.checkpoint('staged', 3n) })
      after = f.advance(after, { kind: 'discarded' })
    }
    const lifecycle = { operationId: f.policy.operationId, receiveIntentDigest: f.policy.receiveIntentDigest,
      generation: 2n, kind: 'partial-directory' as const, reason: 'stopped' as const,
      successCount: 1n, failureCount: 1n, receiptDigest: f.policy.digest }
    const operation: V2RetainedReceiveOperation = { operationId: lifecycle.operationId,
      receiveIntentDigest: lifecycle.receiveIntentDigest, lifecycleGeneration: lifecycle.generation,
      lifecycle, continuation: 'restoration-available', actions: [],
      browserDelivery: summarizeBrowserDeliveries(f.policy, [after]) }
    const snapshot = experienceSnapshot({ activeReceiveOperationId: null,
      output: { ...EMPTY_V2_OUTPUT_PRESENTATION, lifecycle,
        browserFolderProgress: { kind: 'browser-folder', operationId: lifecycle.operationId,
          generation: 1n, summary: summarizeBrowserDeliveries(f.policy, [before]) } },
      progress: { ...EMPTY_V2_PROGRESS, transferJobId: identityText(13), discoveredFiles: 1,
        discoveredBytes: 8n, completedFiles: 0, completedBytes: 0n, materializedBytes: 3n, writtenBytes: 3n, discovery: 'complete' },
      retained: { ...EMPTY_V2_RETAINED_INVENTORY, operations: [operation] } })
    const actual = composeTasks(snapshot).current!
    const retained = presentTask(retainedTaskFacts(operation))
    expect(actual.headline).toBe(retained.headline)
    expect(actual.details).toContain('1 files · 8 B total')
    expect(actual.details).toContain('0 B retained in browser staging.')
    expect(actual.details).toContain(kind === 'staged-complete' ? '8 B saved to the chosen folder.' : '0 B saved to the chosen folder.')
    expect(actual.details.join(' ')).not.toContain('await staging cleanup')
    expect(actual.details.join(' ')).not.toContain('incomplete staged files cannot continue')
  })
})
