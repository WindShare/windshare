import { afterEach, describe, expect, it } from 'vitest'
import { validateTraceEventPayloadV1 } from '../../src/diagnostics/export/trace-event-payload-v1'
import type { V2ReceiverTraceEvent } from '../../src/ui/v2-controller'
import { ReceiverExperienceObservability } from '../../src/ui/experience/observability'
import { projectV2ReceiverTraceEvent } from '../../src/ui/v2-production-trace'
import {
  FakeJoinedShare, FakeReceiveComposition, MANAGED_ENVIRONMENT, controllerFor,
  resetOrchestrationTestEnvironment, startTransfer, next, identityText,
} from './v2-receiver-orchestration-fixture'

afterEach(resetOrchestrationTestEnvironment)

describe('receiver experience diagnostics', () => {
  it('records stage, promoted issue, saving decision and semantic intent without byte chatter', async () => {
    const joined = new FakeJoinedShare(true)
    const controller = controllerFor(joined, new FakeReceiveComposition(MANAGED_ENVIRONMENT))
    await startTransfer(controller, joined)
    const events: V2ReceiverTraceEvent[] = []
    const trace = new ReceiverExperienceObservability({ current: event => events.push(event) })
    const snapshot = controller.getSnapshot()
    if (snapshot.output.lifecycle === null) throw new Error('lifecycle missing')
    const initial = { ...snapshot, output: { ...snapshot.output,
      lifecycle: next(snapshot.output.lifecycle, { kind: 'receiving', activeLeaseId: identityText(90) }) } }
    trace.publish(initial)
    trace.publish({ ...initial, progress: { ...initial.progress, writtenBytes: 64n } })
    expect(events.filter(event => event.name === 'receiver_experience' && event.transition === 'task')).toHaveLength(1)
    trace.publish({ ...initial, progress: { ...initial.progress,
      capacityWaitVisible: true, capacityWaitingFiles: 1 } })
    trace.intent('open-downloads', initial)
    const completed = { ...initial, output: { ...initial.output, lifecycle: {
      ...next(initial.output.lifecycle, { kind: 'published', receiptDigest: identityText(91), cleanupState: 'clean' }),
      timing: { startedAtMilliseconds: 1_000, resultReadyAtMilliseconds: 126_000 },
    } } }
    trace.publish(completed)
    trace.publish(completed)
    expect(events).toEqual(expect.arrayContaining([
      expect.objectContaining({ transition: 'task', stage: 'waiting', reason: 'sender-capacity' }),
      expect.objectContaining({ transition: 'saving' }),
      expect.objectContaining({ transition: 'task', stage: 'saved', elapsedMilliseconds: 125_000 }),
      expect.objectContaining({ transition: 'intent', action: 'open-downloads',
        operationId: initial.output.receiveIntent?.operationId }),
    ]))
    for (const event of events) {
      const exported = projectV2ReceiverTraceEvent(event)
      expect(() => validateTraceEventPayloadV1(exported.eventName, exported.payload)).not.toThrow()
    }
    await controller.dispose()
  })
})
