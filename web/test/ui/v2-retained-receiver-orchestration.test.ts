import { afterEach, describe, expect, it } from 'vitest'

import { V2ReceiverController } from '../../src/ui/v2-controller'
import type { V2BrowserReceiverGateway } from '../../src/ui/v2-gateway'
import type {
  V2RetainedReceiveActionResult,
  V2RetainedReceiveInventory,
} from '../../src/ui/v2-receive-runtime'
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
    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'actions')

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
    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'actions')

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
    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'actions')

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
