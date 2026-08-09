import { describe, expect, it, vi } from 'vitest'

import {
  createPrivacySafeV2ReceiverTraceSink,
  privacySafeV2ReceiverTrace,
  type PrivacySafeV2ReceiverTrace,
} from '../../src/ui/v2-production-trace'

describe('v2 production trace privacy boundary', () => {
  it('retains normalized materialization facts while replacing opaque identities with presence', () => {
    const safe = privacySafeV2ReceiverTrace({
      name: 'receive.materialization.failed',
      operation_id: 'operation-secret',
      receive_intent_digest: 'intent-secret',
      protocol_session_id: 'session-secret',
      transfer_job_id: 'job-secret',
      plan_kind: 'workspace-then-publish',
      directory_failure_reason: 'output-write-failed',
      completed_file_count: 0n,
      completed_bytes: 0n,
    })

    expect(safe).toEqual({
      name: 'receive.materialization.failed',
      plan_kind: 'workspace-then-publish',
      directory_failure_reason: 'output-write-failed',
      completed_file_count: 0n,
      completed_bytes: 0n,
      has_operation_id: true,
      has_receive_intent_digest: true,
      has_protocol_session_id: true,
      has_transfer_job_id: true,
    })
    expect(Object.values(safe)).not.toContain('operation-secret')
    expect(Object.values(safe)).not.toContain('intent-secret')
    expect(Object.values(safe)).not.toContain('session-secret')
    expect(Object.values(safe)).not.toContain('job-secret')
  })

  it('projects workspace preparation milestones without exposing authority identities', () => {
    const safe = privacySafeV2ReceiverTrace({
      name: 'receive.preparation_admission.accepted',
      operation_id: 'operation-secret',
      receive_intent_digest: 'intent-secret',
      plan_kind: 'workspace-then-publish',
      admission_kind: 'workspace-budget',
      artifact_bytes: 68n,
      metadata_bytes: 256n,
      unique_raw_bytes: 68n,
      package_bytes: 512n,
      peak_temporary_bytes: 580n,
      durable_metadata_bytes: 384n,
      peak_owned_bytes: 964n,
      limit_class: 'none',
    })

    expect(safe).toEqual({
      name: 'receive.preparation_admission.accepted',
      plan_kind: 'workspace-then-publish',
      admission_kind: 'workspace-budget',
      artifact_bytes: 68n,
      metadata_bytes: 256n,
      unique_raw_bytes: 68n,
      package_bytes: 512n,
      peak_temporary_bytes: 580n,
      durable_metadata_bytes: 384n,
      peak_owned_bytes: 964n,
      limit_class: 'none',
      has_operation_id: true,
      has_receive_intent_digest: true,
    })
    expect(Object.values(safe)).not.toContain('operation-secret')
    expect(Object.values(safe)).not.toContain('intent-secret')
  })

  it('replaces workspace preparation and publication bindings with presence facts', () => {
    const safe = privacySafeV2ReceiverTrace({
      name: 'receive.package.sealed',
      operation_id: 'operation-secret',
      package_digest: 'package-secret',
      layout_digest: 'layout-secret',
      artifact_bytes: 68n,
    })

    expect(safe).toEqual({
      name: 'receive.package.sealed',
      artifact_bytes: 68n,
      has_operation_id: true,
      has_package_digest: true,
      has_layout_digest: true,
    })
    expect(Object.values(safe)).not.toContain('package-secret')
    expect(Object.values(safe)).not.toContain('layout-secret')
  })

  it('keeps reopen decisions while redacting operation, intent, and lease identities', () => {
    const safe = privacySafeV2ReceiverTrace({
      name: 'receive.operation.reopen_authorized',
      operation_id: 'operation-secret',
      receive_intent_digest: 'intent-secret',
      lifecycle_generation: 7n,
      continuation: 'save-artifact',
      lease_id: 'lease-secret',
    })

    expect(safe).toEqual({
      name: 'receive.operation.reopen_authorized',
      lifecycle_generation: 7n,
      continuation: 'save-artifact',
      has_operation_id: true,
      has_receive_intent_digest: true,
      has_lease_id: true,
    })
    expect(Object.values(safe)).not.toContain('lease-secret')
  })

  it('cannot serialize future exception, path, name, or capability fields', () => {
    const unsafe = {
      name: 'receive.materialization.failed',
      plan_kind: 'portable-handoff',
      directory_failure_reason: 'output-commit-failed',
      completed_file_count: 0n,
      completed_bytes: 0n,
      error_name: 'TypeError',
      error_message: 'failed at private/report.txt',
      source_path: 'private/report.txt',
      suggested_name: 'report.txt',
      capability_input: '#secret',
    } as unknown as Parameters<typeof privacySafeV2ReceiverTrace>[0]

    expect(privacySafeV2ReceiverTrace(unsafe)).toEqual({
      name: 'receive.materialization.failed',
      plan_kind: 'portable-handoff',
      directory_failure_reason: 'output-commit-failed',
      completed_file_count: 0n,
      completed_bytes: 0n,
    })
  })

  it('writes one structured allowlisted event through the injected console', () => {
    const info = vi.fn<(label: string, event: PrivacySafeV2ReceiverTrace) => void>()
    const sink = createPrivacySafeV2ReceiverTraceSink({ info })

    sink({
      name: 'receive.intent.frozen',
      operation_id: 'operation-secret',
      receive_intent_digest: 'intent-secret',
      artifact_kind: 'zip-archive',
      layout_class: 'zip-result-root',
      plan_kind: 'workspace-then-publish',
    })

    expect(info).toHaveBeenCalledWith('windshare.receive', {
      name: 'receive.intent.frozen',
      artifact_kind: 'zip-archive',
      layout_class: 'zip-result-root',
      plan_kind: 'workspace-then-publish',
      has_operation_id: true,
      has_receive_intent_digest: true,
    })
  })
})
