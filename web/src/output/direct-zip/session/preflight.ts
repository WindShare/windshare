import type { DirectZipLifecycleDecision, DirectZipTargetStage } from '../target'

export interface DirectZipDestinationSpacePort<ParentHandle> {
  /** `null` means the destination cannot provide a trustworthy local-space observation. */
  availableBytes(parent: ParentHandle): Promise<bigint | null>
}

export interface DirectZipSpacePreflightInput<ParentHandle> {
  readonly parent: ParentHandle
  readonly committedArchiveLength: bigint
  readonly projectedArchiveLength: bigint
  readonly additionalTemporaryBytesUpperBound: bigint
  readonly space: DirectZipDestinationSpacePort<ParentHandle>
  readonly stage?: DirectZipTargetStage
}

export type DirectZipSpacePreflightResult =
  | Readonly<{
      readonly kind: 'admitted'
      readonly availableBytes: bigint
      readonly requiredAdditionalBytes: bigint
    }>
  | Readonly<{
      readonly kind: 'gated'
      readonly decision: Extract<DirectZipLifecycleDecision, {
        kind: 'destination-space-required'
      }>
      readonly requiredAdditionalBytes: bigint
      readonly availableBytes: bigint | null
    }>

/** Space is checked against remaining archive growth plus the reviewed copy-on-write peak. */
export async function preflightDirectZipDestinationSpace(
  input: DirectZipSpacePreflightInput<unknown>,
): Promise<DirectZipSpacePreflightResult> {
  requireOffset(input.committedArchiveLength, 'committed archive length')
  requireOffset(input.projectedArchiveLength, 'projected archive length')
  requireOffset(input.additionalTemporaryBytesUpperBound, 'temporary-space upper bound')
  if (input.projectedArchiveLength < input.committedArchiveLength) {
    throw new TypeError('projected archive length precedes the committed archive')
  }
  const requiredAdditionalBytes = input.projectedArchiveLength - input.committedArchiveLength +
    input.additionalTemporaryBytesUpperBound
  const availableBytes = await input.space.availableBytes(input.parent)
  if (availableBytes !== null) requireOffset(availableBytes, 'available destination bytes')
  if (availableBytes === null || availableBytes < requiredAdditionalBytes) {
    return Object.freeze({
      kind: 'gated',
      decision: Object.freeze({
        kind: 'destination-space-required',
        stage: input.stage ?? 'epoch-open',
      }),
      requiredAdditionalBytes,
      availableBytes,
    })
  }
  return Object.freeze({ kind: 'admitted', availableBytes, requiredAdditionalBytes })
}

function requireOffset(value: bigint, label: string): void {
  if (typeof value !== 'bigint' || value < 0n || value > BigInt(Number.MAX_SAFE_INTEGER)) {
    throw new RangeError(`${label} exceeds the positioned target bound`)
  }
}
