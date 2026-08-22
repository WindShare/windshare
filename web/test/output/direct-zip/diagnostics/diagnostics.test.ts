import { describe, expect, it, vi } from 'vitest'

import type { OutputTraceEvent } from '../../../../src/output/diagnostics'
import {
  BoundedDirectZipDiagnosticHistory,
  observeDirectZipDiagnostic,
  projectDirectZipDiagnosticHistoryV1,
  projectDirectZipDiagnosticV1,
  snapshotDirectZipLocalDiagnostic,
  type DirectZipDiagnosticMilestoneInput,
} from '../../../../src/output/direct-zip/diagnostics'

const OPERATION_ID = 'AQAAAAAAAAAAAAAAAAAAAA'
const SESSION_ID = 'AgAAAAAAAAAAAAAAAAAAAA'

function milestone(
  overrides: Partial<DirectZipDiagnosticMilestoneInput> = {},
): DirectZipDiagnosticMilestoneInput {
  return {
    operationId: OPERATION_ID,
    sessionId: SESSION_ID,
    planKind: 'direct-resumable-zip',
    milestone: 'checkpoint-policy-decided',
    checkpointPhase: 'inside-member',
    epochOffsetClass: 'member-payload',
    decisions: {
      prefixCopy: 'decline-prefix-copy-budget',
      peakSpace: 'confirmation-required',
      permission: 'granted',
      identity: 'verified',
      space: 'admitted',
      cleanup: 'not-requested',
    },
    ...overrides,
  }
}

describe('Direct ZIP diagnostic projection', () => {
  it('retains raw local facts while exporting only closed content-free facts', () => {
    const rawStageFacts = { stableName: 'private-file-name.zip', targetLength: 12_345 }
    const rawException = new DOMException('C:/private/provider-message.txt', 'QuotaExceededError')
    const local = snapshotDirectZipLocalDiagnostic(milestone({
      rawFsaStageFacts: rawStageFacts,
      rawException,
    }), 1_234)

    expect(local.rawFsaStageFacts).toBe(rawStageFacts)
    expect(local.rawException).toBe(rawException)
    expect(local.rawFsaStageFactsObserved).toBe(true)
    expect(local.rawExceptionObserved).toBe(true)

    const exported = projectDirectZipDiagnosticV1(local)
    expect(exported).toEqual({
      operation_id: OPERATION_ID,
      session_id: SESSION_ID,
      plan_kind: 'direct_resumable_zip',
      milestone: 'checkpoint_policy_decided',
      checkpoint_phase: 'inside_member',
      epoch_offset_class: 'member_payload',
      prefix_copy_decision: 'decline_prefix_copy_budget',
      peak_space_decision: 'confirmation_required',
      permission_decision: 'granted',
      identity_decision: 'verified',
      space_decision: 'admitted',
      cleanup_decision: 'not_requested',
      native_error_class: 'quota_exceeded',
    })
    expect(JSON.stringify(exported)).not.toContain('private')
    expect(JSON.stringify(exported)).not.toContain('12345')
    expect(Object.isFrozen(exported)).toBe(true)
  })

  it('bounds local retention and its independent export projection', () => {
    const events: OutputTraceEvent[] = []
    let now = 10
    const history = new BoundedDirectZipDiagnosticHistory({
      capacity: 2,
      clock: { nowMilliseconds: () => now++ },
      trace: { current: event => events.push(event) },
    })

    history.observe(milestone({ milestone: 'session-started' }))
    history.observe(milestone({ milestone: 'epoch-opened' }))
    history.observe(milestone({ milestone: 'checkpoint-promoted' }))

    expect(history.snapshot().map(record => record.milestone)).toEqual([
      'epoch-opened',
      'checkpoint-promoted',
    ])
    expect(history.droppedCount()).toBe(1n)
    expect(events).toHaveLength(3)
    expect(events.every(event => event.eventName === 'direct_zip_milestone')).toBe(true)
    expect(projectDirectZipDiagnosticHistoryV1(history.snapshot(), 1)).toEqual([
      expect.objectContaining({ milestone: 'checkpoint_promoted' }),
    ])
  })

  it('cannot perturb authority when clocks, observers, or lazy factories fail', () => {
    const failingClock = new BoundedDirectZipDiagnosticHistory({
      clock: { nowMilliseconds: () => { throw new Error('clock failed') } },
    })
    expect(() => failingClock.observe(milestone())).not.toThrow()
    expect(failingClock.snapshot()).toEqual([])
    expect(failingClock.droppedCount()).toBe(1n)

    const retained = new BoundedDirectZipDiagnosticHistory({
      clock: { nowMilliseconds: () => 1 },
      trace: { current: () => { throw new Error('trace failed') } },
    })
    expect(() => retained.observe(milestone())).not.toThrow()
    expect(retained.snapshot()).toHaveLength(1)

    const factory = vi.fn(() => milestone())
    observeDirectZipDiagnostic(undefined, factory)
    expect(factory).not.toHaveBeenCalled()
    expect(() => observeDirectZipDiagnostic({
      observe: () => { throw new Error('observer failed') },
    }, factory)).not.toThrow()
  })

  it('rejects noncanonical identities and invalid capacities at pure boundaries', () => {
    expect(() => snapshotDirectZipLocalDiagnostic(milestone({ operationId: 'private' }), 1))
      .toThrow(/canonical non-zero 16-byte identity/u)
    expect(() => projectDirectZipDiagnosticHistoryV1([], 0)).toThrow(/between 1 and 32/u)
    expect(() => projectDirectZipDiagnosticHistoryV1([], 33)).toThrow(/between 1 and 32/u)
    expect(() => new BoundedDirectZipDiagnosticHistory({
      clock: { nowMilliseconds: () => 1 },
      capacity: 65,
    })).toThrow(/between 1 and 64/u)
    expect(() => snapshotDirectZipLocalDiagnostic(milestone({
      milestone: 'open-text' as DirectZipDiagnosticMilestoneInput['milestone'],
    }), 1)).toThrow(/milestone is invalid/u)
  })
})
