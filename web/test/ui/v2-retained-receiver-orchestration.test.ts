import { afterEach, describe, expect, it, vi } from 'vitest'

import { TransferFailureAccumulator, transferWorkerSettlement } from '../../src/transfer/outcome'
import type { TransferJobResult } from '../../src/transfer/v2-job'
import { V2ReceiverController } from '../../src/ui/v2-controller'
import type { V2BrowserReceiverGateway } from '../../src/ui/v2-gateway'
import type {
  V2RetainedReceiveActionResult,
  V2RetainedReceiveInventory,
} from '../../src/ui/v2-receive-runtime'
import type { CompatibleNameRepairSummary } from '../../src/output/file-system-access/compatible-name/model'
import {
  FakeGateway,
  FakeJoinedShare,
  FakeReceiveComposition,
  MANAGED_ENVIRONMENT,
  controllerFor,
  deferred,
  identityText,
  resetOrchestrationTestEnvironment,
  retainedInventory,
  retainedOperation,
  retainedReceiveContinuation,
  retainedWorkspaceIntent,
  traceSource,
  turns,
  waitFor,
  waitingRetainedOperation,
} from './v2-receiver-orchestration-fixture'

afterEach(resetOrchestrationTestEnvironment)

describe('v2 receiver product orchestration', () => {
  it('qualifies pending retained repair authority and reloads after local catch-up', async () => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const lifecycle = Object.freeze({
      kind: 'receiving' as const,
      operationId: identityText(101),
      receiveIntentDigest: identityText(102, 32),
      generation: 3n,
      activeLeaseId: identityText(103),
    })
    const sourceOperation = Object.freeze({
      operationId: lifecycle.operationId,
      receiveIntentDigest: lifecycle.receiveIntentDigest,
      lifecycleGeneration: lifecycle.generation,
      lifecycle,
      continuation: 'pending-catch-up' as const,
      actions: Object.freeze(['catch-up'] as const),
    })
    const summary = repairSummary(true, 'active')
    receive.retainedOperations = Object.freeze([sourceOperation])
    receive.repairSummaries.set(sourceOperation.operationId, summary)
    const actionGate = deferred<V2RetainedReceiveActionResult>()
    receive.retainedActionGate = actionGate.promise
    const controller = new V2ReceiverController(
      new FakeGateway([]) as unknown as V2BrowserReceiverGateway,
      { receive },
    )
    controller.initialize({ capabilityInput: null, pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => controller.getSnapshot().retained.kind === 'ready')

    const presented = controller.getSnapshot().retained.operations[0]
    expect(presented).not.toBe(sourceOperation)
    expect(presented).toMatchObject({
      actions: ['catch-up'],
      repairSummary: summary,
    })
    if (presented === undefined) throw new Error('pending catch-up was not presented')
    controller.performRetainedAction(presented, 'catch-up')

    expect(receive.retainedActionCalls).toMatchObject([{
      operation: sourceOperation,
      action: 'catch-up',
    }])
    receive.retainedOperations = Object.freeze([])
    actionGate.resolve(Object.freeze({ kind: 'completed' }))
    await waitFor(() => controller.getSnapshot().retained.kind === 'ready' &&
      controller.getSnapshot().retained.operations.length === 0)

    await controller.dispose()
  })

  it('publishes retained v6 lifecycle inventory without acquiring receive authority', async () => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    receive.retainedOperations = Object.freeze([retainedOperation()])
    const traces: string[] = []
    const controller = new V2ReceiverController(
      new FakeGateway([]) as unknown as V2BrowserReceiverGateway,
      { receive, trace: traceSource(event => traces.push(event.name)) },
    )

    controller.initialize({ capabilityInput: null, pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => controller.getSnapshot().retained.kind === 'ready')

    expect(controller.getSnapshot().retained.operations).toEqual(receive.retainedOperations)
    expect(receive.startedAuthorities).toHaveLength(0)
    expect(traces).toContain('receive.inventory.load.started')
    expect(traces).toContain('receive.inventory.load.completed')
    const diagnostic = controller.getDiagnosticSnapshot()
    expect(Object.isFrozen(diagnostic)).toBe(true)
    expect(diagnostic).not.toHaveProperty('retained')
    expect(diagnostic.progress.discoveredFiles).toBe(0n)
    await controller.dispose()
  })

  it('consumes a retained action from the exact inventory and reloads after durable completion', async () => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const operation = waitingRetainedOperation()
    receive.retainedOperations = Object.freeze([operation])
    const actionGate = deferred<V2RetainedReceiveActionResult>()
    receive.retainedActionGate = actionGate.promise
    const traces: string[] = []
    const controller = new V2ReceiverController(
      new FakeGateway([]) as unknown as V2BrowserReceiverGateway,
      { receive, trace: traceSource(event => traces.push(event.name)) },
    )
    controller.initialize({ capabilityInput: null, pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => controller.getSnapshot().retained.kind === 'ready')

    controller.performRetainedAction(Object.freeze({ ...operation }), 'save')
    expect(receive.retainedActionCalls).toHaveLength(0)

    controller.performRetainedAction(operation, 'save')
    expect(receive.retainedActionCalls).toMatchObject([{ operation, action: 'save' }])
    receive.retainedOperations = Object.freeze([])
    actionGate.resolve(Object.freeze({ kind: 'completed' }))
    await waitFor(() => receive.retainedSignals.length === 2 &&
      controller.getSnapshot().retained.kind === 'ready')

    expect(controller.getSnapshot().retained.operations).toEqual([])
    expect(traces).toContain('receive.inventory.action.started')
    expect(traces).toContain('receive.inventory.action.completed')
    expect(receive.retainedInventories[0]?.close).toHaveBeenCalledTimes(1)
    await controller.dispose()
  })

  it('keeps direct selection, artifact, and retained admission closed while an action is pending', async () => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const operation = waitingRetainedOperation()
    const actionGate = deferred<V2RetainedReceiveActionResult>()
    receive.retainedOperations = Object.freeze([operation])
    receive.retainedActionGate = actionGate.promise
    const controller = controllerFor(joined, receive)
    await waitFor(() => controller.getSnapshot().retained.kind === 'ready')
    await waitFor(() => controller.getSnapshot().rows.length > 0)
    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'choices')
    const before = controller.getSnapshot()
    const row = before.rows[0]
    const presentation = before.output.offerPresentation
    if (row === undefined || presentation?.kind !== 'choices') {
      throw new Error('browsing controls were not presented')
    }

    controller.performRetainedAction(operation, 'save')
    expect(controller.getSnapshot().retained.pending).toEqual({
      operationId: operation.operationId,
      action: 'save',
    })

    controller.toggleSelection(row.id)
    const defaultChoice = presentation.defaultChoices[0]
    if (defaultChoice === undefined) throw new Error('default choice was not presented')
    controller.chooseArtifact(defaultChoice.choice.choiceId)
    controller.performRetainedAction(operation, 'discard')

    expect(controller.getSnapshot().rows[0]?.selection).toBe(row.selection)
    expect(receive.startedAuthorities).toHaveLength(0)
    expect(receive.retainedActionCalls).toMatchObject([{ operation, action: 'save' }])

    actionGate.resolve(Object.freeze({ kind: 'completed' }))
    await waitFor(() => controller.getSnapshot().retained.pending === null)
    await controller.dispose()
  })

  it('fences a retained mutation that completes after controller disposal', async () => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const operation = waitingRetainedOperation()
    receive.retainedOperations = Object.freeze([operation])
    const actionGate = deferred<V2RetainedReceiveActionResult>()
    receive.retainedActionGate = actionGate.promise
    const traces: string[] = []
    const controller = new V2ReceiverController(
      new FakeGateway([]) as unknown as V2BrowserReceiverGateway,
      { receive, trace: traceSource(event => traces.push(event.name)) },
    )
    controller.initialize({ capabilityInput: null, pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => controller.getSnapshot().retained.kind === 'ready')

    controller.performRetainedAction(operation, 'save')
    await controller.dispose()
    actionGate.resolve(Object.freeze({ kind: 'completed' }))
    await turns()

    expect(receive.retainedActionCalls[0]?.signal.aborted).toBe(true)
    expect(receive.retainedInventories[0]?.close).toHaveBeenCalledTimes(1)
    expect(traces).not.toContain('receive.inventory.action.completed')
  })

  it('adopts an exact fresh-page receive continuation without a projection-derived plan', async () => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const intent = await retainedWorkspaceIntent(joined)
    const { operation, runtime } = retainedReceiveContinuation(intent)
    receive.retainedOperations = Object.freeze([operation])
    receive.retainedActionGate = Promise.resolve(Object.freeze({
      kind: 'receive-continuation',
      runtime,
    }))
    const controller = controllerFor(joined, receive)
    await waitFor(() => controller.getSnapshot().retained.kind === 'ready')
    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'choices')

    controller.performRetainedAction(operation, 'continue')
    await waitFor(() => joined.transferRuns.length === 1)

    expect(joined.transferRuns[0]?.intent).toBe(intent)
    expect(controller.getSnapshot().output.projection).toBeNull()
    expect(controller.getSnapshot().output.receiveIntent?.digest).toBe(intent.digest)
    expect(controller.getSnapshot().output.lifecycle).toBe(runtime.lifecycle)
    expect(receive.startedAuthorities).toHaveLength(0)

    await controller.dispose()
    expect(runtime.detachments).toEqual(['detached'])
  })

  it('hands a stopped receiver to local catch-up only after releasing its output ownership', async () => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const intent = await retainedWorkspaceIntent(joined)
    const { operation, runtime } = retainedReceiveContinuation(intent)
    const summary = repairSummary(true, 'active')
    const transfer = deferred<TransferJobResult>()
    vi.spyOn(joined, 'transferJob').mockReturnValue({ run: () => transfer.promise })
    const repairedRuntime = Object.assign(runtime, {
      repairProjection: {
        getSnapshot: () => summary,
        subscribe: () => () => undefined,
      },
    })
    receive.retainedOperations = [operation]
    receive.retainedActionGate = Promise.resolve({ kind: 'receive-continuation', runtime: repairedRuntime })
    const controller = controllerFor(joined, receive)
    await waitFor(() => controller.getSnapshot().retained.kind === 'ready')
    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'choices')
    controller.performRetainedAction(operation, 'continue')
    await waitFor(() => controller.getSnapshot().output.lifecycle?.kind === 'receiving')
    controller.catchUpStoppedCompatibleNames()
    expect(runtime.detachments).toEqual([])

    const paused = {
      ...operation.lifecycle,
      generation: runtime.lifecycle.generation + 1n,
    }
    transfer.resolve({
      worker: transferWorkerSettlement('Paused', new TransferFailureAccumulator().snapshot()),
      lifecycle: paused,
      measure: {} as TransferJobResult['measure'],
      transferJobId: runtime.transferJobId,
      intent,
      repairSummary: summary,
    })
    await waitFor(() => controller.getSnapshot().output.lifecycle?.kind === 'resumable-receive')
    const retained = {
      ...operation, lifecycle: paused, lifecycleGeneration: paused.generation,
      actions: ['continue', 'catch-up'] as const,
    }
    receive.retainedOperations = [retained]
    receive.repairSummaries.set(intent.operationId, summary)
    const replay = deferred<V2RetainedReceiveActionResult>()
    receive.retainedActionGate = replay.promise
    controller.catchUpStoppedCompatibleNames()
    await waitFor(() => receive.retainedActionCalls.some(call => call.action === 'catch-up'))
    expect(runtime.detachments).toEqual(['detached'])
    expect(controller.getSnapshot().output.receiveIntent).toBeNull()
    expect(controller.getSnapshot().retained.operations[0]?.actions).toEqual(['continue', 'catch-up'])
    receive.repairSummaries.set(intent.operationId, { ...summary, sidecarSync: 'current' })
    replay.resolve({ kind: 'completed' })
    await waitFor(() => controller.getSnapshot().retained.operations[0]?.actions.length === 1)
    expect(controller.getSnapshot().retained.operations[0]?.actions).toEqual(['continue'])
    await controller.dispose()
  })

  it('drops and closes a fresh-page continuation after reconnect changes its action epoch', async () => {
    const gate = deferred<V2RetainedReceiveActionResult>()
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const intent = await retainedWorkspaceIntent(joined)
    const { operation, runtime } = retainedReceiveContinuation(intent)
    receive.retainedOperations = Object.freeze([operation])
    receive.retainedActionGate = gate.promise
    const traces: string[] = []
    const controller = new V2ReceiverController(
      new FakeGateway([joined]) as unknown as V2BrowserReceiverGateway,
      { receive, trace: traceSource(event => traces.push(event.name)) },
    )
    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => controller.getSnapshot().retained.kind === 'ready')
    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'choices')

    controller.performRetainedAction(operation, 'continue')
    joined.protocolSessionId = identityText(4)
    gate.resolve(Object.freeze({ kind: 'receive-continuation', runtime }))
    await waitFor(() => runtime.detachments.length === 1)

    expect(joined.transferRuns).toHaveLength(0)
    expect(controller.getSnapshot().output.receiveIntent).toBeNull()
    expect(traces).toContain('receive.inventory.action.failed')
    expect(traces).not.toContain('receive.inventory.action.completed')
    await controller.dispose()
  })

  it('rejects a continuation whose persisted operation identity changed', async () => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const intent = await retainedWorkspaceIntent(joined)
    const { operation, runtime } = retainedReceiveContinuation(intent)
    const foreignOperation = Object.freeze({
      ...operation,
      operationId: identityText(99),
    })
    receive.retainedOperations = Object.freeze([foreignOperation])
    receive.retainedActionGate = Promise.resolve(Object.freeze({
      kind: 'receive-continuation',
      runtime,
    }))
    const controller = controllerFor(joined, receive)
    await waitFor(() => controller.getSnapshot().retained.kind === 'ready')
    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'choices')

    controller.performRetainedAction(foreignOperation, 'continue')
    await waitFor(() => runtime.detachments.length === 1)

    expect(joined.transferRuns).toHaveLength(0)
    expect(controller.getSnapshot().output.receiveIntent).toBeNull()
    await controller.dispose()
  })

  it('fences a retained inventory result that resolves after controller disposal', async () => {
    const gate = deferred<V2RetainedReceiveInventory>()
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    receive.retainedGate = gate.promise
    const traces: string[] = []
    const controller = new V2ReceiverController(
      new FakeGateway([]) as unknown as V2BrowserReceiverGateway,
      { receive, trace: traceSource(event => traces.push(event.name)) },
    )

    controller.initialize({ capabilityInput: null, pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => receive.retainedSignals.length === 1)
    await controller.dispose()
    const lateInventory = retainedInventory(Object.freeze([retainedOperation()]))
    gate.resolve(lateInventory)
    await turns()

    expect(receive.retainedSignals[0]?.aborted).toBe(true)
    expect(lateInventory.close).toHaveBeenCalledTimes(1)
    expect(controller.getSnapshot().retained.kind).toBe('loading')
    expect(traces).toEqual(['receive.inventory.load.started'])
  })
})

function repairSummary(
  sidecarPending: boolean,
  footerState: 'active' | 'completed' | 'stopped' | 'failed',
): CompatibleNameRepairSummary {
  const terminalSettlement = sidecarPending ? 'pending' : 'complete'
  return Object.freeze({
    committedCount: 1,
    logicalPathSample: Object.freeze([Object.freeze(['report.txt'])]),
    pairDisplayNames: Object.freeze({
      script: 'restore.windshare-abc234.ps1',
      sidecar: 'restore.windshare-abc234.data',
    }),
    placement: 'inside-logical-root',
    latestObservedFooter: Object.freeze({ committedCount: 1, state: footerState }),
    sidecarSync: sidecarPending ? 'pending' : 'current',
    terminalSettlement: footerState === 'active' ? 'none' : terminalSettlement,
  })
}
