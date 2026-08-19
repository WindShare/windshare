export const MAX_CONSOLE_REPORTS_PER_SCOPE = 25
export const MAX_INCIDENT_HISTORY_RECORDS = 64
export const MAX_CONTRIBUTOR_BUCKETS_PER_INCIDENT = 16
export const MAX_CONSEQUENCE_BUCKETS_PER_INCIDENT = 16
export const MAX_FACT_REPRESENTATIVES_PER_BUCKET = 1
export const MAX_INCIDENT_RECORD_BYTES = 65_536
export const MAX_RECORD_LIST_ITEMS = 32
export const MAX_SAFE_STRING_UTF8_BYTES = 128
export const MAX_LATE_INCIDENT_LINKS = 128
export const MAX_LATE_INCIDENT_LINK_AGE_MS = 600_000
export const INCIDENT_SEAL_DEADLINE_MS = 2_000

export interface IncidentPolicy {
  readonly maxConsoleReportsPerScope: number
  readonly maxIncidentHistoryRecords: number
  readonly maxContributorBucketsPerIncident: number
  readonly maxConsequenceBucketsPerIncident: number
  readonly maxFactRepresentativesPerBucket: 1
  readonly maxIncidentRecordBytes: number
  readonly maxRecordListItems: number
  readonly maxSafeStringUtf8Bytes: number
  readonly maxLateIncidentLinks: number
  readonly maxLateIncidentLinkAgeMilliseconds: number
  readonly incidentSealDeadlineMilliseconds: number
}

const INCIDENT_POLICY_KEYS = Object.freeze([
  'maxConsoleReportsPerScope',
  'maxIncidentHistoryRecords',
  'maxContributorBucketsPerIncident',
  'maxConsequenceBucketsPerIncident',
  'maxFactRepresentativesPerBucket',
  'maxIncidentRecordBytes',
  'maxRecordListItems',
  'maxSafeStringUtf8Bytes',
  'maxLateIncidentLinks',
  'maxLateIncidentLinkAgeMilliseconds',
  'incidentSealDeadlineMilliseconds',
] as const satisfies readonly (keyof IncidentPolicy)[])

export const DEFAULT_INCIDENT_POLICY: IncidentPolicy = Object.freeze({
  maxConsoleReportsPerScope: MAX_CONSOLE_REPORTS_PER_SCOPE,
  maxIncidentHistoryRecords: MAX_INCIDENT_HISTORY_RECORDS,
  maxContributorBucketsPerIncident: MAX_CONTRIBUTOR_BUCKETS_PER_INCIDENT,
  maxConsequenceBucketsPerIncident: MAX_CONSEQUENCE_BUCKETS_PER_INCIDENT,
  maxFactRepresentativesPerBucket: MAX_FACT_REPRESENTATIVES_PER_BUCKET,
  maxIncidentRecordBytes: MAX_INCIDENT_RECORD_BYTES,
  maxRecordListItems: MAX_RECORD_LIST_ITEMS,
  maxSafeStringUtf8Bytes: MAX_SAFE_STRING_UTF8_BYTES,
  maxLateIncidentLinks: MAX_LATE_INCIDENT_LINKS,
  maxLateIncidentLinkAgeMilliseconds: MAX_LATE_INCIDENT_LINK_AGE_MS,
  incidentSealDeadlineMilliseconds: INCIDENT_SEAL_DEADLINE_MS,
})

export function createIncidentPolicy(
  overrides: Partial<IncidentPolicy> = {},
): IncidentPolicy {
  if (
    Object.keys(overrides).some(
      (key) => !INCIDENT_POLICY_KEYS.includes(key as keyof IncidentPolicy),
    )
  ) {
    throw new TypeError('Incident policy contains an unknown field')
  }
  const candidate = { ...DEFAULT_INCIDENT_POLICY, ...overrides }
  for (const [name, value] of Object.entries(candidate)) {
    requirePositiveSafeInteger(value, name)
  }
  if (candidate.maxFactRepresentativesPerBucket !== 1) {
    throw new RangeError('Incident fact buckets retain exactly one public representative')
  }
  return Object.freeze(candidate)
}

function requirePositiveSafeInteger(value: unknown, name: string): asserts value is number {
  if (!Number.isSafeInteger(value) || (value as number) <= 0) {
    throw new RangeError(`Incident policy ${name} must be a positive safe integer`)
  }
}
