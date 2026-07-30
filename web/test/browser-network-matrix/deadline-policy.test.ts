import { describe, expect, it } from 'vitest'

import {
  CONTROL_CREDENTIAL_BROKER_ACQUIRE_DEADLINE_MS,
  CONTROL_CREDENTIAL_BROKER_CALL_HARD_ENVELOPE_MS,
  CONTROL_CREDENTIAL_FORCED_RETIREMENT_MAXIMUM_MS,
  CONTROL_CREDENTIAL_NORMAL_RETIREMENT_MAXIMUM_MS,
  CONTROL_CREDENTIAL_OWNER_RETIREMENT_GRACE_MS,
  CONTROL_CREDENTIAL_RELEASE_DEADLINE_MS,
  CONTROL_CREDENTIAL_REVOKE_DEADLINE_MS,
  NETWORK_MATRIX_MILLISECONDS_PER_MINUTE,
  NETWORK_MATRIX_AUTHORITY_OUTER_SETTLEMENT_RESERVE_MS,
  NETWORK_MATRIX_SAMPLE_OUTER_SETTLEMENT_RESERVE_MS,
  PRODUCTION_NETWORK_MATRIX_FORCED_AUTHORITY_DEADLINE_MS,
  PRODUCTION_NETWORK_MATRIX_NORMAL_AUTHORITY_DEADLINE_MS,
  PRODUCTION_NETWORK_MATRIX_SAMPLE_DEADLINE_MS,
  PRODUCTION_SCHEDULED_NETWORK_MATRIX_JOB_DEADLINE_MS,
  PRODUCTION_SCHEDULED_NETWORK_MATRIX_WORKFLOW_TIMEOUT_MINUTES,
  REMOTE_AUTHORITY_REQUEST_DEADLINE_MS,
  REMOTE_NETWORK_CREDENTIAL_REQUEST_DEADLINE_MS,
  SAMPLE_CONTAINMENT_TERMINATION_GRACE_MS,
  SAMPLE_CONTROL_CREDENTIAL_PRE_ACQUIRE_DEADLINE_MS,
  SAMPLE_FILESYSTEM_CLEANUP_RESERVE_MS,
  SAMPLE_LEAF_PROCESS_DEADLINE_MS,
  SAMPLE_LEAF_OUTER_SETTLEMENT_RESERVE_MS,
  SAMPLE_MAXIMUM_CONTAINMENT_TERMINATIONS,
  SCHEDULED_JOB_EVIDENCE_FINALIZATION_RESERVE_MS,
  SCHEDULED_JOB_EVIDENCE_UPLOAD_RESERVE_MS,
  SCHEDULED_JOB_SETUP_INSTALL_BUILD_RESERVE_MS,
  SCHEDULED_JOB_OUTER_SETTLEMENT_RESERVE_MS,
  SCHEDULED_NETWORK_MATRIX_AUTHORITY_COUNT,
  SCHEDULED_NETWORK_MATRIX_SAMPLE_COUNT,
  deriveForcedNetworkMatrixAuthorityDeadlineBudget,
  deriveNetworkMatrixSampleDeadlineBudget,
  deriveNormalNetworkMatrixAuthorityDeadlineBudget,
  deriveScheduledNetworkMatrixJobDeadlineBudget,
  validateForcedNetworkMatrixAuthorityDeadlineMs,
  validateNetworkMatrixSampleDeadlineMs,
  validateNormalNetworkMatrixAuthorityDeadlineMs,
  validateScheduledNetworkMatrixJobDeadlineMs,
  validateScheduledNetworkMatrixWorkflowTimeoutMinutes,
  type NetworkMatrixNetworkCredentialRequest,
} from '../../scripts/browser-network-matrix/deadline-policy.ts'

describe('production browser network matrix deadline policy', () => {
  it('strictly contains every serial normal and forced authority stage at its maximum', () => {
    const sharedRequestMaximumMs =
      CONTROL_CREDENTIAL_BROKER_ACQUIRE_DEADLINE_MS +
      REMOTE_AUTHORITY_REQUEST_DEADLINE_MS +
      REMOTE_NETWORK_CREDENTIAL_REQUEST_DEADLINE_MS
    const normalInnerMaximumMs = sharedRequestMaximumMs +
      CONTROL_CREDENTIAL_RELEASE_DEADLINE_MS
    const forcedInnerMaximumMs = normalInnerMaximumMs +
      CONTROL_CREDENTIAL_REVOKE_DEADLINE_MS

    expect(CONTROL_CREDENTIAL_OWNER_RETIREMENT_GRACE_MS).toBe(5_000)
    expect(CONTROL_CREDENTIAL_BROKER_CALL_HARD_ENVELOPE_MS).toBe(30_000)
    expect(CONTROL_CREDENTIAL_NORMAL_RETIREMENT_MAXIMUM_MS).toBe(30_000)
    expect(CONTROL_CREDENTIAL_FORCED_RETIREMENT_MAXIMUM_MS).toBe(60_000)
    expect(deriveNormalNetworkMatrixAuthorityDeadlineBudget('stun-or-turn')).toEqual({
      retirement: 'release',
      networkCredentialRequest: 'stun-or-turn',
      serialStagesMaximumMs: normalInnerMaximumMs,
      minimumOuterDeadlineMs: normalInnerMaximumMs +
        NETWORK_MATRIX_AUTHORITY_OUTER_SETTLEMENT_RESERVE_MS,
    })
    expect(deriveForcedNetworkMatrixAuthorityDeadlineBudget('stun-or-turn')).toEqual({
      retirement: 'release-then-revoke',
      networkCredentialRequest: 'stun-or-turn',
      serialStagesMaximumMs: forcedInnerMaximumMs,
      minimumOuterDeadlineMs: forcedInnerMaximumMs +
        NETWORK_MATRIX_AUTHORITY_OUTER_SETTLEMENT_RESERVE_MS,
    })
    expect(PRODUCTION_NETWORK_MATRIX_NORMAL_AUTHORITY_DEADLINE_MS).toBe(105_000)
    expect(PRODUCTION_NETWORK_MATRIX_FORCED_AUTHORITY_DEADLINE_MS).toBe(135_000)
    expect(() => validateNormalNetworkMatrixAuthorityDeadlineMs(
      normalInnerMaximumMs,
      'stun-or-turn',
    )).toThrow(/at least 105000 ms/u)
    expect(validateNormalNetworkMatrixAuthorityDeadlineMs(
      normalInnerMaximumMs + NETWORK_MATRIX_AUTHORITY_OUTER_SETTLEMENT_RESERVE_MS,
      'stun-or-turn',
    )).toBe(normalInnerMaximumMs + NETWORK_MATRIX_AUTHORITY_OUTER_SETTLEMENT_RESERVE_MS)
    expect(() => validateForcedNetworkMatrixAuthorityDeadlineMs(
      forcedInnerMaximumMs,
      'stun-or-turn',
    )).toThrow(/at least 135000 ms/u)
    expect(validateForcedNetworkMatrixAuthorityDeadlineMs(
      forcedInnerMaximumMs + NETWORK_MATRIX_AUTHORITY_OUTER_SETTLEMENT_RESERVE_MS,
      'stun-or-turn',
    )).toBe(forcedInnerMaximumMs + NETWORK_MATRIX_AUTHORITY_OUTER_SETTLEMENT_RESERVE_MS)
  })

  it('charges the STUN or TURN request only when that authority needs it', () => {
    const withoutNetworkCredential = deriveForcedNetworkMatrixAuthorityDeadlineBudget('none')
    const withNetworkCredential = deriveForcedNetworkMatrixAuthorityDeadlineBudget('stun-or-turn')

    expect(withNetworkCredential.serialStagesMaximumMs -
      withoutNetworkCredential.serialStagesMaximumMs)
      .toBe(REMOTE_NETWORK_CREDENTIAL_REQUEST_DEADLINE_MS)
    expect(() => deriveForcedNetworkMatrixAuthorityDeadlineBudget(
      'substituted' as NetworkMatrixNetworkCredentialRequest,
    )).toThrow(/request kind is invalid/u)
  })

  it('keeps the sample outer deadline beyond leaf, containment, retirement, and filesystem maxima', () => {
    const hostileSerialMaximumMs =
      SAMPLE_CONTROL_CREDENTIAL_PRE_ACQUIRE_DEADLINE_MS +
      SAMPLE_LEAF_PROCESS_DEADLINE_MS +
      SAMPLE_MAXIMUM_CONTAINMENT_TERMINATIONS * SAMPLE_CONTAINMENT_TERMINATION_GRACE_MS +
      SAMPLE_LEAF_OUTER_SETTLEMENT_RESERVE_MS +
      CONTROL_CREDENTIAL_FORCED_RETIREMENT_MAXIMUM_MS +
      SAMPLE_FILESYSTEM_CLEANUP_RESERVE_MS
    const budget = deriveNetworkMatrixSampleDeadlineBudget()

    expect(hostileSerialMaximumMs).toBe(305_000)
    expect(budget).toEqual({
      serialStagesMaximumMs: hostileSerialMaximumMs,
      minimumOuterDeadlineMs: hostileSerialMaximumMs +
        NETWORK_MATRIX_SAMPLE_OUTER_SETTLEMENT_RESERVE_MS,
    })
    expect(PRODUCTION_NETWORK_MATRIX_SAMPLE_DEADLINE_MS).toBe(320_000)
    expect(() => validateNetworkMatrixSampleDeadlineMs(hostileSerialMaximumMs))
      .toThrow(/at least 320000 ms/u)
    expect(validateNetworkMatrixSampleDeadlineMs(
      hostileSerialMaximumMs + NETWORK_MATRIX_SAMPLE_OUTER_SETTLEMENT_RESERVE_MS,
    )).toBe(hostileSerialMaximumMs + NETWORK_MATRIX_SAMPLE_OUTER_SETTLEMENT_RESERVE_MS)
  })

  it('derives the hard job deadline from all 45 serial samples and non-sample reserves', () => {
    const authority = deriveForcedNetworkMatrixAuthorityDeadlineBudget('stun-or-turn')
    const sample = deriveNetworkMatrixSampleDeadlineBudget()
    const expectedSerialMaximumMs =
      SCHEDULED_JOB_SETUP_INSTALL_BUILD_RESERVE_MS +
      SCHEDULED_NETWORK_MATRIX_AUTHORITY_COUNT * authority.minimumOuterDeadlineMs +
      SCHEDULED_NETWORK_MATRIX_SAMPLE_COUNT * sample.minimumOuterDeadlineMs +
      SCHEDULED_JOB_EVIDENCE_FINALIZATION_RESERVE_MS +
      SCHEDULED_JOB_EVIDENCE_UPLOAD_RESERVE_MS
    const job = deriveScheduledNetworkMatrixJobDeadlineBudget()

    expect(SAMPLE_FILESYSTEM_CLEANUP_RESERVE_MS).toBeGreaterThan(0)
    expect(SCHEDULED_JOB_EVIDENCE_UPLOAD_RESERVE_MS).toBeGreaterThan(0)
    expect(SCHEDULED_JOB_OUTER_SETTLEMENT_RESERVE_MS).toBeGreaterThan(1_000)
    expect(job.serialStagesMaximumMs).toBe(expectedSerialMaximumMs)
    expect(job.minimumOuterDeadlineMs).toBeGreaterThan(expectedSerialMaximumMs)
    expect(PRODUCTION_SCHEDULED_NETWORK_MATRIX_JOB_DEADLINE_MS)
      .toBe(job.minimumOuterDeadlineMs)
    expect(job.minimumWorkflowTimeoutMinutes).toBe(
      Math.ceil(job.minimumOuterDeadlineMs / NETWORK_MATRIX_MILLISECONDS_PER_MINUTE),
    )
    expect(PRODUCTION_SCHEDULED_NETWORK_MATRIX_WORKFLOW_TIMEOUT_MINUTES)
      .toBe(job.minimumWorkflowTimeoutMinutes)
    expect(PRODUCTION_SCHEDULED_NETWORK_MATRIX_WORKFLOW_TIMEOUT_MINUTES).toBeGreaterThan(120)
    expect(PRODUCTION_SCHEDULED_NETWORK_MATRIX_JOB_DEADLINE_MS).toBeGreaterThan(
      SCHEDULED_NETWORK_MATRIX_SAMPLE_COUNT * SAMPLE_LEAF_PROCESS_DEADLINE_MS,
    )
  })

  it('rejects equality, the obsolete 120-minute cap, and malformed outer deadlines', () => {
    const job = deriveScheduledNetworkMatrixJobDeadlineBudget()

    expect(() => validateScheduledNetworkMatrixJobDeadlineMs(job.serialStagesMaximumMs))
      .toThrow(/scheduled network matrix job deadline/u)
    expect(() => validateScheduledNetworkMatrixJobDeadlineMs(job.minimumOuterDeadlineMs - 1))
      .toThrow(/scheduled network matrix job deadline/u)
    expect(validateScheduledNetworkMatrixJobDeadlineMs(job.minimumOuterDeadlineMs))
      .toBe(job.minimumOuterDeadlineMs)
    expect(() => validateScheduledNetworkMatrixWorkflowTimeoutMinutes(120))
      .toThrow(/scheduled network matrix job deadline/u)
    expect(() => validateScheduledNetworkMatrixWorkflowTimeoutMinutes(
      job.minimumWorkflowTimeoutMinutes - 1,
    )).toThrow(/scheduled network matrix job deadline/u)
    expect(validateScheduledNetworkMatrixWorkflowTimeoutMinutes(
      job.minimumWorkflowTimeoutMinutes,
    )).toBe(job.minimumWorkflowTimeoutMinutes)
    expect(job.minimumWorkflowTimeoutMinutes).toBe(307)
    expect(NETWORK_MATRIX_AUTHORITY_OUTER_SETTLEMENT_RESERVE_MS).toBeGreaterThan(1_000)
    expect(NETWORK_MATRIX_SAMPLE_OUTER_SETTLEMENT_RESERVE_MS).toBeGreaterThan(1_000)
    expect(() => validateNetworkMatrixSampleDeadlineMs(Number.NaN)).toThrow(/sample deadline/u)
    expect(() => validateScheduledNetworkMatrixWorkflowTimeoutMinutes(1.5))
      .toThrow(/positive whole minute/u)
  })
})
