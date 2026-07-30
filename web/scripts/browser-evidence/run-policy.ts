import {
  freezeRecord,
  requireEnum,
  requireExactKeys,
  requireLiteral,
  requireRecord,
  type JsonRecord,
} from './contract/json.ts'

export const BROWSER_RUN_POLICY_SCHEMA_VERSION = 1 as const
export const BROWSER_RUN_POLICY_VERSION = 1 as const
export const BROWSER_RUN_POLICY_IDS = Object.freeze([
  'blocking',
  'closure',
  'stability',
] as const)

export type BrowserRunPolicyId = (typeof BROWSER_RUN_POLICY_IDS)[number]

export interface BrowserRunPolicy {
  readonly schemaVersion: typeof BROWSER_RUN_POLICY_SCHEMA_VERSION
  readonly policyId: BrowserRunPolicyId
  readonly policyVersion: typeof BROWSER_RUN_POLICY_VERSION
  readonly sampleCount: 1 | 3 | 5
}

const POLICY_SAMPLE_COUNTS = Object.freeze({
  blocking: 1,
  closure: 3,
  stability: 5,
} as const satisfies Readonly<Record<BrowserRunPolicyId, BrowserRunPolicy['sampleCount']>>)

const POLICIES = Object.freeze(Object.fromEntries(
  BROWSER_RUN_POLICY_IDS.map((policyId) => [policyId, freezeRecord({
    schemaVersion: BROWSER_RUN_POLICY_SCHEMA_VERSION,
    policyId,
    policyVersion: BROWSER_RUN_POLICY_VERSION,
    sampleCount: POLICY_SAMPLE_COUNTS[policyId],
  })]),
) as Readonly<Record<BrowserRunPolicyId, BrowserRunPolicy>>)

export function browserRunPolicy(policyId: BrowserRunPolicyId): BrowserRunPolicy {
  return POLICIES[policyId]
}

export function parseBrowserRunPolicy(value: unknown, label = 'browser run policy'): BrowserRunPolicy {
  const record = requireRecord(value, label)
  requireExactKeys(
    record,
    ['schemaVersion', 'policyId', 'policyVersion', 'sampleCount'],
    [],
    label,
  )
  const policyId = requireEnum(record.policyId, BROWSER_RUN_POLICY_IDS, `${label} identity`)
  const canonical = browserRunPolicy(policyId)
  requireLiteral(
    record.schemaVersion,
    BROWSER_RUN_POLICY_SCHEMA_VERSION,
    `${label} schema version`,
  )
  requireLiteral(
    record.policyVersion,
    BROWSER_RUN_POLICY_VERSION,
    `${label} version`,
  )
  requireLiteral(record.sampleCount, canonical.sampleCount, `${label} sample count`)
  return canonical
}

export function assertBrowserRunPolicyEqual(
  actual: BrowserRunPolicy,
  expected: BrowserRunPolicy,
  label = 'browser run policy',
): void {
  if (
    actual.schemaVersion !== expected.schemaVersion
    || actual.policyId !== expected.policyId
    || actual.policyVersion !== expected.policyVersion
    || actual.sampleCount !== expected.sampleCount
  ) {
    throw new Error(`${label} does not match the bound policy authority`)
  }
}

export function validatePolicySampleIndex(
  sampleIndex: number,
  policy: BrowserRunPolicy,
  label = 'sample index',
): number {
  if (!Number.isSafeInteger(sampleIndex) || sampleIndex < 1 || sampleIndex > policy.sampleCount) {
    throw new Error(`${label} must be in [1, ${policy.sampleCount}] for ${policy.policyId}@${policy.policyVersion}`)
  }
  return sampleIndex
}

export function parseBrowserRunPolicyId(value: unknown, label = 'browser run policy identity'):
BrowserRunPolicyId {
  return requireEnum(value, BROWSER_RUN_POLICY_IDS, label)
}

export function browserRunPolicyJson(policy: BrowserRunPolicy): JsonRecord {
  return freezeRecord({
    schemaVersion: policy.schemaVersion,
    policyId: policy.policyId,
    policyVersion: policy.policyVersion,
    sampleCount: policy.sampleCount,
  })
}
