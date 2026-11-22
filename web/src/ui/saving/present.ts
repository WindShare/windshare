import type { ReceiveBlockReason } from '../../application/receive/admission'
import { presentReceiveReason } from '../experience/receive-reason'
import type { Language } from '../preferences/model'
import { receiverCopy } from '../locales/receiver'
import type { ArtifactOffers } from '../../output/model/artifacts/offers'
import type { OfferedArtifactChoice } from '../../output/model/artifacts/choices'
import type { BrowserRecoveryPreference } from '../../output/model/browser-delivery'
import type { SavingActionPresentation, SavingChoice, SavingOutcome, SavingOutcomeKind, SavingPresentationTransition } from './model'

interface SavingRequest {
  readonly offers: ArtifactOffers | null
  readonly actionLabel?: string
  readonly blockedBy?: ReceiveBlockReason | null
}

/** Eligibility and route ranking are supplied by the planner, never inferred from browser identity. */
export function presentSavingActions(input: SavingRequest, language: Language = 'en'): SavingActionPresentation {
  const offers = input.offers
  if (offers === null || offers.kind !== 'artifact-actions') return unavailable(input, language)
  const choices = uniqueChoices([
    offers.primary, ...offers.alternatives,
    ...(offers.zip === null ? [] : [offers.zip]),
  ])
  const primaryOffer = choices.find(choice => choice.choice.artifactKind === 'original-file') ??
    choices.find(choice => choice.choice.artifactKind === 'directory-tree') ??
    offers.zip ?? offers.primary
  const disabledReason = presentReceiveReason(input.blockedBy ?? null, language)
  const primary = savingChoice(primaryOffer, disabledReason, language, input.actionLabel)
  const alternatives = groupOutcomes(choices, offers, disabledReason, language)
  const totalChoiceCount = alternatives.reduce((count, group) => count + 1 + group.alternatives.length, 0)
  let reason = 'eligible-route'
  if (primary.outcome === 'original-file') reason = 'original-file-default'
  if (primary.outcome === 'folder') reason = 'hierarchy-preserving-default'
  return Object.freeze({
    primary,
    alternatives,
    hasAlternatives: totalChoiceCount > 1,
    disabledReason,
    retry: false,
    desktopGuidance: null,
    recommendation: null,
    transition: transition(offers, primary, reason, input.blockedBy),
  })
}

function groupOutcomes(
  choices: readonly OfferedArtifactChoice[],
  offers: Extract<ArtifactOffers, { kind: 'artifact-actions' }>,
  disabledReason: string | null,
  language: Language,
): readonly SavingOutcome[] {
  const result: SavingOutcome[] = []
  for (const kind of ['original-file', 'folder', 'zip'] as const) {
    const candidates = choices.filter(choice => outcome(choice) === kind)
    if (candidates.length === 0) continue
    const primary = kind === 'zip' ? offers.zip ?? candidates[0]! : candidates[0]!
    // Only materially different saving processes stay visible within one result.
    const seen = new Set([consequenceIdentity(primary)])
    const alternatives = candidates.filter(candidate => {
      const identity = consequenceIdentity(candidate)
      if (seen.has(identity)) return false
      seen.add(identity)
      return true
    }).map(choice => savingChoice(choice, disabledReason, language))
    if (primary.route.kind === 'direct-tree' && primary.route.target.kind === 'fsa-parent-directory') {
      alternatives.unshift(savingChoice(primary, disabledReason, language, undefined, 'direct'))
    }
    result.push(Object.freeze({
      kind,
      label: outcomeLabel(kind, language),
      primary: savingChoice(primary, disabledReason, language),
      alternatives: Object.freeze(alternatives),
    }))
  }
  return Object.freeze(result)
}

function savingChoice(
  offered: OfferedArtifactChoice,
  disabledReason: string | null,
  language: Language,
  actionLabel?: string,
  recoveryPreference: BrowserRecoveryPreference = 'automatic',
): SavingChoice {
  const t = receiverCopy[language].saving
  const kind = outcome(offered)
  const copy = routeCopy(offered, kind, recoveryPreference, language)
  return Object.freeze({
    offered,
    ...(offered.route.kind === 'direct-tree' ? { recoveryPreference } : {}),
    label: actionLabel ?? (recoveryPreference === 'direct' ? t.directFolder : choiceLabel(offered, language)),
    outcome: kind,
    summary: copy.summary,
    badges: Object.freeze(copy.badges),
    notes: Object.freeze(copy.notes),
    disabledReason,
  })
}

function choiceLabel(offered: OfferedArtifactChoice, language: Language): string {
  const t = receiverCopy[language].saving
  switch (outcome(offered)) {
    case 'original-file':
      return offered.route.kind === 'workspace-then-publish' ? t.resumableDownload : t.downloadOriginal
    case 'folder':
      return t.saveFolder
    case 'zip':
      return offered.route.kind === 'workspace-then-publish' ? t.downloadZip : t.quickZip
  }
}

function routeCopy(
  offered: OfferedArtifactChoice,
  kind: SavingOutcomeKind,
  recoveryPreference: BrowserRecoveryPreference,
  language: Language,
): Readonly<{ summary: string; badges: string[]; notes: string[] }> {
  const t = receiverCopy[language].saving
  switch (offered.route.kind) {
    case 'direct-tree':
      if (recoveryPreference === 'automatic' && offered.route.target.kind === 'fsa-parent-directory') {
        return {
          summary: t.automaticFolderSummary,
          badges: [t.keepsFolders, t.resumable],
          notes: [
            t.stagingLarge,
            t.waitPauseSite,
          ],
        }
      }
      return {
        summary: offered.route.target.kind === 'fsa-parent-directory'
          ? t.directFolderSummary
          : t.folderSummary,
        badges: offered.route.target.kind === 'fsa-parent-directory'
          ? [t.keepsFolders, t.lessTemp]
          : [t.keepsFolders, t.resumable],
        notes: offered.route.target.kind === 'fsa-parent-directory'
          ? [
              t.directPause,
              t.resumeSpace,
            ]
          : [t.waitCheckpoint],
      }
    case 'workspace-then-publish':
      if (kind === 'zip') {
        return {
          summary: t.workspaceZip,
          badges: [t.largeFiles, t.resumable, t.singleZip],
          notes: [
            t.zipExport,
            t.clearingData,
          ],
        }
      }
      return {
        summary: t.workspaceFile,
        badges: [t.largeFiles, t.resumable],
        notes: [
          t.longDownloads,
          t.clearingData,
        ],
      }
    case 'portable-handoff':
      return kind === 'zip'
        ? {
            summary: t.portableZip,
            badges: [t.quick, t.singleZip, t.noResume],
            notes: [t.tabCancels],
          }
        : {
            summary: t.portableFile,
            badges: [t.quick, t.noResume],
            notes: [t.tabCancels],
          }
  }
}

function outcome(offered: OfferedArtifactChoice): SavingOutcomeKind {
  if (offered.choice.artifactKind === 'directory-tree') return 'folder'
  return offered.choice.artifactKind === 'zip-archive' ? 'zip' : 'original-file'
}

function outcomeLabel(kind: SavingOutcomeKind, language: Language): string {
  const t = receiverCopy[language].saving
  if (kind === 'original-file') return t.original
  return kind === 'folder' ? t.folderResult : t.zipResult
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

function unavailable(input: SavingRequest, language: Language): SavingActionPresentation {
  const t = receiverCopy[language].saving
  const offers = input.offers
  const reason = offers === null ? 'confirming-selected-content' : offers.kind
  const copy = unavailableDescription(offers, language)
  return Object.freeze({
    primary: null,
    alternatives: Object.freeze([]),
    hasAlternatives: false,
    disabledReason: presentReceiveReason(input.blockedBy ?? null, language) ?? copy,
    retry: reason === 'retry-confirmation',
    desktopGuidance: offers?.kind === 'no-safe-destination' ? t.desktopGuidance : null,
    recommendation: null,
    transition: transition(offers, null, reason, input.blockedBy),
  })
}

function unavailableDescription(offers: ArtifactOffers | null, language: Language): string {
  const t = receiverCopy[language].saving
  if (offers?.kind === 'selection-empty') return t.selectToDownload
  if (offers?.kind === 'retry-confirmation') return t.confirmFailed
  if (offers?.kind !== 'no-safe-destination') return t.preparingOptions
  if (offers.reason === 'workspace-limit-exceeded') return t.storageFull
  if (offers.reason === 'portable-limit-exceeded') return t.tooLarge
  return t.cannotSave
}

function transition(offers: ArtifactOffers | null, primary: SavingChoice | null, reason: string, blockedBy?: ReceiveBlockReason | null): SavingPresentationTransition {
  const choiceId = primary?.offered.choice.choiceId ?? null
  return Object.freeze({
    name: 'receiver.saving.recommendation',
    projection_epoch: offers?.projectionEpoch ?? null,
    choice_id: choiceId,
    outcome: primary?.outcome ?? null,
    reason,
    fingerprint: [offers?.selectionDigest, choiceId, reason, blockedBy].join(':'),
  })
}

export function compareSavingTransition(
  previous: SavingPresentationTransition | null,
  current: SavingPresentationTransition,
): SavingPresentationTransition | null {
  return previous?.fingerprint === current.fingerprint ? null : current
}
