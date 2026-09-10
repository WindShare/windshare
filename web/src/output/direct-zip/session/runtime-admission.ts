import { encodeBase64Url } from '../../../crypto/bytes'
import type {
  DirectZipRuntimeAuthorityContractV1,
  DirectZipSupportFacts,
  RuntimeDirectZipSupportFacts,
  ZipRouteRecommendationPolicyV1,
} from '../../planning'
import { zipRouteRecommendationPolicyDigestV1 } from '../../planning/zip-route-recommendation'
import {
  canonicalDigest,
  canonicalFrame,
  canonicalRecord,
  canonicalText,
  canonicalU8,
} from '../../workspace/canonical'
import { directZipPolicyDigestsV2 } from '../format'
import { directZipJournalBudgetDigestV1 } from '../journal'
import {
  directZipEpochPolicyDigestV1,
  type DirectZipAutomaticEpochBudgetV1,
} from '../writer'

const CHECKPOINT_POLICY_DOMAIN = 'windshare/direct-zip-checkpoint-policy/v1'
const CAPABILITY_DOMAIN = 'windshare/direct-zip-runtime-capabilities/v1'
const MEBIBYTE = 1_048_576n
const GIBIBYTE = 1_073_741_824n

export interface DirectZipRequiredFeatureFactsV1 {
  readonly createWritable: 'function' | 'missing'
  readonly handleIsSameEntry: 'function' | 'missing'
  readonly handleQueryPermission: 'function' | 'missing'
  readonly handleRequestPermission: 'function' | 'missing'
  readonly indexedDB: 'object' | 'missing'
  readonly isSecureContext: boolean
  readonly locks: 'object' | 'missing'
  readonly showDirectoryPicker: 'function' | 'missing'
}

export type DirectZipRuntimeAuthorityV1 =
  | DirectZipRuntimeAuthorityContractV1
  | Readonly<{
      readonly kind: 'unavailable'
      readonly reason:
        | 'runtime-not-installed'
        | 'journal-unavailable'
        | 'handle-persistence-unavailable'
        | 'coordination-unavailable'
    }>

export interface DirectZipRuntimeCapabilitiesV1 {
  readonly featureFacts: DirectZipRequiredFeatureFactsV1
  /**
   * The installed session must enforce this contract. Feature discovery alone
   * cannot establish journal durability, ownership, or checkpoint validity.
   */
  readonly authority: DirectZipRuntimeAuthorityV1
}

export interface DirectZipRuntimePolicyV1 {
  readonly version: 1
  readonly automaticEpochBudget: DirectZipAutomaticEpochBudgetV1
  /** Ranking is optional and never grants target authority. */
  readonly workspacePeakBytesThreshold: bigint | null
}

/**
 * Product resource ceilings bound avoidable prefix copying and workspace use.
 * They are neither local free-space observations nor platform performance claims.
 */
export const DEFAULT_DIRECT_ZIP_RUNTIME_POLICY_V1: DirectZipRuntimePolicyV1 = Object.freeze({
  version: 1,
  automaticEpochBudget: Object.freeze({
    maximumPrefixCopyBytes: 256n * MEBIBYTE,
    maximumCumulativePrefixCopyBytes: 512n * MEBIBYTE,
    maximumModeledPeakTemporaryBytes: 256n * MEBIBYTE,
  }),
  workspacePeakBytesThreshold: GIBIBYTE,
})

export interface DirectZipRuntimeFactsV1 {
  readonly support: RuntimeDirectZipSupportFacts
  readonly recommendationPolicy: ZipRouteRecommendationPolicyV1
  readonly automaticEpochBudget: DirectZipAutomaticEpochBudgetV1
}

export type DirectZipSupportLookupV1 =
  | Readonly<{ readonly kind: 'available'; readonly facts: DirectZipRuntimeFactsV1 }>
  | Readonly<{ readonly kind: 'unavailable'; readonly support: DirectZipSupportFacts }>

/**
 * Admission selects an implemented session contract; each operation must still
 * persist its handles, acquire its locks, and verify ownership before mutation.
 * Release-machine identity has no role in those runtime obligations.
 */
export async function admitDirectZipRuntimeV1(input: Readonly<{
  readonly capabilities: DirectZipRuntimeCapabilitiesV1
  readonly policy?: DirectZipRuntimePolicyV1
}>): Promise<DirectZipSupportLookupV1> {
  const { featureFacts, authority } = input.capabilities
  if (!hasRequiredFeatures(featureFacts)) return unavailable('required-api-unavailable')
  if (authority.kind === 'unavailable') return unavailable(authority.reason)
  if (!hasRequiredAuthority(authority)) return unavailable('authority-contract-unavailable')

  const policy = input.policy ?? DEFAULT_DIRECT_ZIP_RUNTIME_POLICY_V1
  if (!validPolicy(policy)) return unavailable('policy-digests-unavailable')
  const automaticEpochBudget = Object.freeze({ ...policy.automaticEpochBudget })
  const workspacePeakBytesThreshold = policy.workspacePeakBytesThreshold
  const authoritySnapshot = Object.freeze({ ...authority })
  const format = await directZipPolicyDigestsV2()
  const checkpoint = await canonicalDigest(canonicalRecord(CHECKPOINT_POLICY_DOMAIN, 1, [
    canonicalFrame(format.encodingPolicy),
    canonicalFrame(format.layoutPolicy),
    canonicalFrame(canonicalU8(1)),
    canonicalFrame(canonicalU8(1)),
    canonicalFrame(canonicalU8(1)),
    canonicalFrame(canonicalU8(2)),
    canonicalFrame(canonicalU8(3)),
  ]))
  // Admission has established the fixed V1 API requirements. Only the semantic
  // session contract participates in authority identity across browser upgrades.
  const capabilityDigest = await canonicalDigest(canonicalRecord(CAPABILITY_DOMAIN, 1, [
    canonicalFrame(canonicalText(authoritySnapshot.kind)),
    canonicalFrame(canonicalText(authoritySnapshot.recovery)),
    canonicalFrame(canonicalText(authoritySnapshot.replacement)),
    canonicalFrame(canonicalText(authoritySnapshot.cleanup)),
  ]))
  const support: RuntimeDirectZipSupportFacts = Object.freeze({
    kind: 'runtime-supported',
    capabilityDigest,
    authority: authoritySnapshot,
    policies: Object.freeze({
      zipEncoding: encodeBase64Url(format.encodingPolicy),
      layout: encodeBase64Url(format.layoutPolicy),
      checkpoint,
      journalBudget: await directZipJournalBudgetDigestV1(),
      epoch: await directZipEpochPolicyDigestV1(automaticEpochBudget),
    }),
  })
  const recommendationPolicy: ZipRouteRecommendationPolicyV1 =
    workspacePeakBytesThreshold === null || !validOffset(workspacePeakBytesThreshold)
      ? Object.freeze({
          version: 1,
          kind: 'unavailable',
          reason: workspacePeakBytesThreshold === null
            ? 'workspace-threshold-unavailable' : 'policy-digest-unavailable',
        })
      : Object.freeze({
          version: 1,
          kind: 'available',
          workspacePeakBytesThreshold,
          policyDigest: await zipRouteRecommendationPolicyDigestV1(workspacePeakBytesThreshold),
        })
  return Object.freeze({
    kind: 'available',
    facts: Object.freeze({ support, recommendationPolicy, automaticEpochBudget }),
  })
}

function unavailable(
  reason: Extract<DirectZipSupportFacts, { kind: 'unavailable' }>['reason'],
): DirectZipSupportLookupV1 {
  return Object.freeze({ kind: 'unavailable', support: Object.freeze({ kind: 'unavailable', reason }) })
}

function hasRequiredFeatures(value: DirectZipRequiredFeatureFactsV1): boolean {
  return value.createWritable === 'function' && value.handleIsSameEntry === 'function' &&
    value.handleQueryPermission === 'function' && value.handleRequestPermission === 'function' &&
    value.indexedDB === 'object' && value.isSecureContext === true && value.locks === 'object' &&
    value.showDirectoryPicker === 'function'
}

function hasRequiredAuthority(value: DirectZipRuntimeAuthorityContractV1): boolean {
  return value.kind === 'owned-target-session-v1' &&
    value.recovery === 'persisted-handle-and-verified-checkpoint' &&
    value.replacement === 'coordinated-no-replace' && value.cleanup === 'ownership-proof-required'
}

function validPolicy(value: DirectZipRuntimePolicyV1): boolean {
  return value.version === 1 &&
    validOffset(value.automaticEpochBudget?.maximumPrefixCopyBytes) &&
    validOffset(value.automaticEpochBudget?.maximumCumulativePrefixCopyBytes) &&
    validOffset(value.automaticEpochBudget?.maximumModeledPeakTemporaryBytes)
}

function validOffset(value: bigint): boolean {
  return typeof value === 'bigint' && value >= 0n && value <= BigInt(Number.MAX_SAFE_INTEGER)
}
