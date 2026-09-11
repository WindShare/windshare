import { describe, expect, it } from 'vitest'
import { BrowserAttemptLifecycle } from '../../src/connectivity/v2-session-signaling-lifecycle'
import type { V2ConnectivityTraceEvent } from '../../src/connectivity/diagnostics'
import {
  createV2ProtocolSessionIdentity, createV2ProtocolOperationIdentity,
  createV2PeerPathIdentityValue, createV2PeerAttemptIdentity,
} from '../../src/session/v2-identities'
import { projectConnectivityTraceEvent } from '../../src/ui/v2-production-trace'
import { snapshotTraceEventObservationV1 } from '../../src/diagnostics/export/trace-event-v1'
import { createBrowserDiagnosticsComposition } from '../../src/diagnostics/browser-composition'
import { FakeTraceTime } from '../diagnostics/trace/test-support'

const bytes = (seed: number) => new Uint8Array(16).fill(seed)

function timedOutAttempt() {
  let now = 0
  const events: V2ConnectivityTraceEvent[] = []
  const lifecycle = new BrowserAttemptLifecycle(() => ({
    protocolSessionId: createV2ProtocolSessionIdentity(bytes(1)),
    peerPathId: createV2PeerPathIdentityValue(bytes(2)),
    attemptId: createV2PeerAttemptIdentity(bytes(3)),
    waveOrdinal: 1, waveAttemptOrdinal: 1, sessionAttemptOrdinal: 1,
  }), { current: event => events.push(event) }, () => now)
  lifecycle.phaseDeadlineArmed('negotiation', 65_000)
  for (const stage of ['offer-created', 'offer-sent', 'answer-received', 'datachannel-open'] as const) {
    lifecycle.offerMilestone(stage, { localEmitted: 1, remoteAccepted: 1 })
  }
  now = 1_000
  lifecycle.phaseDeadlineArmed('admission', 20_000)
  lifecycle.grantRequested(createV2ProtocolOperationIdentity(bytes(4)), 2)
  now = 21_000
  lifecycle.phaseDeadlineExpired('admission', 20_000)
  lifecycle.failed({ kind: 'local-transient', phase: 'admission', reason: 'admission-timeout' })
  const last = events.at(-1)!
  return { lifecycle, events, last, projected: snapshotTraceEventObservationV1(projectConnectivityTraceEvent(last)) }
}

describe('peer failure summaries', () => {
  it('exports the exact timeout and waiting stage in one immutable terminal record', () => {
    const { last, projected, lifecycle, events } = timedOutAttempt()
    expect(last).toMatchObject({
      stage: 'failed', failedAtStage: 'grant-received',
      summary: { lastCompletedStage: 'grant-requested', attemptElapsedMilliseconds: 21_000,
        stageElapsedMilliseconds: 20_000, deadlineExpired: true },
    })
    expect(projected.payload).toMatchObject({
      failure: { kind: 'local-transient', phase: 'admission', reason: 'admission-timeout' },
      summary: { last_completed_stage: 'grant_requested', attempt_elapsed_ms: 21_000,
        stage_elapsed_ms: 20_000, deadline_expired: true },
    })
    expect(Object.isFrozen(projected.payload)).toBe(true)
    const count = events.length
    lifecycle.failed({ kind: 'local-contract', code: 'invalid-proof' })
    expect(events).toHaveLength(count)
  })

  it('retains the failure through production trace rollover and NDJSON export', () => {
    const time = new FakeTraceTime()
    const diagnostics = createBrowserDiagnosticsComposition({
      build: { version: '0.0.0', mode: 'test' }, secureContext: true,
      consoleSink: { error: () => undefined }, randomBytes: size => new Uint8Array(size).fill(1),
      clock: { nowMilliseconds: () => time.nowMilliseconds(), captureTime: () => new Date(time.nowMilliseconds()).toISOString() },
      scheduler: time,
    })
    diagnostics.runtime.enable()
    const observer = diagnostics.trace.current!
    const { projected } = timedOutAttempt()
    observer(projected)
    const operation = snapshotTraceEventObservationV1({
      eventName: 'protocol_operation',
      correlation: { protocol_session_id: 'AQAAAAAAAAAAAAAAAAAAAA', protocol_operation_id: 'AgAAAAAAAAAAAAAAAAAAAA' },
      payload: { transition: 'request_sent', request_kind: 'open_revisions' },
    })
    for (let i = 0; i < 2_060; i++) observer(operation)
    const snapshot = diagnostics.trace.captureSnapshot()!
    expect(snapshot.retainedEventCount).toBe(2_048n)
    expect(snapshot.health.overwrittenCount).toBeGreaterThan(0n)
    const exported = diagnostics.runtime.export().split('\n').filter(Boolean).map(line => JSON.parse(line) as { line_type: string; record?: { event: string; payload: unknown } })
    expect(exported.find(line => line.record?.event === 'peer_attempt')?.record?.payload).toMatchObject({
      stage: 'failed', failure: { reason: 'admission-timeout' },
      summary: { last_completed_stage: 'grant_requested' },
    })
    diagnostics.runtime.clear()
    expect(diagnostics.trace.captureSnapshot()?.retainedEventCount).toBe(0n)
  })

  it('rejects unbounded failure fields and inconsistent summary durations', () => {
    const { projected } = timedOutAttempt()
    const payload = projected.payload
    expect(() => snapshotTraceEventObservationV1({ ...projected, payload: {
      ...payload, failure: { kind: 'local-transient', phase: 'admission', reason: 'provider text' },
    } } as never)).toThrow()
    expect(() => snapshotTraceEventObservationV1({ ...projected, payload: {
      ...payload, summary: { last_completed_stage: 'grant_requested', attempt_elapsed_ms: 1, stage_elapsed_ms: 2, deadline_expired: false },
    } } as never)).toThrow()
  })
})
