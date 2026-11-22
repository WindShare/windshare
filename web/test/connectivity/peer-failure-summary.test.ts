import { describe, expect, it } from 'vitest'
import { BrowserAttemptLifecycle } from '../../src/connectivity/session-signaling-lifecycle'
import type { ConnectivityTraceEvent } from '../../src/connectivity/diagnostics'
import {
  createProtocolSessionIdentity,
  createProtocolOperationIdentity,
  createPeerPathIdentityValue,
  createPeerAttemptIdentity,
} from '../../src/session/identities'
import { projectConnectivityTraceEvent } from '../../src/application/diagnostics/receive-trace'
import { createDiagnosticsRuntime } from '../../src/diagnostics'

const bytes = (seed: number) => new Uint8Array(16).fill(seed)

function timedOutAttempt() {
  let now = 0
  const events: ConnectivityTraceEvent[] = []
  const lifecycle = new BrowserAttemptLifecycle(() => ({
    protocolSessionId: createProtocolSessionIdentity(bytes(1)),
    peerPathId: createPeerPathIdentityValue(bytes(2)),
    attemptId: createPeerAttemptIdentity(bytes(3)),
    waveOrdinal: 1,
    waveAttemptOrdinal: 1,
    sessionAttemptOrdinal: 1,
  }), { current: event => events.push(event) }, () => now)
  lifecycle.phaseDeadlineArmed('negotiation', 65_000)
  for (const stage of ['offer-created', 'offer-sent', 'answer-received', 'datachannel-open'] as const) {
    lifecycle.offerMilestone(stage, { localEmitted: 1, remoteAccepted: 1 })
  }
  now = 1_000
  lifecycle.phaseDeadlineArmed('admission', 20_000)
  lifecycle.grantRequested(createProtocolOperationIdentity(4n), 2)
  now = 21_000
  lifecycle.phaseDeadlineExpired('admission', 20_000)
  lifecycle.failed({ kind: 'local-transient', phase: 'admission', reason: 'admission-timeout' })
  const last = events.at(-1)!
  return { lifecycle, events, last, projected: projectConnectivityTraceEvent(last) }
}

describe('peer failure summaries', () => {
  it('exports the exact timeout and waiting stage in one terminal record', () => {
    const { last, projected, lifecycle, events } = timedOutAttempt()
    expect(last).toMatchObject({
      stage: 'failed',
      failedAtStage: 'grant-received',
      summary: {
        lastCompletedStage: 'grant-requested',
        attemptElapsedMilliseconds: 21_000,
        stageElapsedMilliseconds: 20_000,
        deadlineExpired: true,
      },
    })
    expect(projected.payload).toMatchObject({
      failure: { kind: 'local-transient', phase: 'admission', reason: 'admission-timeout' },
      summary: {
        last_completed_stage: 'grant_requested',
        attempt_elapsed_ms: 21_000,
        stage_elapsed_ms: 20_000,
        deadline_expired: true,
      },
    })
    const count = events.length
    lifecycle.failed({ kind: 'local-contract', code: 'invalid-proof' })
    expect(events).toHaveLength(count)
  })

  it('exports projected peer failure summaries and isolates failures from trace rollover', () => {
    const runtime = createDiagnosticsRuntime({
      build: { version: '0.0.0', mode: 'test' },
      secureContext: true,
      randomBytes: size => new Uint8Array(size).fill(1),
      clock: { nowMilliseconds: () => 1_000, captureTime: () => '2026-01-01T00:00:00.000Z' },
      scheduler: { schedule: () => ({ cancel: () => undefined }) },
      maxTraceEventCount: 16,
    })
    runtime.enable()
    const observer = runtime.trace.current!
    runtime.recordFailure({ stage: 'connectivity', error: new Error('admission timed out') })
    for (let i = 0; i < 20; i++) {
      observer({
        event: 'protocol_operation',
        correlation: { protocol_session_id: 'AQAAAAAAAAAAAAAAAAAAAA', protocol_operation_id: String(i + 1) },
        payload: { transition: 'request_sent', request_kind: 'open_revisions' },
      })
    }
    const { projected } = timedOutAttempt()
    observer(projected)

    const status = runtime.status()
    expect(status.retained_event_count).toBe('16')
    expect(Number(status.overwritten_count)).toBeGreaterThan(0)
    expect(status.failure_count).toBe('1')

    const exported = runtime.export()
      .split('\n')
      .filter(Boolean)
      .map(line => JSON.parse(line) as { line_type: string; stage?: string; record?: { event: string; payload: unknown } })
    expect(exported.find(line => line.line_type === 'failure')).toMatchObject({
      line_type: 'failure',
      stage: 'connectivity',
    })
    expect(exported.find(line => line.record?.event === 'peer_attempt')?.record?.payload).toMatchObject({
      stage: 'failed',
      failure: { reason: 'admission-timeout' },
      summary: { last_completed_stage: 'grant_requested' },
    })

    runtime.clear()
    expect(runtime.status().retained_event_count).toBe('0')
    expect(runtime.status().failure_count).toBe('0')
  })
})
