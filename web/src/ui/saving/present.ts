import type { ArtifactOffers } from '../../output/model/artifacts/offers'
import type { OfferedArtifactChoice } from '../../output/model/artifacts/choices'
import type { BrowserRecoveryPreference } from '../../output/model/browser-delivery'
import type { SavingActionPresentation, SavingChoice, SavingOutcome, SavingOutcomeKind, SavingPresentationTransition } from './model'

const DESKTOP_GUIDANCE = 'Too large for browser saving. Install WindShare and run wind get with this share link.'

/** Eligibility and route ranking are supplied by the planner, never inferred from browser identity. */
export function presentSavingActions(input: Readonly<{
  offers: ArtifactOffers | null
  actionLabel?: string
  disabledReason?: string | null
}>): SavingActionPresentation {
  const offers = input.offers
  if (offers === null || offers.kind !== 'artifact-actions') return unavailable(input)
  const choices = uniqueChoices([
    offers.primary, ...offers.alternatives,
    ...(offers.zip === null ? [] : [offers.zip.primary, ...(offers.zip.secondary === null ? [] : [offers.zip.secondary])]),
  ])
  const primaryOffer = choices.find(choice => choice.choice.artifactKind === 'original-file') ??
    choices.find(choice => choice.choice.artifactKind === 'directory-tree') ??
    offers.zip?.primary ?? offers.primary
  const disabledReason = input.disabledReason ?? null
  const primary = savingChoice(primaryOffer, disabledReason, input.actionLabel)
  const alternatives = groupOutcomes(choices, offers, disabledReason)
  const totalChoiceCount = alternatives.reduce((count, group) => count + 1 + group.alternatives.length, 0)
  let reason: string = offers.zip?.recommendation.reason ?? 'eligible-route'
  if (primary.outcome === 'original-file') reason = 'original-file-default'
  if (primary.outcome === 'folder') reason = 'hierarchy-preserving-default'
  const recommendation = zipRecommendation(primary, offers.zip)
  return Object.freeze({
    primary,
    alternatives,
    hasAlternatives: totalChoiceCount > 1,
    disabledReason,
    retry: false,
    desktopGuidance: null,
    recommendation,
    transition: transition(offers, primary, reason),
  })
}

function zipRecommendation(primary: SavingChoice, zip: Extract<ArtifactOffers, { kind: 'artifact-actions' }>['zip']): string | null {
  if (primary.outcome !== 'zip' || zip === null) return null
  if (zip.recommendation.reason === 'workspace-within-discovered-budget') {
    return 'You can start now. Still discovering files — the recommended option may update.'
  }
  return zip.recommendation.kind === 'no-recommendation' && zip.recommendation.reason !== 'only-one-route-available'
    ? 'You can start now. Another ZIP option is available if needed.' : null
}

function groupOutcomes(
  choices: readonly OfferedArtifactChoice[],
  offers: Extract<ArtifactOffers, { kind: 'artifact-actions' }>,
  disabledReason: string | null,
): readonly SavingOutcome[] {
  const result: SavingOutcome[] = []
  for (const kind of ['original-file', 'folder', 'zip'] as const) {
    const candidates = choices.filter(choice => outcome(choice) === kind)
    if (candidates.length === 0) continue
    const primary = kind === 'zip' ? offers.zip?.primary ?? candidates[0]! : candidates[0]!
    // Only materially different saving processes stay visible within one result.
    const seen = new Set([consequenceIdentity(primary)])
    const alternatives = candidates.filter(candidate => {
      const identity = consequenceIdentity(candidate)
      if (seen.has(identity)) return false
      seen.add(identity)
      return true
    }).map(choice => savingChoice(choice, disabledReason))
    if (primary.route.kind === 'direct-tree' && primary.route.target.kind === 'fsa-parent-directory') {
      alternatives.unshift(savingChoice(primary, disabledReason, undefined, 'direct'))
    }
    result.push(Object.freeze({
      kind,
      label: outcomeLabel(kind),
      primary: savingChoice(primary, disabledReason),
      alternatives: Object.freeze(alternatives),
    }))
  }
  return Object.freeze(result)
}

function savingChoice(
  offered: OfferedArtifactChoice,
  disabledReason: string | null,
  actionLabel?: string,
  recoveryPreference: BrowserRecoveryPreference = 'automatic',
): SavingChoice {
  const kind = outcome(offered)
  const copy = routeCopy(offered, kind, recoveryPreference)
  return Object.freeze({
    offered,
    ...(offered.route.kind === 'direct-tree' ? { recoveryPreference } : {}),
    label: actionLabel ?? (recoveryPreference === 'direct' ? 'Write directly to folder' : choiceLabel(offered)),
    outcome: kind,
    summary: copy.summary,
    badges: Object.freeze(copy.badges),
    notes: Object.freeze(copy.notes),
    disabledReason,
  })
}

function choiceLabel(offered: OfferedArtifactChoice): string {
  switch (outcome(offered)) {
    case 'original-file':
      return offered.route.kind === 'workspace-then-publish' ? 'Resumable download' : 'Download original'
    case 'folder':
      return 'Save to folder'
    case 'zip':
      if (offered.route.kind === 'direct-resumable-zip') return 'Save ZIP to folder'
      return offered.route.kind === 'workspace-then-publish' ? 'Download as ZIP' : 'Quick ZIP download'
  }
}

function routeCopy(
  offered: OfferedArtifactChoice,
  kind: SavingOutcomeKind,
  recoveryPreference: BrowserRecoveryPreference,
): Readonly<{ summary: string; badges: string[]; notes: string[] }> {
  switch (offered.route.kind) {
    case 'direct-tree':
      if (recoveryPreference === 'automatic' && offered.route.target.kind === 'fsa-parent-directory') {
        return {
          summary: 'Pick a folder on your device. Keeps the folder layout and supports pause and resume.',
          badges: ['Keeps folders', 'Resumable'],
          notes: [
            'Larger files may stage in browser storage briefly while downloading, then move into your folder.',
            'Wait for Pause to finish before closing the tab. Clearing site data removes unfinished progress.',
          ],
        }
      }
      return {
        summary: offered.route.target.kind === 'fsa-parent-directory'
          ? 'Writes straight to your folder without browser temp storage. Uses less temporary space.'
          : 'Saves directly to your chosen folder with its original layout and resume support.',
        badges: offered.route.target.kind === 'fsa-parent-directory'
          ? ['Keeps folders', 'Less temp space']
          : ['Keeps folders', 'Resumable'],
        notes: offered.route.target.kind === 'fsa-parent-directory'
          ? [
              'Wait for Pause to finish before leaving. Closing or crashing mid-file may lose progress on an unfinished large file; completed files stay saved.',
              'Resuming a partially written file may temporarily use extra space in your folder.',
            ]
          : ['Wait for Pause to finish before leaving. Unexpected exits resume from the last verified checkpoint.'],
      }
    case 'direct-resumable-zip':
      return {
        summary: 'Writes one uncompressed ZIP directly into a folder you pick. Keep the unfinished file untouched until done.',
        badges: ['Single ZIP', 'Resumable', 'Less browser storage'],
        notes: [
          'The ZIP becomes usable once the download finishes and verifies.',
          'Wait for Pause to finish before leaving. Resuming may temporarily need extra space in the folder.',
        ],
      }
    case 'workspace-then-publish':
      if (kind === 'zip') {
        return {
          summary: 'Downloads all files reliably in browser storage first, then saves one uncompressed ZIP.',
          badges: ['Best for large files', 'Resumable', 'Single ZIP'],
          notes: [
            'The ZIP becomes usable once packaging finishes. Exporting requires space for the finished copy.',
            'Clearing site data removes unfinished progress and retained downloads.',
          ],
        }
      }
      return {
        summary: 'Saves progress in browser storage as it downloads, then exports the finished file.',
        badges: ['Best for large files', 'Resumable'],
        notes: [
          'Safer for slow or long downloads. Exporting the finished file copies it to your destination.',
          'Clearing site data removes unfinished progress and retained downloads.',
        ],
      }
    case 'portable-handoff':
      return kind === 'zip'
        ? {
            summary: 'Downloads one uncompressed ZIP directly in your browser. Keep this tab open until it finishes.',
            badges: ['Quick', 'Single ZIP', 'Not resumable'],
            notes: ['Refreshing or closing the tab cancels the download.'],
          }
        : {
            summary: 'Downloads directly via your browser. Keep this tab open until it finishes.',
            badges: ['Quick', 'Not resumable'],
            notes: ['Refreshing or closing the tab cancels the download.'],
          }
    case 'direct-atomic':
      return {
        summary: 'Saves directly to the location you choose. Must restart if interrupted.',
        badges: ['Not resumable'],
        notes: ['An interrupted transfer does not keep partial progress.'],
      }
  }
}

function outcome(offered: OfferedArtifactChoice): SavingOutcomeKind {
  if (offered.choice.artifactKind === 'directory-tree') return 'folder'
  return offered.choice.artifactKind === 'zip-archive' ? 'zip' : 'original-file'
}

function outcomeLabel(kind: SavingOutcomeKind): string {
  if (kind === 'original-file') return 'Original file'
  return kind === 'folder' ? 'Files in a folder' : 'ZIP archive'
}

function consequenceIdentity(choice: OfferedArtifactChoice): string {
  return `${choice.route.kind}:${choice.choice.recovery}`
}

function uniqueChoices(choices: readonly OfferedArtifactChoice[]): readonly OfferedArtifactChoice[] {
  const seen = new Set<string>()
  return choices.filter(choice => {
    if (seen.has(choice.choice.choiceId)) return false
    seen.add(choice.choice.choiceId)
    return true
  })
}

function unavailable(input: Readonly<{
  offers: ArtifactOffers | null
  disabledReason?: string | null
}>): SavingActionPresentation {
  const offers = input.offers
  const reason = offers === null ? 'confirming-selected-content' : offers.kind
  const copy = unavailableDescription(offers)
  return Object.freeze({
    primary: null,
    alternatives: Object.freeze([]),
    hasAlternatives: false,
    disabledReason: input.disabledReason ?? copy,
    retry: reason === 'retry-confirmation',
    desktopGuidance: offers?.kind === 'no-safe-destination' ? DESKTOP_GUIDANCE : null,
    recommendation: null,
    transition: transition(offers, null, reason),
  })
}

function unavailableDescription(offers: ArtifactOffers | null): string {
  if (offers?.kind === 'selection-empty') return 'Select items to download'
  if (offers?.kind === 'retry-confirmation') return 'Could not confirm the selection. Try again.'
  if (offers?.kind !== 'no-safe-destination') return 'Preparing download options…'
  if (offers.reason === 'workspace-limit-exceeded') return 'Not enough browser storage for this selection. Free space or select fewer items.'
  if (offers.reason === 'portable-limit-exceeded') return 'This selection is too large for direct browser download.'
  return 'This browser cannot save the selected items.'
}

function transition(offers: ArtifactOffers | null, primary: SavingChoice | null, reason: string): SavingPresentationTransition {
  const choiceId = primary?.offered.choice.choiceId ?? null
  return Object.freeze({
    name: 'receiver.saving.recommendation',
    projection_epoch: offers?.projectionEpoch ?? null,
    choice_id: choiceId,
    outcome: primary?.outcome ?? null,
    reason,
    fingerprint: [offers?.selectionDigest, choiceId, reason, primary?.disabledReason].join(':'),
  })
}

export function compareSavingTransition(
  previous: SavingPresentationTransition | null,
  current: SavingPresentationTransition,
): SavingPresentationTransition | null {
  return previous?.fingerprint === current.fingerprint ? null : current
}
