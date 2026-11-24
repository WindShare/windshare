import type { ArtifactKind, ResolvedArtifactSpec } from '../../transfer/selection'

export type SaveBackendKind = 'portable' | 'fsa' | 'opfs'

/**
 * Distinguishes how each save backend preserves progress across interruptions:
 * - `none`: in-memory stream; progress is lost if the tab closes.
 * - `pause`: direct filesystem writes; pause, retry, long disconnects, and completed files survive,
 *   while an unexpected crash can still lose an uncheckpointed in-flight file.
 * - `continuous`: periodic in-place browser storage checkpoints for crash recovery.
 */
export type OfferResumeSupport = 'none' | 'pause' | 'continuous'

export type OfferUnavailableReason =
  | 'confirming'
  | 'selection-empty'
  | 'retry-required'
  | 'no-safe-destination'
  | 'portable-limit-exceeded'
  | 'storage-full'

export interface ArtifactChoice {
  readonly choiceId: string
  readonly backend: SaveBackendKind
  readonly artifactKind: ArtifactKind
  readonly artifact: ResolvedArtifactSpec
  readonly resumeSupport: OfferResumeSupport
  readonly recommended: boolean
  readonly suggestedName: string
  readonly estimatedBytes?: bigint
}

export type ArtifactOffers =
  | Readonly<{
      kind: 'confirming'
      epoch: bigint
      selectionKey: string
    }>
  | Readonly<{
      kind: 'retry-required'
      epoch: bigint
      selectionKey: string
    }>
  | Readonly<{
      kind: 'selection-empty'
      epoch: bigint
      selectionKey: string
    }>
  | Readonly<{
      kind: 'unavailable'
      epoch: bigint
      selectionKey: string
      reason: Exclude<OfferUnavailableReason, 'confirming' | 'selection-empty' | 'retry-required'>
    }>
  | Readonly<{
      kind: 'ready'
      epoch: bigint
      selectionKey: string
      primary: ArtifactChoice
      alternatives: readonly ArtifactChoice[]
      zip: ArtifactChoice | null
    }>
