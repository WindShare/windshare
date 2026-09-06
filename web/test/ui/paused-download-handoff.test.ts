import { afterEach, expect, it } from 'vitest'
import { composeTasks } from '../../src/ui/experience/task-composition'
import type { V2RetainedReceiveOperation } from '../../src/ui/v2-receive-runtime'
import {
  FakeReceiveComposition, FakeJoinedShare, WORKSPACE_ENVIRONMENT, controllerFor,
  startTransfer, next, identityText, waitFor, turns, deferred, resetOrchestrationTestEnvironment,
} from './v2-receiver-orchestration-fixture'

afterEach(resetOrchestrationTestEnvironment)

it('retains a paused ZIP through authority release and exposes the same native partial export', async () => {
  const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
  const joined = new FakeJoinedShare(true, [], 'tree')
  const controller = controllerFor(joined, receive)
  await startTransfer(controller, joined)
  const runtime = receive.startedAuthorities[0]!.runtime!
  const display = controller.getSnapshot().taskDisplay!
  const paused = next(runtime.lifecycle, {
    kind: 'resumable-receive', payloadKind: 'opfs-zip',
    objectId: identityText(92), checkpointGeneration: 2n, occupiedBytes: 4096n,
    completedFileCount: 1n, completedBytes: 1024n, discoveryComplete: false,
  })
  joined.transferRuns[0]!.resolve(paused)
  await waitFor(() => controller.canRetainCurrentOperation)
  await turns()
  const operation: V2RetainedReceiveOperation = Object.freeze({
    operationId: runtime.intent.operationId, receiveIntentDigest: runtime.intent.digest,
    shareInstance: joined.descriptor.shareInstanceId, lifecycleGeneration: paused.generation,
    lifecycle: paused, continuation: 'resume-receive',
    actions: Object.freeze(['continue', 'save-partial', 'discard'] as const), display,
  })
  const release = deferred<void>()
  runtime.detach = async () => {
    runtime.detachments.push('release-started')
    await release.promise
    receive.retainedOperations = Object.freeze([operation])
    runtime.detachments.push('released')
  }

  const handingOff = controller.retainCurrentOperation()
  await turns()
  expect(controller.getSnapshot().startAdmission.allowed).toBe(false)
  expect(controller.canRetainCurrentOperation).toBe(false)
  expect(controller.getSnapshot().output.lifecycle).toBe(paused)
  expect(controller.getSnapshot().taskDisplay).toBe(display)
  expect(controller.activeLifecycleActionAdmission('continue').allowed).toBe(false)
  expect(receive.retainedOperations).toEqual([])

  release.resolve()
  expect(await handingOff).toBe(true)
  const snapshot = controller.getSnapshot()
  expect(runtime.detachments).toEqual(['release-started', 'released'])
  expect(snapshot.output.lifecycle).toBeNull()
  expect(snapshot.startAdmission.allowed).toBe(true)
  const tasks = composeTasks(snapshot, (retained, action) => controller.retainedActionAdmission(retained, action))
  expect(tasks.current).toBeNull()
  expect(tasks.tasks).toHaveLength(1)
  const task = tasks.tasks[0]!
  expect(task.operationId).toBe(operation.operationId)
  expect(task.objectLabel).toBe(display.objectLabel)
  expect(task.progress?.label).toContain('1.0 KiB retained')
  const partial = task.secondaryActions.find(action => action.id === 'save-partial')!
  expect(partial.disabledReason).toBeNull()
  expect(partial.target.kind).toBe('retained')
  if (partial.target.kind === 'retained') {
    expect(partial.target.operation).toBe(operation)
    controller.performRetainedAction(partial.target.operation, partial.target.action)
  }
  await turns()
  expect(receive.retainedActionCalls[0]?.operation).toBe(operation)
  expect(receive.retainedActionCalls[0]?.action).toBe('save-partial')
  await controller.dispose()
})
