import { afterEach, describe, expect, it } from 'vitest'
import type { V2ReceiverTraceEvent } from '../../src/ui/controller/contracts'
import {
  FakeJoinedShare, FakeReceiveComposition, WORKSPACE_ENVIRONMENT, controllerFor,
  identityText, next, resetOrchestrationTestEnvironment, stableLifecycle, startTransfer, turns, waitFor,
} from './v2-receiver-orchestration-fixture'

afterEach(resetOrchestrationTestEnvironment)

describe('local action authority and outcome', () => {
  it('publishes the recovered lifecycle when only part of the requested local work succeeds', async () => {
    const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const events: V2ReceiverTraceEvent[] = []
    const controller = controllerFor(joined, receive, undefined, event => events.push(event))
    await startTransfer(controller, joined)
    const runtime = receive.startedAuthorities[0]!.runtime!
    const stable = stableLifecycle(runtime.lifecycle, 'waiting-to-save')
    joined.transferRuns[0]!.resolve(stable)
    await waitFor(() => controller.getSnapshot().output.lifecycle?.kind === 'waiting-to-save')
    const failure = new Error('Another retained file could not be saved')
    runtime.nextLifecycleAction = (_action, lifecycle) => ({
      lifecycle: next(lifecycle, { kind: 'waiting-to-save', packageDigest: identityText(92, 32) }),
      actionOutcome: { kind: 'failed', error: failure },
    })
    controller.performLifecycleAction('save')
    await waitFor(() => controller.getSnapshot().error === failure.message)
    expect(controller.getSnapshot().output.lifecycle?.generation).toBe(stable.generation + 1n)
    expect(events.filter(event => event.name === 'lifecycle_action_transition').map(event => event.transition))
      .toEqual(['started', 'failed'])
    await controller.dispose()
  })

  it.each(['same-authority', 'equivalent-copy', 'changed-data'] as const)('accepts journal-only completion only with the same lifecycle authority: %s', async authority => {
    const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const events: V2ReceiverTraceEvent[] = []
    const controller = controllerFor(joined, receive, undefined, event => events.push(event))
    await startTransfer(controller, joined)
    const runtime = receive.startedAuthorities[0]!.runtime!
    joined.transferRuns[0]!.resolve(stableLifecycle(runtime.lifecycle, 'waiting-to-save'))
    await waitFor(() => controller.getSnapshot().output.lifecycle?.kind === 'waiting-to-save')
    const before = controller.getSnapshot().output.lifecycle
    runtime.nextLifecycleAction = (_action, lifecycle) => ({
      lifecycle: authority === 'same-authority' ? lifecycle : alteredLifecycle(lifecycle, authority),
      actionOutcome: { kind: 'completed' },
    })
    controller.performLifecycleAction('save')
    await turns()
    expect(controller.getSnapshot().output.lifecycle).toBe(before)
    expect(events.filter(event => event.name === 'lifecycle_action_transition').map(event => event.transition))
      .toEqual(['started', authority === 'same-authority' ? 'completed' : 'excluded'])
    await controller.dispose()
  })
})

function alteredLifecycle(lifecycle: import('../../src/output/workspace').ReceiveLifecycleState,
  authority: 'equivalent-copy' | 'changed-data') {
  if (authority === 'equivalent-copy' || lifecycle.kind !== 'waiting-to-save') return { ...lifecycle }
  return { ...lifecycle, packageDigest: identityText(93, 32) }
}
