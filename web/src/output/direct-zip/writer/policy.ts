import {
  canonicalDigest,
  canonicalFrame,
  canonicalRecord,
  canonicalU64,
} from '../../workspace/canonical'

const DIRECT_ZIP_EPOCH_POLICY_DOMAIN = 'windshare/direct-zip-epoch-policy/v2'

export interface DirectZipAutomaticEpochPolicyV1 {
  readonly minimumAdvanceBytes: bigint
}

export interface DirectZipAutomaticCheckpointInputV1 {
  readonly archiveOffset: bigint
  readonly committedLength: bigint
  readonly policy?: DirectZipAutomaticEpochPolicyV1
}

export type DirectZipAutomaticCheckpointDecisionV1 =
  | Readonly<{
      kind: 'admit'
      additionalTemporaryBytesUpperBound: bigint
    }>
  | Readonly<{
      kind: 'decline'
      reason: 'policy-unavailable' | 'insufficient-progress'
      additionalTemporaryBytesUpperBound: bigint
    }>

export async function directZipEpochPolicyDigestV2(
  policy: DirectZipAutomaticEpochPolicyV1,
): Promise<string> {
  requirePolicy(policy)
  return canonicalDigest(canonicalRecord(DIRECT_ZIP_EPOCH_POLICY_DOMAIN, 2, [
    canonicalFrame(canonicalU64(policy.minimumAdvanceBytes)),
  ]))
}

/**
 * Each automatic close must earn its next full-prefix reopen through new archive
 * progress. During forward progress, doubling the durable prefix bounds automatic
 * copies below twice archive progress. Recovery reconstructs this spacing from the
 * checkpoint; user pauses and retry copies are separate costs.
 * Large archives therefore keep gaining durable progress, with proportionally larger
 * replay windows; the pending prefix remains the explicit temporary-space bound.
 */
export function decideDirectZipAutomaticCheckpointV1(
  input: DirectZipAutomaticCheckpointInputV1,
): DirectZipAutomaticCheckpointDecisionV1 {
  requireOffset(input.committedLength, 'direct ZIP committed length')
  requireOffset(input.archiveOffset, 'direct ZIP archive offset')
  if (input.archiveOffset < input.committedLength) {
    throw new RangeError('direct ZIP archive offset precedes its durable checkpoint')
  }
  const spaceBound = input.archiveOffset
  if (input.policy === undefined) {
    return Object.freeze({
      kind: 'decline',
      reason: 'policy-unavailable',
      additionalTemporaryBytesUpperBound: spaceBound,
    })
  }
  requirePolicy(input.policy)
  const requiredAdvance = input.committedLength > input.policy.minimumAdvanceBytes
    ? input.committedLength : input.policy.minimumAdvanceBytes
  if (input.archiveOffset - input.committedLength < requiredAdvance) {
    return Object.freeze({
      kind: 'decline',
      reason: 'insufficient-progress',
      additionalTemporaryBytesUpperBound: spaceBound,
    })
  }
  return Object.freeze({
    kind: 'admit',
    additionalTemporaryBytesUpperBound: spaceBound,
  })
}

function requirePolicy(policy: DirectZipAutomaticEpochPolicyV1): void {
  requireOffset(policy.minimumAdvanceBytes, 'direct ZIP automatic minimum advance')
  if (policy.minimumAdvanceBytes === 0n) {
    throw new RangeError('direct ZIP automatic minimum advance must be positive')
  }
}

function requireOffset(value: bigint, label: string): void {
  if (typeof value !== 'bigint' || value < 0n || value > BigInt(Number.MAX_SAFE_INTEGER)) {
    throw new RangeError(`${label} exceeds the positioned target bound`)
  }
}
