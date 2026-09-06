import type { ArtifactOffers, OfferedArtifactChoice } from '../../output/planning'
import type { SavingActionPresentation, SavingChoice, SavingOutcome, SavingOutcomeKind, SavingPresentationTransition } from './model'

const DESKTOP_GUIDANCE = 'Use the WindShare desktop receiver: install WindShare, then run wind get with this share link to save outside browser storage limits.'

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
  let reason: string = offers.zip?.recommendation.reason ?? 'eligible-route'
  if (primary.outcome === 'original-file') reason = 'original-file-default'
  if (primary.outcome === 'folder') reason = 'hierarchy-preserving-default'
  const recommendation = primary.outcome === 'zip' && offers.zip?.recommendation.kind === 'no-recommendation' &&
    offers.zip.recommendation.reason !== 'only-one-route-available'
    ? 'You can start now. The space comparison is not settled; another saving method remains available in Other ways to save.'
    : null
  return Object.freeze({
    primary, alternatives, disabledReason, retry: false, desktopGuidance: null, recommendation,
    transition: transition(offers, primary, reason),
  })
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
    result.push(Object.freeze({
      kind, label: outcomeLabel(kind),
      primary: savingChoice(primary, disabledReason),
      alternatives: Object.freeze(alternatives),
    }))
  }
  return Object.freeze(result)
}

function savingChoice(offered: OfferedArtifactChoice, disabledReason: string | null, actionLabel?: string): SavingChoice {
  const kind = outcome(offered)
  const descriptions: Record<SavingOutcomeKind, string> = {
    'original-file': 'Keeps the original file and format.',
    folder: 'Keeps the selected files and their folder hierarchy.',
    zip: 'Creates one ZIP containing the selected files, without compression.',
  }
  const description = descriptions[kind]
  const consequences = routeConsequences(offered)
  if (kind === 'zip') consequences.unshift('The result is a ZIP package. It is usable only after closing and verification finish.')
  return Object.freeze({
    offered,
    label: actionLabel ?? choiceLabel(offered),
    outcome: kind,
    description,
    consequences: Object.freeze(consequences),
    disabledReason,
  })
}

function choiceLabel(offered: OfferedArtifactChoice): string {
  switch (outcome(offered)) {
    case 'original-file': return 'Download original'
    case 'folder': return 'Save to folder'
    case 'zip':
      if (offered.route.kind === 'direct-resumable-zip') return 'Save ZIP to a folder'
      return offered.route.kind === 'workspace-then-publish' ? 'Receive ZIP, then save' : 'Download ZIP'
  }
}

function routeConsequences(offered: OfferedArtifactChoice): string[] {
  switch (offered.route.kind) {
    case 'direct-tree':
      return [
        'Choose a folder now. Completed files become visible as they arrive.',
        'Verified checkpoints can preserve progress. Checkpointing or resuming a partially written file may copy its saved prefix and need extra temporary destination space.',
      ]
    case 'direct-resumable-zip':
      return [
        'Choose a folder now. An unfinished ZIP is visible while downloading; keep it in place and unchanged.',
        'Only verified checkpoint bytes can resume after restart. Continuing may copy the existing ZIP and need temporary space as large as its committed length.',
      ]
    case 'workspace-then-publish':
      return [
        'Retains received content in browser storage. Saving writes an additional exported copy, so allow space for both.',
        offered.route.publicationTarget.kind === 'browser-handoff'
          ? 'The browser download starts automatically when allowed. If it does not start, choose Save; the retained copy stays available.'
          : 'The retained result is published to the authorized destination after receiving finishes.',
        'Clearing this site’s data removes retained progress and results.',
      ]
    case 'portable-handoff':
      return [
        'Checks the complete result fits before receiving. The browser download starts automatically when allowed; otherwise choose Save.',
        'Interrupted progress does not survive a page reload. Keep this page open until the browser download starts.',
      ]
    case 'direct-atomic':
      return ['The complete result is committed to the authorized destination. An interrupted transfer must start again.']
  }
}

function outcome(offered: OfferedArtifactChoice): SavingOutcomeKind {
  if (offered.choice.artifactKind === 'directory-tree') return 'folder'
  return offered.choice.artifactKind === 'zip-archive' ? 'zip' : 'original-file'
}

function outcomeLabel(kind: SavingOutcomeKind): string {
  if (kind === 'original-file') return 'Original file'
  return kind === 'folder' ? 'Files in a folder' : 'ZIP package'
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
    disabledReason: input.disabledReason ?? copy,
    retry: reason === 'retry-confirmation',
    desktopGuidance: offers?.kind === 'no-safe-destination' ? DESKTOP_GUIDANCE : null,
    recommendation: null,
    transition: transition(offers, null, reason),
  })
}

function unavailableDescription(offers: ArtifactOffers | null): string {
  if (offers?.kind === 'selection-empty') return 'Select items to download'
  if (offers?.kind === 'retry-confirmation') return 'Could not confirm the selection. Retry to keep the authenticated progress already found.'
  if (offers?.kind !== 'no-safe-destination') return 'Confirming the selected content…'
  if (offers.reason === 'workspace-limit-exceeded') return 'This selection exceeds available browser storage. Free space or choose a smaller selection.'
  if (offers.reason === 'portable-limit-exceeded') return 'This selection is too large for the available browser download method.'
  return 'This browser has no eligible saving method for the selected result.'
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
