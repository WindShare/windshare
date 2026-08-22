import {
  canonicalDigest,
  canonicalFrame,
  canonicalRecord,
  canonicalU64,
} from '../../workspace/canonical'

const DIRECT_ZIP_EPOCH_POLICY_DOMAIN = 'windshare/direct-zip-epoch-policy/v1'

export interface DirectZipAutomaticEpochBudgetV1 {
  readonly maximumPrefixCopyBytes: bigint
  readonly maximumCumulativePrefixCopyBytes: bigint
  readonly maximumModeledPeakTemporaryBytes: bigint
}

export interface DirectZipAutomaticCheckpointInputV1 {
  readonly committedLength: bigint
  readonly cumulativePrefixCopyBytes: bigint
  readonly budget?: DirectZipAutomaticEpochBudgetV1
}

export type DirectZipAutomaticCheckpointDecisionV1 =
  | Readonly<{
      kind: 'admit'
      nextCumulativePrefixCopyBytes: bigint
      additionalTemporaryBytesUpperBound: bigint
    }>
  | Readonly<{
      kind: 'decline'
      reason:
        | 'evidence-unavailable'
        | 'prefix-copy-budget'
        | 'cumulative-copy-budget'
        | 'modeled-peak-temporary-budget'
      additionalTemporaryBytesUpperBound: bigint
    }>

export async function directZipEpochPolicyDigestV1(
  budget: DirectZipAutomaticEpochBudgetV1,
): Promise<string> {
  requireBudget(budget)
  return canonicalDigest(canonicalRecord(DIRECT_ZIP_EPOCH_POLICY_DOMAIN, 1, [
    canonicalFrame(canonicalU64(budget.maximumPrefixCopyBytes)),
    canonicalFrame(canonicalU64(budget.maximumCumulativePrefixCopyBytes)),
    canonicalFrame(canonicalU64(budget.maximumModeledPeakTemporaryBytes)),
  ]))
}

/** Missing evidence is a policy result, not a reason to invent a permissive threshold. */
export function decideDirectZipAutomaticCheckpointV1(
  input: DirectZipAutomaticCheckpointInputV1,
): DirectZipAutomaticCheckpointDecisionV1 {
  requireOffset(input.committedLength, 'direct ZIP committed length')
  requireOffset(input.cumulativePrefixCopyBytes, 'direct ZIP cumulative prefix-copy bytes')
  // The committed length is the exact archive prefix, so ZIP headers, descriptors,
  // and ownership metadata are admitted instead of only the selected source payload.
  const spaceBound = input.committedLength
  if (input.budget === undefined) {
    return Object.freeze({
      kind: 'decline',
      reason: 'evidence-unavailable',
      additionalTemporaryBytesUpperBound: spaceBound,
    })
  }
  requireBudget(input.budget)
  if (spaceBound > input.budget.maximumPrefixCopyBytes) {
    return Object.freeze({
      kind: 'decline',
      reason: 'prefix-copy-budget',
      additionalTemporaryBytesUpperBound: spaceBound,
    })
  }
  if (spaceBound > input.budget.maximumModeledPeakTemporaryBytes) {
    return Object.freeze({
      kind: 'decline',
      reason: 'modeled-peak-temporary-budget',
      additionalTemporaryBytesUpperBound: spaceBound,
    })
  }
  if (input.cumulativePrefixCopyBytes > input.budget.maximumCumulativePrefixCopyBytes ||
      spaceBound > input.budget.maximumCumulativePrefixCopyBytes -
        input.cumulativePrefixCopyBytes) {
    return Object.freeze({
      kind: 'decline',
      reason: 'cumulative-copy-budget',
      additionalTemporaryBytesUpperBound: spaceBound,
    })
  }
  const nextCumulative = input.cumulativePrefixCopyBytes + spaceBound
  return Object.freeze({
    kind: 'admit',
    nextCumulativePrefixCopyBytes: nextCumulative,
    additionalTemporaryBytesUpperBound: spaceBound,
  })
}

function requireBudget(budget: DirectZipAutomaticEpochBudgetV1): void {
  requireOffset(budget.maximumPrefixCopyBytes, 'direct ZIP automatic prefix-copy budget')
  requireOffset(
    budget.maximumCumulativePrefixCopyBytes,
    'direct ZIP automatic cumulative-copy budget',
  )
  requireOffset(
    budget.maximumModeledPeakTemporaryBytes,
    'direct ZIP automatic modeled peak temporary-space budget',
  )
}

function requireOffset(value: bigint, label: string): void {
  if (typeof value !== 'bigint' || value < 0n || value > BigInt(Number.MAX_SAFE_INTEGER)) {
    throw new RangeError(`${label} exceeds the positioned target bound`)
  }
}
