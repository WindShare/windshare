import {
  contractError,
  freezeRecord,
  optionalField,
  requireEnum,
  requireExactKeys,
  requireLiteral,
  requireRecord,
  requireString,
} from './contract/json.ts'
import {
  BROWSER_EVIDENCE_SCHEMA_VERSION,
  type RtcCapability,
} from './vocabulary.ts'

export const CAPABILITY_PROBE_DEADLINE_MS = 5_000 as const
export const CAPABILITY_PROBE_DATA_CHANNEL_LABEL = 'windshare-capability-probe' as const

export const RTC_API_PRESENCE = Object.freeze(['unknown', 'absent', 'present'] as const)
export const CAPABILITY_PROBE_OUTCOMES = Object.freeze([
  'not-started',
  'succeeded',
  'failed',
] as const)
export const CAPABILITY_PROBE_FAILURE_CODES = Object.freeze([
  'peer-construction',
  'datachannel-construction',
  'offer-creation',
  'local-description',
  'probe-deadline',
  'unexpected',
] as const)

export type RtcApiPresence = (typeof RTC_API_PRESENCE)[number]
export type CapabilityProbeOutcome = (typeof CAPABILITY_PROBE_OUTCOMES)[number]
export type CapabilityProbeFailureCode = (typeof CAPABILITY_PROBE_FAILURE_CODES)[number]

export interface CapabilityEvidence {
  readonly schemaVersion: typeof BROWSER_EVIDENCE_SCHEMA_VERSION
  readonly apiPresence: RtcApiPresence
  readonly probeOutcome: CapabilityProbeOutcome
  readonly probeDeadlineMs: typeof CAPABILITY_PROBE_DEADLINE_MS
  readonly failureCode?: CapabilityProbeFailureCode
  readonly failureMessage?: string
}

export interface RtcApiEnvironment {
  readonly RTCPeerConnection?: unknown
}

export function rtcPeerConnectionApiPresent(environment: RtcApiEnvironment): boolean {
  return typeof environment.RTCPeerConnection === 'function'
}

/**
 * The probe proves only that a native PeerConnection can retain a non-empty
 * local offer after creating the WindShare-shaped DataChannel. Waiting for ICE
 * or a remote peer here would collapse runtime capability into topology health.
 */
export function classifyRtcCapability(evidence: CapabilityEvidence): RtcCapability {
  validateCapabilityCombination(evidence)
  if (evidence.apiPresence === 'unknown') return 'unknown'
  if (evidence.apiPresence === 'absent') return 'unavailable'
  if (evidence.probeOutcome === 'not-started') return 'unknown'
  return evidence.probeOutcome === 'succeeded' ? 'available' : 'unusable'
}

export function parseCapabilityEvidence(value: unknown): CapabilityEvidence {
  const record = requireRecord(value, 'capability evidence')
  requireExactKeys(
    record,
    ['schemaVersion', 'apiPresence', 'probeOutcome', 'probeDeadlineMs'],
    ['failureCode', 'failureMessage'],
    'capability evidence',
  )
  const failureCodeValue = optionalField(record, 'failureCode')
  const failureMessageValue = optionalField(record, 'failureMessage')
  const result: CapabilityEvidence = {
    schemaVersion: requireLiteral(
      record.schemaVersion,
      BROWSER_EVIDENCE_SCHEMA_VERSION,
      'capability schema version',
    ),
    apiPresence: requireEnum(record.apiPresence, RTC_API_PRESENCE, 'RTC API presence'),
    probeOutcome: requireEnum(
      record.probeOutcome,
      CAPABILITY_PROBE_OUTCOMES,
      'capability probe outcome',
    ),
    probeDeadlineMs: requireLiteral(
      record.probeDeadlineMs,
      CAPABILITY_PROBE_DEADLINE_MS,
      'capability probe deadline',
    ),
    ...(failureCodeValue === undefined
      ? {}
      : {
          failureCode: requireEnum(
            failureCodeValue,
            CAPABILITY_PROBE_FAILURE_CODES,
            'capability probe failure code',
          ),
        }),
    ...(failureMessageValue === undefined
      ? {}
      : { failureMessage: requireString(failureMessageValue, 'capability failure message', 512) }),
  }
  validateCapabilityCombination(result)
  return freezeRecord(result)
}

function validateCapabilityCombination(evidence: CapabilityEvidence): void {
  const failed = evidence.probeOutcome === 'failed'
  if (failed !== (evidence.failureCode !== undefined)) {
    contractError('only a failed capability probe may carry a failure code, and it must carry one')
  }
  if ((evidence.failureMessage !== undefined) !== failed) {
    contractError('only a failed capability probe may carry a failure message, and it must carry one')
  }
  if (evidence.apiPresence !== 'present' && evidence.probeOutcome !== 'not-started') {
    contractError('a capability probe cannot run before native RTC API presence is proved')
  }
}

export function provisionalCapabilityEvidence(): CapabilityEvidence {
  return Object.freeze({
    schemaVersion: BROWSER_EVIDENCE_SCHEMA_VERSION,
    apiPresence: 'unknown',
    probeOutcome: 'not-started',
    probeDeadlineMs: CAPABILITY_PROBE_DEADLINE_MS,
  })
}
