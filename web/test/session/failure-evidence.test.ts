import { expect, it } from 'vitest'
import { createBrowserDiagnosticsComposition } from '../../src/application/diagnostics/composition'
import { createProtocolTraceSource } from '../../src/application/diagnostics/receive-trace'
import { evidenceFor } from '../../src/diagnostics/failure/evidence'
import { encodeBody, MESSAGE_KIND } from '../../src/session/message'
import { runtimeFixture } from './v2-send-fixture'

it.each(['send', 'read'] as const)('retains the original %s cause without activating trace or creating a user incident', async direction => {
  const diagnostics = createBrowserDiagnosticsComposition({ build: { version: '0.0.0', mode: 'test' }, secureContext: true,
    consoleSink: { error: () => undefined },
  })
  const { runtime, channel } = runtimeFixture(createProtocolTraceSource(diagnostics.trace, diagnostics.failures))
  const physical = new Error('socket failed', { cause: new DOMException('connection reset', 'NetworkError') })
  try {
    const request = runtime.beginOperation(MESSAGE_KIND.listChildren, encodeBody([]))
    const rejected = expect(request).rejects.toMatchObject({ scope: 'lane' })
    await channel.sending.promise
    if (direction === 'read') channel.failIncoming(physical)
    else channel.failOutgoing(physical)
    await rejected
    const snapshot = diagnostics.runtime.inspectFailureEvidence()
    expect(snapshot.records.length).toBeGreaterThan(0)
    const lane = snapshot.records.find(row => row.fact.correlation?.lane_id === 1)!
    expect(lane.fact.evidence?.operation).toBe('session.' + direction)
    expect(JSON.stringify(lane.fact.evidence?.exception)).toContain('connection reset')
    expect(lane.fact.correlation?.protocol_session_id).toBeDefined()
    expect(diagnostics.runtime.inspectLastFailure()).toBeNull()
    expect(diagnostics.runtime.status().state).toBe('idle')
    expect(evidenceFor(physical)?.exception.message ?? lane.fact.evidence?.exception.cause?.message).toBe('socket failed')
  } finally { await runtime.close() }
})

it('links queued send failures to one captured originating explanation', async () => {
  const { runtime, channel, events } = runtimeFixture()
  try {
    const first = expect(runtime.beginOperation(MESSAGE_KIND.listChildren, encodeBody([]))).rejects.toThrow()
    await channel.sending.promise
    const second = expect(runtime.beginOperation(MESSAGE_KIND.listChildren, encodeBody([]))).rejects.toThrow()
    channel.failOutgoing(new Error('physical delivery rejected'))
    await Promise.all([first, second])
    const failed = events.filter(event => event.eventName === 'protocol_operation' && event.transition === 'send_failed')
    expect(failed).toHaveLength(2)
    const evidence = failed.map(event => 'evidence' in event ? event.evidence : undefined)
    expect(evidence[0]).toBeDefined()
    expect(evidence[0]).toBe(evidence[1])
    expect(evidence[0]?.context.phase).toBe('sending')
    expect(evidence[0]?.exception.cause?.message).toBe('physical delivery rejected')
  } finally { await runtime.close() }
})
