import { describe, expect, it, vi } from 'vitest'

import { browserBuildSnapshot } from '../../src/diagnostics/build-identity'
import {
  createBrowserDiagnosticsComposition,
  type BrowserDiagnosticsClock,
} from '../../src/diagnostics/browser-composition'
import { installWindShareDiagnostics } from '../../src/diagnostics/export/developer-api'
import { unclassifiedFailureFact } from '../../src/diagnostics/incident'
import type {
  TraceScheduledTask,
  TraceScheduler,
} from '../../src/diagnostics/trace/ports'
import { nextProjectionEpoch } from '../../src/transfer/projection'
import {
  createOutputTraceSource,
  createV2ReceiverTraceSource,
  projectV2ReceiverTraceEvent,
} from '../../src/ui/v2-production-trace'
import type { V2ReceiverTraceEvent } from '../../src/ui/v2-controller'

describe('browser diagnostics production composition', () => {
  it('uses injected package identity and the test build mode', () => {
    expect(browserBuildSnapshot()).toEqual({
      version: '0.0.0',
      mode: 'test',
    })
  })

  it('keeps the event factory untouched until tracing is explicitly enabled', () => {
    const composition = productionComposition()
    const source = createV2ReceiverTraceSource(composition.trace)
    const createEvent = vi.fn<() => V2ReceiverTraceEvent>(() =>
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
      state: 'recording_pre_failure',
      enabled: true,
      retained_event_count: '1',
    })
  })

  it('exports enabled trace without writing per-event console output', () => {
    const error = vi.fn()
    const composition = productionComposition(error)
    const source = createV2ReceiverTraceSource(composition.trace)

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

  it('keeps retained-inventory failure events closed at the typed adapter', () => {
    const projected = projectV2ReceiverTraceEvent({
      name: 'receive.inventory.load.failed',
    })

    expect(projected).toEqual({
      eventName: 'retained_inventory',
      payload: { transition: 'load_failed' },
    })
    expect(Object.keys(projected.payload)).toEqual(['transition'])
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
    const started = projectV2ReceiverTraceEvent({
      ...activation,
      transition: 'activation_started',
      observedProtocolSessionId: 'AQAAAAAAAAAAAAAAAAAAAA',
      projectionEpoch: nextProjectionEpoch(6n),
      observationRevision: 1,
    })
    const waiting = projectV2ReceiverTraceEvent({
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

    expect(projectV2ReceiverTraceEvent({
      ...activation,
      transition: 'retry_required',
      retryableDiscoveryReason: 'receiver-reconnecting',
    }).payload).toMatchObject({
      transition: 'retry_required',
      retryable_discovery_reason: 'receiver_reconnecting',
    })
    expect(projectV2ReceiverTraceEvent({
      ...activation,
      transition: 'semantic_invalidated',
      invalidationReason: 'installed-route-changed',
    }).payload).toMatchObject({
      transition: 'semantic_invalidated',
      invalidation_reason: 'installed_route_changed',
    })
    expect(projectV2ReceiverTraceEvent({
      ...activation,
      transition: 'commit_owned_effects',
      receiverOperationId: 'AwAAAAAAAAAAAAAAAAAAAA',
    }).payload).toMatchObject({
      transition: 'commit_owned_effects',
      receiver_operation_id: 'AwAAAAAAAAAAAAAAAAAAAA',
    })
    expect(projectV2ReceiverTraceEvent({
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
    expect(projectV2ReceiverTraceEvent({
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
    expect(projectV2ReceiverTraceEvent({
      ...activation,
      transition: 'cleanup_completed',
    }).payload).not.toHaveProperty('receiver_operation_id')
  })

  it('exports worker-family consequence diagnostics with stable operation context', () => {
    const composition = productionComposition()
    const source = createV2ReceiverTraceSource(composition.trace)

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
    const projected = projectV2ReceiverTraceEvent(Object.freeze({
      name: 'transfer_progress',
      discoveredFiles: 2n,
      discoveredBytes: 10n,
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
      capacity_waiting_files: '1',
      capacity_accumulated_wait_ms: '125',
      capacity_wait_attempts: '2',
      capacity_wait_visible: true,
      file_errors: '0',
      partial: false,
    })
  })

  it('exports authenticated capacity retry scheduling without file or path data', () => {
    const projected = projectV2ReceiverTraceEvent(Object.freeze({
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
  source: ReturnType<typeof createV2ReceiverTraceSource>,
  createEvent: () => V2ReceiverTraceEvent,
): void {
  const observer = source.current
  if (observer === undefined) return
  observer(createEvent())
}
