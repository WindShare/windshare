import type { ArtifactOffers, OfferedArtifactChoice } from '../../output/planning'

export type SavingOutcomeKind = 'original-file' | 'folder' | 'zip'

export interface SavingChoice {
  readonly offered: OfferedArtifactChoice
  readonly label: string
  readonly outcome: SavingOutcomeKind
  readonly description: string
  readonly consequences: readonly string[]
  readonly disabledReason: string | null
}

export interface SavingOutcome {
  readonly kind: SavingOutcomeKind
  readonly label: string
  readonly primary: SavingChoice
  readonly alternatives: readonly SavingChoice[]
}

export interface SavingPresentationTransition {
  readonly name: 'receiver.saving.recommendation'
  readonly projection_epoch: ArtifactOffers['projectionEpoch'] | null
  readonly choice_id: string | null
  readonly outcome: SavingOutcomeKind | null
  readonly reason: string
  readonly fingerprint: string
}

export interface SavingActionPresentation {
  readonly primary: SavingChoice | null
  readonly alternatives: readonly SavingOutcome[]
  readonly disabledReason: string | null
  readonly retry: boolean
  readonly desktopGuidance: string | null
  readonly recommendation: string | null
  readonly transition: SavingPresentationTransition
}
