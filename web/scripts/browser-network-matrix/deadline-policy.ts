export const CONTROL_CREDENTIAL_BROKER_CHILD_DEADLINE_MS = 15_000 as const
export const REMOTE_AUTHORITY_REQUEST_DEADLINE_MS = 15_000 as const
export const REMOTE_NETWORK_CREDENTIAL_REQUEST_DEADLINE_MS = 15_000 as const
export const CONTROL_CREDENTIAL_OWNER_RETIREMENT_GRACE_MS = 5_000 as const
export const CONTROL_CREDENTIAL_BROKER_MAXIMUM_TERMINATIONS = 2 as const
export const CONTROL_CREDENTIAL_WINDOWS_WATCHDOG_RESERVE_MS = 5_000 as const
export const CONTROL_CREDENTIAL_WINDOWS_POST_KILL_RESERVE_MS = 5_000 as const

// Every acquire/release/revoke call is itself an owned child. The hard envelope
// is the larger of Linux's two cleanup phases and Windows' grace/watchdog/lease.
export const CONTROL_CREDENTIAL_BROKER_CALL_HARD_ENVELOPE_MS = Math.max(
  CONTROL_CREDENTIAL_BROKER_CHILD_DEADLINE_MS +
    CONTROL_CREDENTIAL_BROKER_MAXIMUM_TERMINATIONS *
      CONTROL_CREDENTIAL_OWNER_RETIREMENT_GRACE_MS,
  CONTROL_CREDENTIAL_BROKER_CHILD_DEADLINE_MS +
    CONTROL_CREDENTIAL_OWNER_RETIREMENT_GRACE_MS +
    CONTROL_CREDENTIAL_WINDOWS_WATCHDOG_RESERVE_MS +
    CONTROL_CREDENTIAL_WINDOWS_POST_KILL_RESERVE_MS,
)
export const CONTROL_CREDENTIAL_BROKER_ACQUIRE_DEADLINE_MS =
  CONTROL_CREDENTIAL_BROKER_CALL_HARD_ENVELOPE_MS
export const CONTROL_CREDENTIAL_RELEASE_DEADLINE_MS =
  CONTROL_CREDENTIAL_BROKER_CALL_HARD_ENVELOPE_MS
export const CONTROL_CREDENTIAL_REVOKE_DEADLINE_MS =
  CONTROL_CREDENTIAL_BROKER_CALL_HARD_ENVELOPE_MS

export const SAMPLE_CONTROL_CREDENTIAL_PRE_ACQUIRE_DEADLINE_MS =
  CONTROL_CREDENTIAL_BROKER_CALL_HARD_ENVELOPE_MS
export const SAMPLE_LEAF_PROCESS_DEADLINE_MS = 180_000 as const
export const SAMPLE_CONTAINMENT_TERMINATION_GRACE_MS = 10_000 as const
export const SAMPLE_MAXIMUM_CONTAINMENT_TERMINATIONS = 2 as const
export const SAMPLE_LEAF_OUTER_SETTLEMENT_RESERVE_MS = 5_000 as const
export const SAMPLE_FILESYSTEM_CLEANUP_RESERVE_MS = 10_000 as const

export const SCHEDULED_NETWORK_MATRIX_SAMPLE_COUNT = 45 as const
export const SCHEDULED_NETWORK_MATRIX_AUTHORITY_COUNT = 3 as const
export const SCHEDULED_JOB_SETUP_INSTALL_BUILD_RESERVE_MS = 1_800_000 as const
export const SCHEDULED_JOB_EVIDENCE_FINALIZATION_RESERVE_MS = 600_000 as const
export const SCHEDULED_JOB_EVIDENCE_UPLOAD_RESERVE_MS = 600_000 as const
export const SCHEDULED_JOB_OUTER_SETTLEMENT_RESERVE_MS = 600_000 as const

export const NETWORK_MATRIX_AUTHORITY_OUTER_SETTLEMENT_RESERVE_MS = 15_000 as const
export const NETWORK_MATRIX_SAMPLE_OUTER_SETTLEMENT_RESERVE_MS = 15_000 as const
export const NETWORK_MATRIX_OUTER_SETTLEMENT_RESERVE_MS =
  NETWORK_MATRIX_SAMPLE_OUTER_SETTLEMENT_RESERVE_MS
export const NETWORK_MATRIX_MILLISECONDS_PER_MINUTE = 60_000 as const

export type NetworkMatrixNetworkCredentialRequest = 'none' | 'stun-or-turn'

export interface NetworkMatrixDeadlineBudget {
  readonly serialStagesMaximumMs: number
  readonly minimumOuterDeadlineMs: number
}

export interface NetworkMatrixAuthorityDeadlineBudget extends NetworkMatrixDeadlineBudget {
  readonly retirement: 'release' | 'release-then-revoke'
  readonly networkCredentialRequest: NetworkMatrixNetworkCredentialRequest
}

export interface ScheduledNetworkMatrixJobDeadlineBudget extends NetworkMatrixDeadlineBudget {
  readonly minimumWorkflowTimeoutMinutes: number
}

export const CONTROL_CREDENTIAL_NORMAL_RETIREMENT_MAXIMUM_MS =
  CONTROL_CREDENTIAL_RELEASE_DEADLINE_MS

export const CONTROL_CREDENTIAL_FORCED_RETIREMENT_MAXIMUM_MS =
  CONTROL_CREDENTIAL_NORMAL_RETIREMENT_MAXIMUM_MS +
  CONTROL_CREDENTIAL_REVOKE_DEADLINE_MS

export const SAMPLE_LEAF_OWNER_HARD_ENVELOPE_MS =
  SAMPLE_LEAF_PROCESS_DEADLINE_MS +
  SAMPLE_MAXIMUM_CONTAINMENT_TERMINATIONS * SAMPLE_CONTAINMENT_TERMINATION_GRACE_MS

export const SAMPLE_LEAF_OUTER_DEADLINE_MS =
  SAMPLE_LEAF_OWNER_HARD_ENVELOPE_MS + SAMPLE_LEAF_OUTER_SETTLEMENT_RESERVE_MS

export function deriveNormalNetworkMatrixAuthorityDeadlineBudget(
  networkCredentialRequest: NetworkMatrixNetworkCredentialRequest,
): NetworkMatrixAuthorityDeadlineBudget {
  return authorityDeadlineBudget(
    networkCredentialRequest,
    'release',
    CONTROL_CREDENTIAL_NORMAL_RETIREMENT_MAXIMUM_MS,
  )
}

export function deriveForcedNetworkMatrixAuthorityDeadlineBudget(
  networkCredentialRequest: NetworkMatrixNetworkCredentialRequest,
): NetworkMatrixAuthorityDeadlineBudget {
  return authorityDeadlineBudget(
    networkCredentialRequest,
    'release-then-revoke',
    CONTROL_CREDENTIAL_FORCED_RETIREMENT_MAXIMUM_MS,
  )
}

export function validateNormalNetworkMatrixAuthorityDeadlineMs(
  deadlineMs: number,
  networkCredentialRequest: NetworkMatrixNetworkCredentialRequest,
): number {
  return validateOuterDeadline(
    deadlineMs,
    deriveNormalNetworkMatrixAuthorityDeadlineBudget(networkCredentialRequest),
    'normal network matrix authority deadline',
  )
}

export function validateForcedNetworkMatrixAuthorityDeadlineMs(
  deadlineMs: number,
  networkCredentialRequest: NetworkMatrixNetworkCredentialRequest,
): number {
  return validateOuterDeadline(
    deadlineMs,
    deriveForcedNetworkMatrixAuthorityDeadlineBudget(networkCredentialRequest),
    'forced network matrix authority deadline',
  )
}

export function deriveNetworkMatrixSampleDeadlineBudget(): NetworkMatrixDeadlineBudget {
  const serialStagesMaximumMs = sumMilliseconds(
    SAMPLE_CONTROL_CREDENTIAL_PRE_ACQUIRE_DEADLINE_MS,
    SAMPLE_LEAF_OUTER_DEADLINE_MS,
    CONTROL_CREDENTIAL_FORCED_RETIREMENT_MAXIMUM_MS,
    SAMPLE_FILESYSTEM_CLEANUP_RESERVE_MS,
  )
  return deadlineBudget(serialStagesMaximumMs, NETWORK_MATRIX_SAMPLE_OUTER_SETTLEMENT_RESERVE_MS)
}

export function validateNetworkMatrixSampleDeadlineMs(deadlineMs: number): number {
  return validateOuterDeadline(
    deadlineMs,
    deriveNetworkMatrixSampleDeadlineBudget(),
    'network matrix sample deadline',
  )
}

export function deriveScheduledNetworkMatrixJobDeadlineBudget():
ScheduledNetworkMatrixJobDeadlineBudget {
  const authority = deriveForcedNetworkMatrixAuthorityDeadlineBudget('stun-or-turn')
  const sample = deriveNetworkMatrixSampleDeadlineBudget()
  // The runner owns samples serially, so their deadlines accumulate instead of overlapping.
  const serialStagesMaximumMs = sumMilliseconds(
    SCHEDULED_JOB_SETUP_INSTALL_BUILD_RESERVE_MS,
    SCHEDULED_NETWORK_MATRIX_AUTHORITY_COUNT * authority.minimumOuterDeadlineMs,
    SCHEDULED_NETWORK_MATRIX_SAMPLE_COUNT * sample.minimumOuterDeadlineMs,
    SCHEDULED_JOB_EVIDENCE_FINALIZATION_RESERVE_MS,
    SCHEDULED_JOB_EVIDENCE_UPLOAD_RESERVE_MS,
  )
  const budget = deadlineBudget(serialStagesMaximumMs, SCHEDULED_JOB_OUTER_SETTLEMENT_RESERVE_MS)
  return Object.freeze({
    ...budget,
    minimumWorkflowTimeoutMinutes: Math.ceil(
      budget.minimumOuterDeadlineMs / NETWORK_MATRIX_MILLISECONDS_PER_MINUTE,
    ),
  })
}

export function validateScheduledNetworkMatrixJobDeadlineMs(deadlineMs: number): number {
  return validateOuterDeadline(
    deadlineMs,
    deriveScheduledNetworkMatrixJobDeadlineBudget(),
    'scheduled network matrix job deadline',
  )
}

export function validateScheduledNetworkMatrixWorkflowTimeoutMinutes(
  timeoutMinutes: number,
): number {
  if (!Number.isSafeInteger(timeoutMinutes) || timeoutMinutes < 1) {
    throw new Error('scheduled network matrix workflow timeout must be a positive whole minute')
  }
  const timeoutMs = timeoutMinutes * NETWORK_MATRIX_MILLISECONDS_PER_MINUTE
  if (!Number.isSafeInteger(timeoutMs)) {
    throw new Error('scheduled network matrix workflow timeout exceeds the safe integer range')
  }
  validateScheduledNetworkMatrixJobDeadlineMs(timeoutMs)
  return timeoutMinutes
}

export const PRODUCTION_NETWORK_MATRIX_NORMAL_AUTHORITY_DEADLINE_MS =
  deriveNormalNetworkMatrixAuthorityDeadlineBudget('stun-or-turn').minimumOuterDeadlineMs

export const PRODUCTION_NETWORK_MATRIX_FORCED_AUTHORITY_DEADLINE_MS =
  deriveForcedNetworkMatrixAuthorityDeadlineBudget('stun-or-turn').minimumOuterDeadlineMs

export const PRODUCTION_NETWORK_MATRIX_SAMPLE_DEADLINE_MS =
  deriveNetworkMatrixSampleDeadlineBudget().minimumOuterDeadlineMs

export const PRODUCTION_SCHEDULED_NETWORK_MATRIX_JOB_DEADLINE_MS =
  deriveScheduledNetworkMatrixJobDeadlineBudget().minimumOuterDeadlineMs

export const PRODUCTION_SCHEDULED_NETWORK_MATRIX_WORKFLOW_TIMEOUT_MINUTES =
  deriveScheduledNetworkMatrixJobDeadlineBudget().minimumWorkflowTimeoutMinutes

function authorityDeadlineBudget(
  networkCredentialRequest: NetworkMatrixNetworkCredentialRequest,
  retirement: NetworkMatrixAuthorityDeadlineBudget['retirement'],
  retirementMaximumMs: number,
): NetworkMatrixAuthorityDeadlineBudget {
  const serialStagesMaximumMs = sumMilliseconds(
    CONTROL_CREDENTIAL_BROKER_ACQUIRE_DEADLINE_MS,
    REMOTE_AUTHORITY_REQUEST_DEADLINE_MS,
    networkCredentialRequestMaximumMs(networkCredentialRequest),
    retirementMaximumMs,
  )
  return Object.freeze({
    retirement,
    networkCredentialRequest,
    ...deadlineBudget(serialStagesMaximumMs, NETWORK_MATRIX_AUTHORITY_OUTER_SETTLEMENT_RESERVE_MS),
  })
}

function networkCredentialRequestMaximumMs(
  request: NetworkMatrixNetworkCredentialRequest,
): number {
  if (request === 'none') return 0
  if (request === 'stun-or-turn') return REMOTE_NETWORK_CREDENTIAL_REQUEST_DEADLINE_MS
  throw new Error('network matrix authority credential request kind is invalid')
}

function deadlineBudget(
  serialStagesMaximumMs: number,
  settlementReserveMs: number,
): NetworkMatrixDeadlineBudget {
  // Equal deadlines leave the final inner completion and outer timer in a scheduler race.
  return Object.freeze({
    serialStagesMaximumMs,
    minimumOuterDeadlineMs: sumMilliseconds(
      serialStagesMaximumMs,
      settlementReserveMs,
    ),
  })
}

function validateOuterDeadline(
  deadlineMs: number,
  budget: NetworkMatrixDeadlineBudget,
  label: string,
): number {
  if (
    !Number.isSafeInteger(deadlineMs) ||
    deadlineMs < budget.minimumOuterDeadlineMs
  ) {
    throw new Error(`${label} must be at least ${budget.minimumOuterDeadlineMs} ms`)
  }
  return deadlineMs
}

function sumMilliseconds(...segments: readonly number[]): number {
  let total = 0
  for (const segment of segments) {
    if (!Number.isSafeInteger(segment) || segment < 0) {
      throw new Error('network matrix deadline segment is outside the safe range')
    }
    total += segment
    if (!Number.isSafeInteger(total)) {
      throw new Error('network matrix deadline total exceeds the safe integer range')
    }
  }
  return total
}
