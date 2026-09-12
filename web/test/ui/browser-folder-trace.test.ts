import { describe, expect, it } from 'vitest'
import { browserFolderDeliveryTrace, observeBrowserFolderCheckpoint } from '../../src/ui/browser-receive/fsa/delivery-trace'
import { validateTraceEventPayloadV2 } from '../../src/diagnostics/export/trace-event-payload-v2'
import type { OutputDiagnosticsPorts, OutputTraceEvent } from '../../src/output/diagnostics'
import { deliveryIdentity } from '../output/browser-delivery-fixture'

function traceFixture() {
  const events: OutputTraceEvent[] = []
  const diagnostics: OutputDiagnosticsPorts = {
    backend: 'file_system_access',
    trace: { current: event => { events.push(event) } },
  }
  return { events, diagnostics }
}

describe('folder delivery diagnostic projection', () => {
  it('exports placement and local-save failure context without claiming target success', () => {
    const { events, diagnostics } = traceFixture()
    browserFolderDeliveryTrace(diagnostics)({
      name: 'browser.delivery.runtime', operation_id: deliveryIdentity(1, 16), file_id: deliveryIdentity(2, 16),
      transition: 'copy-failed', placement: 'staged', placement_reason: 'unknown-speed-large-file',
      recoverable_bytes: 1024n, failure_name: 'QuotaExceededError', copy_milliseconds: 23,
    })
    expect(events[0]).toMatchObject({ eventName: 'browser_delivery', payload: {
      transition: 'copy-failed', recoverable_bytes: '1024', failure_name: 'QuotaExceededError',
    } })
    expect(() => validateTraceEventPayloadV2('browser_delivery', events[0]!.payload)).not.toThrow()
  })

  it('records actual checkpoint cuts and pending bytes without logging every accepted range', () => {
    const { events, diagnostics } = traceFixture()
    const input = {
      operationId: deliveryIdentity(1, 16), transferJobId: deliveryIdentity(3, 16), fileId: deliveryIdentity(2, 16),
      objectId: deliveryIdentity(4), pendingBytes: 64n, durableBytes: 128n, atMilliseconds: 2000,
    }
    observeBrowserFolderCheckpoint(diagnostics, { ...input, stage: 'pending' })
    expect(events).toHaveLength(0)
    observeBrowserFolderCheckpoint(diagnostics, { ...input, stage: 'advanced', lastCheckpointMilliseconds: 2000, durationMilliseconds: 10 })
    expect(events[0]).toMatchObject({ eventName: 'browser_delivery', payload: {
      checkpoint_stage: 'advanced', pending_bytes: '64', recoverable_bytes: '128', last_checkpoint_milliseconds: 2000,
    } })
    expect(() => validateTraceEventPayloadV2('browser_delivery', events[0]!.payload)).not.toThrow()
  })
})
