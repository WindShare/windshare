import type { ArtifactKind, ResolvedArtifactSpec } from '../../transfer/selection'

export type SaveBackendKind = 'portable' | 'fsa' | 'opfs'

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
  readonly resumable: boolean
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
