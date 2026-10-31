import { emitRelayHeartbeat, emitRelayDelivery } from '../../src/diagnostics/trace/connection-payload'
import { describe, expect, it, vi } from 'vitest'
import { PeerSlotPool } from '../../src/session/peer-slots/slots'
import {
  createPeerPathIdentityValue,
  createProtocolSessionIdentity,
} from '../../src/session/identities'
import { PeerRecoverySupervisor } from '../../src/connectivity/peer-set/path'
import { snapshotTraceEventObservationV2 } from '../../src/diagnostics/export/trace-event-v2'
import { TRACE_FAILURE_DETAIL_MAX_CHARACTERS } from '../../src/diagnostics/trace/lane-payload'
import { DownloadMetrics } from '../../src/receiver/download-metrics'
import { waitingReconnectBackoff } from '../../src/receiver/recovery-clock'
import { validateTraceEventPayloadV2 } from '../../src/diagnostics/export/trace-event-payload-v2'

import { browserBuildSnapshot } from '../../src/diagnostics/build-identity'
import {
  createBrowserDiagnosticsComposition,
  type BrowserDiagnosticsClock,
} from '../../src/application/diagnostics/composition'
import { installWindShareDiagnostics } from '../../src/diagnostics/export/developer-api'
import { unclassifiedFailureFact } from '../../src/diagnostics/incident'
import type {
  TraceScheduledTask,
  TraceScheduler,
} from '../../src/diagnostics/trace/ports'
import { createSelectionSpec } from '../../src/transfer/intent'
import { nextProjectionEpoch, SelectionProjectionController } from '../../src/transfer/projection'
import {
  createConnectivityTraceSource,
  projectConnectivityTraceEvent,
  createOutputTraceSource,
  createProtocolTraceSource,
  projectProtocolTraceEvent,
  createReceiverTraceSource,
  projectReceiverTraceEvent,
} from '../../src/application/diagnostics/receive-trace'
import type { ReceiverTraceEvent } from '../../src/application/receive/observation/contracts'

it('exports scheduling decisions with session, lane, block, and completion estimates', () => {
  const composition = productionComposition()
  composition.runtime.enable()
  const source = createProtocolTraceSource(composition.trace)
  const purposes = ['content', 'probe', 'sample', 'rescue'] as const
  for (const purpose of purposes) {
    const event = {
      eventName: 'content_scheduling' as const,
      correlation: {
        protocolSessionId: createProtocolSessionIdentity(new Uint8Array(16).fill(1)),
        lane: { id: 2, epoch: 3 },
      },
      dispatchSequence: 10,
      fileId: 'AQAAAAAAAAAAAAAAAAAAAA', localBlockIndex: 7n,
      route: 'application-relay' as const, purpose,
      expectedMilliseconds: 12.25, pendingBytes: 4096, bytesPerSecond: 1000.2,
      admission: { waitedMilliseconds: 200.5, unfinishedBytes: 2048, aheadBytes: 512.2, bytesPerSecond: 1024.5 },
    }
    const projected = projectProtocolTraceEvent(event)
    expect(() => validateTraceEventPayloadV2(projected.eventName, projected.payload)).not.toThrow()
    expect(projected.payload).toMatchObject({
      dispatch_sequence: '10', block_index: '7', purpose, expected_ms: 13, bytes_per_second: 1000,
      admission: { waited_ms: 201, unfinished_bytes: '2048', ahead_bytes: '512', bytes_per_second: 1025 },
    })
    source.current?.(event)
  }
  expect(composition.runtime.status().retained_event_count).toBe(String(purposes.length))
})

it('exports request route decisions and outcomes with validated session correlation', () => {
  const composition = productionComposition()
  composition.runtime.enable()
  const source = createProtocolTraceSource(composition.trace)
  for (const transition of ['dispatched', 'completed', 'failed', 'cancelled'] as const) {
    const event = {
      eventName: 'request_scheduling' as const,
      correlation: {
        protocolSessionId: createProtocolSessionIdentity(new Uint8Array(16).fill(1)),
        lane: { id: 2, epoch: 3 },
      },
      sequence: 7, kind: 'open_revisions' as const, laneId: 2, laneEpoch: 3,
      route: 'direct' as const, transition, expectedMilliseconds: 2.25,
      pendingRequests: 1, elapsedMilliseconds: 1.25,
    }
    const projected = projectProtocolTraceEvent(event)
    expect(() => validateTraceEventPayloadV2(projected.eventName, projected.payload)).not.toThrow()
    expect(projected.payload).toMatchObject({
      request_sequence: '7', request_kind: 'open_revisions', route: 'direct', transition,
      expected_ms: 3, elapsed_ms: 2, pending_requests: 1,
    })
    source.current?.(event)
  }
  expect(composition.runtime.status().retained_event_count).toBe('4')
})

it('exports remote lease retirement independently of transfer incidents', () => {
  const composition = productionComposition()
  composition.runtime.enable()
  const source = createProtocolTraceSource(composition.trace)
  const correlation = { protocolSessionId: createProtocolSessionIdentity(new Uint8Array(16).fill(1)) }
  const identity = { eventName: 'lease_retirement' as const, correlation,
    leaseId: '01000000000000000000000000000000', attempt: 1 }
  const events = [
    { ...identity, transition: 'waiting_for_reads' as const },
    { ...identity, transition: 'deferred_for_reads' as const },
    { ...identity, transition: 'retrying' as const, failure: new Error('lane disconnected') },
    { ...identity, transition: 'released' as const },
    ...(['service_closed', 'deadline', 'remote_failure', 'barrier_failure'] as const).map(reason => ({
      ...identity, transition: 'abandoned' as const, reason, failure: new Error(reason),
    })),
  ]
  for (const event of events) {
    const projected = projectProtocolTraceEvent(event)
    expect(snapshotTraceEventObservationV2(projected)).toEqual(projected)
    expect(projected.payload).toMatchObject({ lease_id: identity.leaseId, attempt: 1, transition: event.transition })
    source.current?.(event)
  }
  expect(composition.runtime.status().retained_event_count).toBe(String(events.length))
  const projected = projectProtocolTraceEvent(events[2]!)
  for (const malformed of [
    { ...projected.payload, lease_id: 'unscoped' },
    { ...projected.payload, attempt: -1 },
    { ...projected.payload, failure_detail: 'x'.repeat(TRACE_FAILURE_DETAIL_MAX_CHARACTERS + 1) },
  ]) {
    expect(() => validateTraceEventPayloadV2('lease_retirement', malformed)).toThrow()
  }
})

it('exports a sealed unsampled final per-download record through the existing trace', () => {
    const metrics = new DownloadMetrics('01010101-0101-4101-8101-010101010101', true, () => 0)
    metrics.delivered('revision', 0n, 10n, 'direct')
    const event = {
      name: 'receive_transition' as const, transition: 'download_connectivity' as const,
      transferJobId: 'AQAAAAAAAAAAAAAAAAAAAA', connectivity: metrics.snapshot(true),
    }
    const projected = projectReceiverTraceEvent(event)
    expect(projected.eventName).toBe('receive_transition')
    expect(() => validateTraceEventPayloadV2(projected.eventName, projected.payload)).not.toThrow()
    expect(projected.payload).toMatchObject({ transition: 'download_connectivity', connectivity: {
      direct_bytes: '10', direct_fraction: 1, final: true,
    } })
    expect(() => validateTraceEventPayloadV2('receive_transition', {
      ...projected.payload, connectivity: { ...event.connectivity, incomplete: true },
    })).toThrow('incomplete')
    const composition = productionComposition()
    composition.runtime.enable()
    const source = createReceiverTraceSource(composition.trace)
    emit(source, () => event)
    expect(composition.runtime.status().retained_event_count).toBe('1')
  })

it('exports discovery scheduling decisions with bounded queue and operation context', () => {
  const composition = productionComposition()
  composition.runtime.enable()
  const source = createReceiverTraceSource(composition.trace)
  for (const decision of ['waiting', 'resumed', 'cancelled', 'complete'] as const) {
    const event = {
      name: 'receive_transition' as const, transition: 'discovery_scheduling' as const,
      operationId: 'AgAAAAAAAAAAAAAAAAAAAA',
      transferJobId: 'AQAAAAAAAAAAAAAAAAAAAA',
      queue: 'generations' as const, decision,
      pendingItems: 256, metadataBytes: 4096n,
      maximumItems: 256, maximumMetadataBytes: 16777216n,
    }
    const projected = projectReceiverTraceEvent(event)
    expect(() => validateTraceEventPayloadV2(projected.eventName, projected.payload)).not.toThrow()
    expect(projected.payload).toMatchObject({
      operation_id: event.operationId, transfer_job_id: event.transferJobId, decision,
      pending_items: '256', metadata_bytes: '4096', maximum_metadata_bytes: '16777216',
    })
    emit(source, () => event)
  }
  expect(composition.runtime.status().retained_event_count).toBe('4')
})

it('retains bounded discovery from the projection controller through trace export', async () => {
  const composition = productionComposition()
  composition.runtime.enable()
  const controller = new SelectionProjectionController(createReceiverTraceSource(composition.trace))
  const selection = await createSelectionSpec({
    shareInstance: 'AgAAAAAAAAAAAAAAAAAAAA',
    syntheticRoot: 'AQAAAAAAAAAAAAAAAAAAAA',
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const initial = controller.beginSelection(selection)
  const epoch = initial.projection.epoch
  controller.apply({ kind: 'discovery-started', epoch })
  const bounded = controller.apply({ kind: 'discovery-bounded', epoch })
  expect(bounded.discovery.kind).toBe('bounded')

  const lines = composition.runtime.export().trimEnd().split('\n').map(
    line => JSON.parse(line) as Record<string, unknown>,
  )
  expect(lines.at(-1)).toMatchObject({
    line_type: 'trace_event',
    record: {
      event: 'projection_transition',
      payload: { transition: 'refined', projection_epoch: String(epoch), discovery_state: 'bounded' },
    },
  })
  expect(composition.runtime.status().health).toMatchObject({
    trace_dropped_count: '0',
    trace_overwritten_count: '0',
  })
})

it('exports lane exception causes and bounded stacks without mutating the failure', () => {
    const cause = new Error('Peer continuation binding changed')
    const failure = new Error('Authenticated session lane failed', { cause })
    const event = {
      eventName: 'lane_transition' as const,
      transition: 'detached' as const,
      termination: { kind: 'authenticated_failure' as const, failure },
      correlation: {
        protocolSessionId: createProtocolSessionIdentity(new Uint8Array(16).fill(1)),
        lane: { id: 2, epoch: 0 },
      },
    }
    const composition = productionComposition()
    composition.runtime.enable()
    createProtocolTraceSource(composition.trace).current?.(event)
    const exported = composition.runtime.export().trim().split('\n').map(line => JSON.parse(line))
    const detail = exported.find(line => line.record?.event === 'lane_transition').record.payload.failure_detail
    expect(JSON.parse(detail)).toMatchObject({
      message: 'Authenticated session lane failed',
      stack: expect.any(String),
      cause: { message: 'Peer continuation binding changed', stack: expect.any(String) },
    })
    expect(failure.cause).toBe(cause)

    Object.defineProperty(failure, 'cause', { value: failure })
    failure.message = 'x'.repeat(TRACE_FAILURE_DETAIL_MAX_CHARACTERS * 2)
    const projected = projectProtocolTraceEvent(event)
    expect(() => validateTraceEventPayloadV2(projected.eventName, projected.payload)).not.toThrow()
    expect(() => validateTraceEventPayloadV2('lane_transition', {
      transition: 'detached', detachment_class: 'authenticated_failure',
      failure_detail: 'x'.repeat(TRACE_FAILURE_DETAIL_MAX_CHARACTERS + 1),
    })).toThrow('bounded text')
  })

it('exports initial connection recovery before a protocol session exists', () => {
  const composition = productionComposition()
  composition.runtime.enable()
  const source = createProtocolTraceSource(composition.trace)
  for (const transition of ['attempt_started', 'attempt_failed', 'waiting', 'connected', 'terminal', 'retry_requested'] as const) {
    source.current?.({
      eventName: 'connection_recovery', correlation: {}, generationId: 0,
      shareId: 'share', relayBase: 'https://relay.invalid', attempt: 2, phase: 'initial', transition,
      delayMilliseconds: 1000, failure: new Error('relay unreachable', { cause: new Error('timeout') }),
      ...(transition === 'waiting' ? { waitReason: 'capacity' } : {}),
    })
  }
  const records = composition.runtime.export().trim().split('\n').map(line => JSON.parse(line))
    .filter(line => line.record?.event === 'connection_recovery').map(line => line.record)
  expect(records).toHaveLength(6)
  expect(records[2].payload.wait_reason).toBe('capacity')
  expect(records[0].correlation).toBeUndefined()
  expect(records[0].payload).toMatchObject({ generation_id: '0', attempt: '2', share_id: 'share', delay_ms: 1000 })
  expect(JSON.parse(records[0].payload.failure_detail)).toMatchObject({ message: 'relay unreachable', cause: { message: 'timeout' } })
  for (const invalid of [
    { phase: 'rpc' }, { attempt: '-1' }, { generation_id: '01' }, { delay_ms: -1 }, { delay_ms: 99.9 }, { wait_reason: 'unknown' },
    { relay_base: '' }, { share_id: 'x'.repeat(2049) }, { failure_detail: 'x'.repeat(TRACE_FAILURE_DETAIL_MAX_CHARACTERS + 1) },
    { unexpected: 'field' },
  ]) {
    expect(() => validateTraceEventPayloadV2('connection_recovery', { ...records[0].payload, ...invalid })).toThrow()
  }
})

it.each([
  ['production jitter', waitingReconnectBackoff(() => 0.123456789), 25482, 'backoff'],
  ['remaining backoff', 99.9, 100, 'backoff'],
  ['remaining capacity wait', 99.9, 100, 'capacity'],
  ['remaining server wait', 99.9, 100, 'server'],
  ['sub-millisecond wait', 0.01, 1, 'backoff'],
  ['zero wait', 0, 0, 'backoff'],
  ['integer wait', 100, 100, 'backoff'],
  ['maximum safe duration', Number.MAX_SAFE_INTEGER, Number.MAX_SAFE_INTEGER, 'backoff'],
] as const)('records and exports reconnect %s without losing the waiting event', (_, delayMilliseconds, expected, waitReason) => {
  const composition = productionComposition()
  composition.runtime.enable()
  const source = createProtocolTraceSource(composition.trace)
  const protocolSessionId = createProtocolSessionIdentity(new Uint8Array(16).fill(1))
  const event = Object.freeze({
    eventName: 'connection_recovery' as const,
    correlation: { protocolSessionId },
    generationId: 1, shareInstanceId: 'instance', attempt: 4,
    phase: 'waiting' as const, transition: 'waiting' as const,
    delayMilliseconds, waitReason, failure: new Error('relay unavailable'),
  })

  source.current?.(event)

  const records = composition.runtime.export().trim().split('\n').map(line => JSON.parse(line))
    .filter(line => line.record?.event === 'connection_recovery').map(line => line.record)
  expect(records).toHaveLength(1)
  expect(records[0]).toMatchObject({
    correlation: { protocol_session_id: 'AQEBAQEBAQEBAQEBAQEBAQ' },
    payload: {
      generation_id: '1', share_instance_id: 'instance', attempt: '4',
      phase: 'waiting', transition: 'waiting', delay_ms: expected, wait_reason: waitReason,
    },
  })
  expect(JSON.parse(records[0].payload.failure_detail)).toMatchObject({ message: 'relay unavailable' })
  expect(composition.runtime.status()).toMatchObject({
    retained_event_count: '1',
    health: { trace_dropped_count: '0', trace_overwritten_count: '0' },
  })
})

it.each([-1, -0.1, NaN, Infinity, -Infinity, Number.MAX_SAFE_INTEGER + 1])(
  'accounts for an invalid reconnect duration %s without interrupting capture',
  delayMilliseconds => {
    const composition = productionComposition()
    composition.runtime.enable()
    const source = createProtocolTraceSource(composition.trace)
    const event = {
      eventName: 'connection_recovery' as const, correlation: {},
      generationId: 0, attempt: 1, phase: 'initial' as const,
      transition: 'waiting' as const, waitReason: 'backoff' as const, delayMilliseconds,
    }

    expect(() => source.current?.(event)).not.toThrow()
    expect(composition.runtime.status()).toMatchObject({
      retained_event_count: '0', health: { trace_dropped_count: '1' },
    })
    source.current?.({ ...event, delayMilliseconds: 100 })
    const records = composition.runtime.export().trim().split('\n').map(line => JSON.parse(line))
      .filter(line => line.record?.event === 'connection_recovery').map(line => line.record)
    expect(records).toHaveLength(1)
    expect(records[0].payload.delay_ms).toBe(100)
    expect(composition.runtime.status().health.trace_dropped_count).toBe('1')
  },
)

it('exports connection probes with available session identity and bounded queue context', () => {
  const composition = productionComposition()
  const source = createProtocolTraceSource(composition.trace)
  const context = vi.fn(() => ({
    correlation: { protocolSessionId: createProtocolSessionIdentity(new Uint8Array(16).fill(1)) },
    shareInstanceId: 'instance',
  }))
  const event = { connectionId: 4n, round: 3n, stage: 'failed' as const, bufferedBytes: 4096, elapsedMilliseconds: 44999.25, timeoutMilliseconds: 45000 }
  emitRelayHeartbeat(source, event, 'https://relay.invalid', context)
  expect(context).not.toHaveBeenCalled()
  composition.runtime.enable()
  emitRelayHeartbeat(source, event, 'https://relay.invalid', context)
  const records = composition.runtime.export().trim().split('\n').map(line => JSON.parse(line))
    .filter(line => line.record?.event === 'relay_heartbeat').map(line => line.record)
  expect(records).toHaveLength(1)
  expect(records[0].correlation.protocol_session_id).toBeDefined()
  expect(records[0].payload).toEqual({
    connection_id: '4', share_instance_id: 'instance', relay_base: 'https://relay.invalid', round: '3',
    stage: 'failed', buffered_bytes: '4096', elapsed_ms: 45000, timeout_ms: 45000,
  })
  for (const invalid of [
    { connection_id: '0' }, { round: '0' }, { stage: 'retrying' }, { elapsed_ms: -1 }, { timeout_ms: 0 },
    { buffered_bytes: '-1' }, { generation_id: '-1' },
  ]) {
    expect(() => validateTraceEventPayloadV2('relay_heartbeat', { ...records[0].payload, ...invalid })).toThrow()
  }
  expect(() => emitRelayHeartbeat({ current: () => { throw new Error('observer failed') } }, event,
    'https://relay.invalid', context)).not.toThrow()
})

it('exports relay receipt totals without losing uint64 precision or confusing discard with consumption', () => {
  const composition = productionComposition()
  const source = createProtocolTraceSource(composition.trace)
  const context = vi.fn(() => ({ correlation: {}, shareId: 'share' }))
  const received = { frames: 1n << 53n, encodedBytes: 21n << 53n }
  const consumed = { frames: received.frames - 1n, encodedBytes: received.encodedBytes - 21n }
  const zero = { frames: 0n, encodedBytes: 0n }
  const event = {
    connectionId: 'relay-receiver-4', sessionId: '0100000000000001', action: 'settlement' as const,
    capacity: { frames: 256n, encodedBytes: 16n * 1024n * 1024n },
    failure: undefined, usage: { held: zero, outstanding: zero, pendingConsumed: zero, retainedSessions: 1 },
    progress: { phase: 'settled' as const, received, consumed, consumedPrefix: consumed, discarded: { frames: 1n, encodedBytes: 21n } },
  }
  emitRelayDelivery(source, event, 'https://relay.invalid', context)
  expect(context).not.toHaveBeenCalled()
  composition.runtime.enable()
  emitRelayDelivery(source, event, 'https://relay.invalid', context)
  const records = composition.runtime.export().trim().split('\n').map(line => JSON.parse(line))
    .filter(line => line.record?.event === 'relay_delivery').map(line => line.record)
  expect(records).toHaveLength(1)
  expect(records[0].payload.progress).toMatchObject({
    received: { frames: received.frames.toString(), encoded_bytes: received.encodedBytes.toString() },
    consumed: { frames: consumed.frames.toString(), encoded_bytes: consumed.encodedBytes.toString() },
    discarded: { frames: '1', encoded_bytes: '21' },
  })
  expect(() => validateTraceEventPayloadV2('relay_delivery', records[0].payload)).not.toThrow()
  expect(() => validateTraceEventPayloadV2('relay_delivery', { ...records[0].payload, held: { frames: '-1', encoded_bytes: '0' } })).toThrow()
  expect(composition.runtime.status().health.trace_dropped_count).toBe('0')
})

it('exports operation recovery decisions with session and local operation correlation', () => {
  const composition = productionComposition()
  composition.runtime.enable()
  const source = createProtocolTraceSource(composition.trace)
  const transitions = ['retry_available_lanes', 'wait_for_availability', 'wait_for_generation', 'exhausted'] as const
  for (const transition of transitions) {
    source.current?.({
      eventName: 'operation_recovery',
      correlation: { protocolSessionId: createProtocolSessionIdentity(new Uint8Array(16).fill(1)) },
      operationSequence: 2, generationId: 1, availabilityRevision: 3,
      laneCount: 1, unchangedAvailabilityRetries: 2,
      ...(transition === 'wait_for_availability'
        ? { transition, delayMilliseconds: 199.25 } : { transition }),
    })
  }
  const records = composition.runtime.export().trim().split('\n').map(line => JSON.parse(line))
    .filter(line => line.record?.event === 'operation_recovery').map(line => line.record)
  expect(records).toHaveLength(4)
  expect(records.map(record => record.payload.transition)).toEqual(transitions)
  expect(records.every(record => record.payload.operation_sequence === '2' &&
    record.payload.availability_revision === '3' && record.correlation.protocol_session_id)).toBe(true)
  expect(records[1].payload.delay_ms).toBe(200)
  expect(() => validateTraceEventPayloadV2('operation_recovery', {
    ...records[1].payload, delay_ms: -1,
  })).toThrow()
  expect(() => validateTraceEventPayloadV2('operation_recovery', {
    ...records[0].payload, delay_ms: 200,
  })).toThrow()
})

describe('peer recovery terminal trace', () => {
  const correlation = {
    protocolSessionId: createProtocolSessionIdentity(new Uint8Array(16).fill(1)),
    peerPathId: createPeerPathIdentityValue(new Uint8Array(16).fill(2)),
  }

  it.each([
    ['runtime-closed', 'runtime_closed'],
    ['generation-retired', 'generation_retired'],
    ['binding-conflict', 'binding_conflict'],
    ['continuation-conflict', 'continuation_conflict'],
    ['protocol-failure', 'protocol_failure'],
  ] as const)('preserves session stop reason %s through projection and capture validation', (reason, exportedReason) => {
    const projected = projectConnectivityTraceEvent({
      eventName: 'peer_recovery', correlation, stage: 'session-stopped', reason,
    })

    expect(snapshotTraceEventObservationV2(projected).payload).toEqual({
      stage: 'session_stopped', reason: exportedReason,
    })
  })

  it.each([
    ['sample', 'path-sampling-started'],
    ['settle', 'sampling-work-settling'],
    ['retire', 'sampling-work-unavailable'],
    ['retire', 'sampling-work-unsettled'],
    ['retire', 'insufficient-scheduling-evidence'],
    ['abandon', 'trial-path-replaced'],
  ] as const)('exports %s with the exact trial identity and settlement evidence', (action, reason) => {
    const projected = projectConnectivityTraceEvent({
      eventName: 'peer_recovery', correlation, stage: 'parallelism', action, reason, paths: 2,
      networkWaitingBytes: 1000, inFlightBytes: 100, remainingBytes: 10_000, remainingKnown: true,
      queuedBytes: 1000, unfinishedBytes: 100, capacityWaitingBytes: 0,
      usefulRate: 100, outputRate: 100, baselineUsefulRate: 100, baselineOutputRate: 100,
      retryAfterMilliseconds: 30_000,
      trialPath: { pathId: 'AgICAgICAgICAgICAgICAg', laneId: 2, laneEpoch: 3 },
      trialWork: { offeredBytes: 400, settledBytes: 300, completedBytes: 100,
        completedAttempts: 1, failedAttempts: 1, supersededAttempts: 0, cancelledAttempts: 1, busyMilliseconds: 1500 },
    })
    expect(snapshotTraceEventObservationV2(projected).payload).toMatchObject({
      action, reason, retry_after_ms: 30_000, trial_path: { path_id: 'AgICAgICAgICAgICAgICAg', lane_id: 2, lane_epoch: 3 },
      trial_work: { offered_bytes: '400', settled_bytes: '300', completed_bytes: '100', completed_attempts: '1',
        failed_attempts: '1', superseded_attempts: '0', cancelled_attempts: '1', busy_ms: '1500' },
    })
  })

  it('rejects protocol_error as a session stop reason', () => {
    expect(() => validateTraceEventPayloadV2('peer_recovery', {
      stage: 'session_stopped', reason: 'protocol_error',
    })).toThrow('peer session stop reason')
  })

  it('retains and exports the supervisor protocol failure without dropping trace evidence', async () => {
    const composition = productionComposition()
    composition.runtime.enable()
    createReceiverTraceSource(composition.trace).current?.({
      name: 'join_transition', transition: 'started',
    })
    const createAttempt = vi.fn(() => { throw new Error('Stopped sessions cannot start attempts') })
    const supervisor = new PeerRecoverySupervisor({
      slot: new PeerSlotPool().acquire(),
      ...correlation,
      attempts: { createAttempt },
      trace: createConnectivityTraceSource(composition.trace),
    })

    await supervisor.sessionTerminated({
      authority: 'protocol-session-terminal', code: 'protocol-failure',
    })

    expect(supervisor.state).toEqual({ kind: 'session-stopped', reason: 'protocol-failure' })
    expect(createAttempt).not.toHaveBeenCalled()
    const lines = composition.runtime.export().trimEnd().split('\n').map(
      line => JSON.parse(line) as Record<string, unknown>,
    )
    expect(composition.runtime.status()).toMatchObject({
      retained_event_count: '2',
      health: { trace_dropped_count: '0' },
    })
    expect(lines.filter(line => line.line_type === 'trace_event')).toMatchObject([
      { record: { event: 'join_transition', payload: { transition: 'started' } } },
      {
        record: {
          event: 'peer_recovery',
          correlation: {
            protocol_session_id: 'AQEBAQEBAQEBAQEBAQEBAQ',
            peer_path_id: 'AgICAgICAgICAgICAgICAg',
          },
          payload: { stage: 'session_stopped', reason: 'protocol_failure' },
        },
      },
    ])
  })
})

describe('retained receive trace projection', () => {
  it.each([
    'resume-receive', 'resume-direct-zip', 'reauthorize-direct-zip',
    'verify-direct-zip-target', 'retry-direct-zip-space', 'verify-direct-zip-completion',
    'pending-catch-up', 'restoration-available', 'history-only', 'resume-package',
    'resume-local-finalization', 'save-artifact', 'retry-download',
    'cleanup-incompatible', 'retry-cleanup', 'needs-attention',
  ] as const)('exports retained continuation %s through every action milestone', (continuation) => {
    for (const name of [
      'receive.inventory.action.started',
      'receive.inventory.action.completed',
      'receive.inventory.action.failed',
    ] as const) {
      const projected = projectReceiverTraceEvent({
        name,
        retained_action: 'continue',
        continuation,
      })
      expect(projected.eventName).toBe('retained_action')
      expect(() => validateTraceEventPayloadV2(projected.eventName, projected.payload)).not.toThrow()
    }
  })

  it('distinguishes verification of a completed ZIP from receiving more content', () => {
    const projected = projectReceiverTraceEvent({
      name: 'receive.inventory.action.started',
      retained_action: 'continue',
      continuation: 'verify-direct-zip-completion',
    })
    expect(projected.payload).toEqual({
      transition: 'started',
      action: 'continue',
      continuation: 'verify_direct_zip_completion',
    })
  })

  it('keeps retained-inventory failure events closed at the typed adapter', () => {
    const projected = projectReceiverTraceEvent({
      name: 'receive.inventory.load.failed',
    })

    expect(projected).toEqual({
      eventName: 'retained_inventory',
      payload: { transition: 'load_failed' },
    })
    expect(Object.keys(projected.payload)).toEqual(['transition'])
  })
})

describe('browser diagnostics production composition', () => {
  it('uses injected package identity and the test build mode', () => {
    expect(browserBuildSnapshot()).toEqual({
      version: '0.0.0',
      mode: 'test',
    })
  })

  it('keeps the event factory untouched until tracing is explicitly enabled', () => {
    const composition = productionComposition()
    const source = createReceiverTraceSource(composition.trace)
    const createEvent = vi.fn<() => ReceiverTraceEvent>(() =>
      Object.freeze({
        name: 'join_transition',
        transition: 'started',
      }))

    emit(source, createEvent)
    expect(createEvent).not.toHaveBeenCalled()
    expect(composition.runtime.status()).toMatchObject({
      state: 'idle',
      enabled: false,
      retained_event_count: '0',
    })

    composition.runtime.enable()
    emit(source, createEvent)
    expect(createEvent).toHaveBeenCalledOnce()
    expect(composition.runtime.status()).toMatchObject({
      state: 'recording',
      enabled: true,
      retained_event_count: '1',
    })
  })

  it('exports enabled trace without writing per-event console output', () => {
    const error = vi.fn()
    const composition = productionComposition(error)
    const source = createReceiverTraceSource(composition.trace)

    composition.runtime.enable()
    emit(source, () => Object.freeze({
      name: 'join_transition',
      transition: 'started',
    }))
    const lines = composition.runtime.export().trimEnd().split('\n').map(
      (line) => JSON.parse(line) as Record<string, unknown>,
    )

    expect(lines.map((line) => line.line_type)).toEqual([
      'bundle_header',
      'trace_capture',
      'trace_event',
    ])
    expect(lines[2]).toMatchObject({
      line_type: 'trace_event',
      record: {
        event: 'join_transition',
        payload: { transition: 'started' },
      },
    })
    expect(error).not.toHaveBeenCalled()
  })

  it('projects activation decisions with stable local identity across observation replacement', () => {
    const activation = Object.freeze({
      name: 'authority_transition' as const,
      activationId: 'BQAAAAAAAAAAAAAAAAAAAA',
      authenticatedShareInstanceId: 'BgAAAAAAAAAAAAAAAAAAAA',
      selectionDigest: 'BwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA',
      artifactKind: 'directory-tree' as const,
      planKind: 'direct-tree' as const,
    })
    const started = projectReceiverTraceEvent({
      ...activation,
      transition: 'activation_started',
      observedProtocolSessionId: 'AQAAAAAAAAAAAAAAAAAAAA',
      projectionEpoch: nextProjectionEpoch(6n),
      observationRevision: 1,
    })
    const waiting = projectReceiverTraceEvent({
      ...activation,
      transition: 'prerequisite_waiting',
      waitingFor: 'resolution',
      observedProtocolSessionId: 'AgAAAAAAAAAAAAAAAAAAAA',
      projectionEpoch: nextProjectionEpoch(7n),
      observationRevision: 2,
    })

    expect(started.payload).toEqual({
      transition: 'activation_started',
      activation_id: activation.activationId,
      authenticated_share_instance_id: activation.authenticatedShareInstanceId,
      selection_digest: activation.selectionDigest,
      observed_protocol_session_id: 'AQAAAAAAAAAAAAAAAAAAAA',
      projection_epoch: '7',
      observation_revision: '1',
      artifact_kind: 'directory_tree',
      plan_kind: 'direct_tree',
    })
    expect(waiting.payload).toMatchObject({
      transition: 'prerequisite_waiting',
      activation_id: activation.activationId,
      observed_protocol_session_id: 'AgAAAAAAAAAAAAAAAAAAAA',
      projection_epoch: '8',
      observation_revision: '2',
      waiting_for: 'resolution',
    })
    expect(waiting.payload).not.toHaveProperty('protocol_operation_id')
  })

  it('projects closed retry, invalidation, commit-result, and cleanup decision fields', () => {
    const activation = Object.freeze({
      name: 'authority_transition' as const,
      activationId: 'BQAAAAAAAAAAAAAAAAAAAA',
      authenticatedShareInstanceId: 'BgAAAAAAAAAAAAAAAAAAAA',
      selectionDigest: 'BwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA',
      observedProtocolSessionId: 'AQAAAAAAAAAAAAAAAAAAAA',
      projectionEpoch: nextProjectionEpoch(8n),
      observationRevision: 3,
      artifactKind: 'directory-tree' as const,
      planKind: 'direct-tree' as const,
    })

    expect(projectReceiverTraceEvent({
      ...activation,
      transition: 'retry_required',
      retryableDiscoveryReason: 'receiver-reconnecting',
    }).payload).toMatchObject({
      transition: 'retry_required',
      retryable_discovery_reason: 'receiver_reconnecting',
    })
    expect(projectReceiverTraceEvent({
      ...activation,
      transition: 'semantic_invalidated',
      invalidationReason: 'installed-route-changed',
    }).payload).toMatchObject({
      transition: 'semantic_invalidated',
      invalidation_reason: 'installed_route_changed',
    })
    expect(projectReceiverTraceEvent({
      ...activation,
      transition: 'commit_owned_effects',
      receiverOperationId: 'AwAAAAAAAAAAAAAAAAAAAA',
    }).payload).toMatchObject({
      transition: 'commit_owned_effects',
      receiver_operation_id: 'AwAAAAAAAAAAAAAAAAAAAA',
    })
    expect(projectReceiverTraceEvent({
      ...activation,
      transition: 'commit_pre_cut_retry',
      receiverOperationId: 'AgAAAAAAAAAAAAAAAAAAAA',
    }).payload).toEqual({
      transition: 'commit_pre_cut_retry',
      activation_id: activation.activationId,
      authenticated_share_instance_id: activation.authenticatedShareInstanceId,
      selection_digest: activation.selectionDigest,
      observed_protocol_session_id: activation.observedProtocolSessionId,
      projection_epoch: '9',
      observation_revision: '3',
      artifact_kind: 'directory_tree',
      plan_kind: 'direct_tree',
      receiver_operation_id: 'AgAAAAAAAAAAAAAAAAAAAA',
    })
    expect(projectReceiverTraceEvent({
      ...activation,
      transition: 'cleanup_failed',
      receiverOperationId: 'AwAAAAAAAAAAAAAAAAAAAA',
      failedStage: 'detach',
    }).payload).toEqual({
      transition: 'cleanup_failed',
      activation_id: activation.activationId,
      authenticated_share_instance_id: activation.authenticatedShareInstanceId,
      selection_digest: activation.selectionDigest,
      observed_protocol_session_id: activation.observedProtocolSessionId,
      projection_epoch: '9',
      observation_revision: '3',
      artifact_kind: 'directory_tree',
      plan_kind: 'direct_tree',
      receiver_operation_id: 'AwAAAAAAAAAAAAAAAAAAAA',
      failed_stage: 'detach',
    })
    expect(projectReceiverTraceEvent({
      ...activation,
      transition: 'cleanup_completed',
    }).payload).not.toHaveProperty('receiver_operation_id')
  })

  it('exports worker-family consequence diagnostics with stable operation context', () => {
    const composition = productionComposition()
    const source = createReceiverTraceSource(composition.trace)

    composition.runtime.enable()
    emit(source, () => Object.freeze({
      name: 'receive_transition',
      transition: 'worker_consequence_observed',
      workerFamily: 'prepared-files',
      failureSource: Object.freeze({ kind: 'worker', workerIndex: 2 }),
      operationId: 'BQAAAAAAAAAAAAAAAAAAAA',
      transferJobId: 'BgAAAAAAAAAAAAAAAAAAAA',
      protocolSessionId: 'AQAAAAAAAAAAAAAAAAAAAA',
      protocolGeneration: 3,
      outputSessionId: 'AgAAAAAAAAAAAAAAAAAAAA',
    }))
    const lines = composition.runtime.export().trimEnd().split('\n').map(
      line => JSON.parse(line) as Record<string, unknown>,
    )

    expect(lines.at(-1)).toMatchObject({
      line_type: 'trace_event',
      record: {
        event: 'receive_transition',
        payload: {
          transition: 'worker_consequence_observed',
          worker_family: 'prepared_files',
          failure_source: 'worker',
          failure_source_index: 2,
          operation_id: 'BQAAAAAAAAAAAAAAAAAAAA',
          transfer_job_id: 'BgAAAAAAAAAAAAAAAAAAAA',
          protocol_session_id: 'AQAAAAAAAAAAAAAAAAAAAA',
          protocol_generation: 3,
          output_session_id: 'AgAAAAAAAAAAAAAAAAAAAA',
        },
      },
    })
  })

  it('projects complete capacity scheduling progress independently from file outcomes', () => {
    const projected = projectReceiverTraceEvent(Object.freeze({
      name: 'transfer_progress',
      operationId: 'BQAAAAAAAAAAAAAAAAAAAA',
      transferJobId: 'BgAAAAAAAAAAAAAAAAAAAA',
      discoveredFiles: 2n,
      discoveredBytes: 10n,
      receivedObjectBytes: 8n,
      writtenBytes: 4n,
      completedFiles: 1n,
      completedBytes: 4n,
      fileErrors: 0n,
      selectionErrors: 0n,
      failedDirectories: 0n,
      capacityWaitingFiles: 1n,
      capacityAccumulatedWaitMilliseconds: 125,
      capacityWaitAttempts: 2n,
      capacityWaitVisible: true,
      contentLanes: 2,
      discovery: 'open',
      partial: false,
    }))

    expect(projected.payload).toMatchObject({
      operation_id: 'BQAAAAAAAAAAAAAAAAAAAA',
      transfer_job_id: 'BgAAAAAAAAAAAAAAAAAAAA',
      capacity_waiting_files: '1',
      capacity_accumulated_wait_ms: '125',
      capacity_wait_attempts: '2',
      capacity_wait_visible: true,
      file_errors: '0',
      partial: false,
    })
  })

  it('exports authenticated capacity retry scheduling without file or path data', () => {
    const projected = projectReceiverTraceEvent(Object.freeze({
      name: 'receive_transition',
      transition: 'capacity_retry_scheduled',
      capacityWaitId: 'BwAAAAAAAAAAAAAAAAAAAA',
      capacitySurface: 'revision_open',
      receiveOperationId: 'BQAAAAAAAAAAAAAAAAAAAA',
      transferJobId: 'BgAAAAAAAAAAAAAAAAAAAA',
      protocolSessionId: 'AQAAAAAAAAAAAAAAAAAAAA',
      protocolOperationId: 'AgAAAAAAAAAAAAAAAAAAAA',
      attempt: 2n,
      senderHintMilliseconds: 300,
      jitterMilliseconds: 17,
      delayMilliseconds: 317,
      accumulatedWaitMilliseconds: 125,
      activeWaiters: 2,
    }))

    expect(projected).toEqual({
      eventName: 'receive_transition',
      payload: {
        transition: 'capacity_retry_scheduled',
        capacity_wait_id: 'BwAAAAAAAAAAAAAAAAAAAA',
        capacity_surface: 'revision_open',
        receive_operation_id: 'BQAAAAAAAAAAAAAAAAAAAA',
        transfer_job_id: 'BgAAAAAAAAAAAAAAAAAAAA',
        protocol_session_id: 'AQAAAAAAAAAAAAAAAAAAAA',
        protocol_operation_id: 'AgAAAAAAAAAAAAAAAAAAAA',
        attempt: '2',
        sender_hint_ms: 300,
        jitter_ms: 17,
        delay_ms: 317,
        accumulated_wait_ms: 125,
        active_waiters: 2,
      },
    })
    expect(JSON.stringify(projected)).not.toMatch(/file|path/iu)
  })

  it('retains and exports an incident even when the console sink throws', () => {
    const error = vi.fn(() => {
      throw new Error('console unavailable')
    })
    const composition = productionComposition(error)
    const owner = composition.incidents.openScope('join')
    const trigger = owner.facts.record(unclassifiedFailureFact({
      stage: 'join',
      recoveryDisposition: 'terminal',
    }), 'contributor')

    composition.incidents.submitDecision(owner.handle, {
      kind: 'incident',
      boundary: 'join',
      outcome: 'failed',
      trigger,
    })
    owner.close()

    expect(error).toHaveBeenCalledOnce()
    expect(composition.runtime.inspectLastFailure()).toMatchObject({
      event: 'failure_incident',
      payload: {
        scope: { scope_kind: 'join', scope_sequence: '1' },
        presentation: { boundary: 'join', outcome: 'failed' },
      },
    })
    expect(composition.runtime.export()).toContain('"line_type":"incident"')
  })

  it('installs one frozen readonly developer facade without product dependencies', () => {
    const composition = productionComposition()
    const target = {}
    const api = installWindShareDiagnostics(target, composition.runtime)
    const descriptor = Object.getOwnPropertyDescriptor(
      target,
      'windshareDiagnostics',
    )

    expect(Object.isFrozen(api)).toBe(true)
    expect(descriptor).toMatchObject({
      configurable: false,
      enumerable: false,
      writable: false,
      value: api,
    })
    expect(api.status()).toMatchObject({ state: 'idle', enabled: false })
  })
})

describe('checkpoint authority production trace', () => {
  it('accepts stable decisions through the production recorder', () => {
    const composition = productionComposition()
    const source = createOutputTraceSource(composition.trace)
    composition.runtime.enable()

    source.current?.({
      eventName: 'checkpoint',
      payload: Object.freeze({
        backend: 'file_system_access',
        transition: 'authority_decision',
        authority: 'automatic_admission',
        receive_operation_id: 'AQAAAAAAAAAAAAAAAAAAAA',
        transfer_job_id: 'AgAAAAAAAAAAAAAAAAAAAA',
        output_session_id: 'AwAAAAAAAAAAAAAAAAAAAA',
        materialization_relative_path: Object.freeze(['folder', 'large.bin']),
        trigger: 'pending_bytes',
        checkpoint_ordinal: 1,
        prefix_copy_bytes: '67108864',
        write_amplification_bytes: '67108864',
        temporary_bytes: '67108864',
        remaining_automatic_write_amplification_bytes: '2080374784',
        decision: 'admitted',
      }),
    })

    const lines = composition.runtime.export().trimEnd().split('\n').map(
      line => JSON.parse(line) as Record<string, unknown>,
    )
    expect(lines.at(-1)).toMatchObject({
      line_type: 'trace_event',
      record: {
        event: 'checkpoint',
        payload: {
          transition: 'authority_decision',
          receive_operation_id: 'AQAAAAAAAAAAAAAAAAAAAA',
          transfer_job_id: 'AgAAAAAAAAAAAAAAAAAAAA',
          output_session_id: 'AwAAAAAAAAAAAAAAAAAAAA',
          decision: 'admitted',
        },
      },
    })
  })
})

function productionComposition(error = vi.fn()) {
  let now = 1_000
  const clock: BrowserDiagnosticsClock = Object.freeze({
    nowMilliseconds: () => now++,
    captureTime: () => new Date(now++).toISOString(),
  })
  const scheduler: TraceScheduler = Object.freeze({
    schedule: (): TraceScheduledTask => Object.freeze({ cancel: () => undefined }),
  })
  return createBrowserDiagnosticsComposition({
    build: browserBuildSnapshot(),
    secureContext: true,
    consoleSink: Object.freeze({ error }),
    randomBytes: (byteLength) =>
      Uint8Array.from({ length: byteLength }, (_, index) => index + 1),
    clock,
    scheduler,
  })
}

function emit(
  source: ReturnType<typeof createReceiverTraceSource>,
  createEvent: () => ReceiverTraceEvent,
): void {
  const observer = source.current
  if (observer === undefined) return
  observer(createEvent())
}
