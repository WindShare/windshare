import type { ReceiveBlockReason, SaveUnavailableReason } from '../../application/receive/contracts'
import { presentReceiveReason } from '../experience/receive-reason'
import type { Language } from '../preferences/model'
import { receiverCopy } from '../locales/receiver'
import type { ArtifactChoice, ArtifactOffers } from '../../output/model/offers'
import type {
  SavingActionPresentation,
  SavingChoice,
  SavingOutcome,
  SavingOutcomeKind,
  SavingPresentationTransition,
} from './model'

const GIBIBYTE = 1024n * 1024n * 1024n

// Surface crash-recovery caveats on the primary control once a direct-folder transfer
// is large enough that losing an in-flight file to a browser crash becomes costly.
export const LARGE_DIRECT_DOWNLOAD_GUIDANCE_BYTES = 2n * GIBIBYTE

interface SavingRequest {
  readonly offers: ArtifactOffers | null
  readonly actionLabel?: string
  readonly blockedBy?: ReceiveBlockReason | null
  readonly localChoice?: Readonly<{ choiceId: string; reason: ReceiveBlockReason | null }> | null
}

function choiceBlockedReason(choice: ArtifactChoice, input: SavingRequest): ReceiveBlockReason | null {
  if (input.localChoice != null && choice.choiceId === input.localChoice.choiceId) {
    return input.localChoice.reason
  }
  return input.blockedBy ?? null
}

export function presentSavingActions(input: SavingRequest, language: Language = 'en'): SavingActionPresentation {
  const offers = input.offers
  if (offers === null || offers.kind !== 'ready') return unavailable(input, language)

  const choices = uniqueChoices([
    offers.primary,
    ...offers.alternatives,
    ...(offers.zip === null ? [] : [offers.zip]),
  ])
  const primaryBlockedBy = choiceBlockedReason(offers.primary, input)
  const disabledReason = presentReceiveReason(primaryBlockedBy, language)
  const primary = savingChoice(offers.primary, disabledReason, language, input.actionLabel)
  const alternatives = groupOutcomes(choices, offers, input, language)
  const totalChoiceCount = alternatives.reduce((count, group) => count + 1 + group.alternatives.length, 0)
  let reason = 'eligible-route'
  if (primary.outcome === 'original-file') reason = 'original-file-default'
  if (primary.outcome === 'folder') reason = 'hierarchy-preserving-default'

  const hasAlternatives = totalChoiceCount > 1

  return Object.freeze({
    primary,
    alternatives,
    hasAlternatives,
    disabledReason,
    retry: false,
    recoveryNotice: directRecoveryNotice(offers.primary, choices, hasAlternatives, language),
    desktopGuidance: null,
    recommendation: null,
    transition: transition(offers, primary, reason, primaryBlockedBy),
  })
}

function directRecoveryNotice(
  primary: ArtifactChoice,
  choices: readonly ArtifactChoice[],
  hasAlternatives: boolean,
  language: Language,
): string | null {
  if (primary.backend !== 'fsa') return null
  const t = receiverCopy[language].saving
  const singleFile = primary.artifact.singleFileId !== undefined
  if (!hasAlternatives) {
    // When FSA is the sole browser option (for example, exceeding OPFS quota), the
    // alternatives sheet and its notes are unreachable, so surface the caveat directly.
    return singleFile ? t.directFileCrashNotice : t.directFolderCrashNotice
  }
  const estimatedBytes = primary.estimatedBytes ?? 0n
  if (estimatedBytes < LARGE_DIRECT_DOWNLOAD_GUIDANCE_BYTES) return null
  const hasCrashRecoveryAlternative = choices.some(choice => choice.resumeSupport === 'continuous')
  if (!singleFile && hasCrashRecoveryAlternative) return t.directLargeFolderNotice
  return singleFile ? t.directFileCrashNotice : t.directFolderCrashNotice
}

function groupOutcomes(
  choices: readonly ArtifactChoice[],
  offers: Extract<ArtifactOffers, { kind: 'ready' }>,
  input: SavingRequest,
  language: Language,
): readonly SavingOutcome[] {
  const result: SavingOutcome[] = []
  for (const kind of ['original-file', 'folder', 'zip'] as const) {
    const candidates = choices.filter(choice => outcome(choice) === kind)
    if (candidates.length === 0) continue
    const primary = kind === 'zip' ? offers.zip ?? candidates[0]! : candidates[0]!
    const alternatives = candidates
      .filter(candidate => candidate.choiceId !== primary.choiceId)
      .map(choice => savingChoice(choice, presentReceiveReason(choiceBlockedReason(choice, input), language), language))
    result.push(Object.freeze({
      kind,
      label: outcomeLabel(kind, language),
      primary: savingChoice(primary, presentReceiveReason(choiceBlockedReason(primary, input), language), language),
      alternatives: Object.freeze(alternatives),
    }))
  }
  return Object.freeze(result)
}

function savingChoice(
  choice: ArtifactChoice,
  disabledReason: string | null,
  language: Language,
  actionLabel?: string,
): SavingChoice {
  const kind = outcome(choice)
  const copy = routeCopy(choice, kind, language)
  return Object.freeze({
    choice,
    label: actionLabel ?? choiceLabel(choice, language),
    outcome: kind,
    summary: copy.summary,
    detail: copy.detail,
    badges: Object.freeze(copy.badges),
    notes: Object.freeze(copy.notes),
    disabledReason,
  })
}

function choiceLabel(choice: ArtifactChoice, language: Language): string {
  const t = receiverCopy[language].saving
  switch (outcome(choice)) {
    case 'original-file':
      return choice.backend === 'opfs' ? t.resumableDownload : t.downloadOriginal
    case 'folder':
      return t.saveFolder
    case 'zip':
      return choice.backend === 'opfs' ? t.downloadZip : t.quickZip
  }
}

function resumeBadge(
  resumeSupport: ArtifactChoice['resumeSupport'],
  t: typeof receiverCopy.en.saving,
): string {
  switch (resumeSupport) {
    case 'continuous':
      return t.crashRecovery
    case 'pause':
      return t.pauseResume
    case 'none':
      return t.noResume
  }
}

function routeCopy(
  choice: ArtifactChoice,
  kind: SavingOutcomeKind,
  language: Language,
): Readonly<{ summary: string; detail: string; badges: string[]; notes: string[] }> {
  const t = receiverCopy[language].saving
  const resume = resumeBadge(choice.resumeSupport, t)
  if (choice.backend === 'fsa') {
    const singleFile = choice.artifact.singleFileId !== undefined
    return {
      summary: singleFile ? t.directFileSummary : t.directFolderSummary,
      detail: singleFile ? t.directFileDetail : t.directFolderDetail,
      badges: [singleFile ? t.directWrite : t.keepsFolders, resume],
      notes: [t.directPause],
    }
  }
  if (choice.backend === 'opfs') {
    return kind === 'zip'
      ? {
          summary: t.workspaceZip,
          detail: t.workspaceZipDetail,
          badges: [t.saferForLong, resume, t.singleZip],
          notes: [t.clearingData],
        }
      : {
          summary: t.workspaceFile,
          detail: t.workspaceFileDetail,
          badges: [t.saferForLong, resume],
          notes: [t.clearingData],
        }
  }
  return kind === 'zip'
    ? {
        summary: t.portableZip,
        detail: t.portableZip,
        badges: [t.quick, t.singleZip, resume],
        notes: [t.tabCancels],
      }
    : {
        summary: t.portableFile,
        detail: t.portableFile,
        badges: [t.quick, resume],
        notes: [t.tabCancels],
      }
}

function outcome(choice: ArtifactChoice): SavingOutcomeKind {
  if (choice.artifactKind === 'directory-tree') return 'folder'
  return choice.artifactKind === 'zip-archive' ? 'zip' : 'original-file'
}

function outcomeLabel(kind: SavingOutcomeKind, language: Language): string {
  const t = receiverCopy[language].saving
  if (kind === 'original-file') return t.original
  return kind === 'folder' ? t.folderResult : t.zipResult
}

function uniqueChoices(choices: readonly ArtifactChoice[]): readonly ArtifactChoice[] {
  const seen = new Set<string>()
  return choices.filter(choice => {
    if (seen.has(choice.choiceId)) return false
    seen.add(choice.choiceId)
    return true
  })
}

export function presentSaveUnavailable(
  reason: SaveUnavailableReason,
  saving: typeof receiverCopy.en.saving,
): Readonly<{ description: string; desktopGuidance: string | null }> {
  if (reason === 'storage-full') {
    return { description: saving.storageFull, desktopGuidance: saving.desktopGuidance }
  }
  if (reason === 'portable-limit-exceeded') {
    return { description: saving.tooLarge, desktopGuidance: saving.desktopGuidance }
  }
  // `no-safe-destination` is not a size or quota limit, so size-based desktop guidance would mislead.
  return { description: saving.cannotSave, desktopGuidance: null }
}

function unavailable(input: SavingRequest, language: Language): SavingActionPresentation {
  const t = receiverCopy[language].saving
  const offers = input.offers
  const reason = offers === null ? 'confirming' : offers.kind
  const unavailableOffer = offers?.kind === 'unavailable' ? presentSaveUnavailable(offers.reason, t) : null
  const copy = unavailableOffer?.description ?? unavailableDescription(offers, language)
  const terminalConnection = input.blockedBy === 'shareEnded' || input.blockedBy === 'relayAccessDenied' || input.blockedBy === 'connectionUnavailable'
  return Object.freeze({
    primary: null,
    alternatives: Object.freeze([]),
    hasAlternatives: false,
    disabledReason: presentReceiveReason(input.blockedBy ?? null, language) ?? copy,
    retry: reason === 'retry-required' && !terminalConnection,
    recoveryNotice: null,
    desktopGuidance: unavailableOffer?.desktopGuidance ?? null,
    recommendation: null,
    transition: transition(offers, null, reason, input.blockedBy),
  })
}

function unavailableDescription(offers: ArtifactOffers | null, language: Language): string {
  const t = receiverCopy[language].saving
  if (offers?.kind === 'selection-empty') return t.selectToDownload
  if (offers?.kind === 'retry-required') return t.confirmFailed
  return t.preparingOptions
}

function transition(
  offers: ArtifactOffers | null,
  primary: SavingChoice | null,
  reason: string,
  blockedBy?: ReceiveBlockReason | null,
): SavingPresentationTransition {
  const choiceId = primary?.choice.choiceId ?? null
  return Object.freeze({
    name: 'receiver.saving.recommendation',
    projection_epoch: offers?.epoch ?? null,
    choice_id: choiceId,
    outcome: primary?.outcome ?? null,
    reason,
    fingerprint: [offers?.selectionKey, choiceId, reason, blockedBy].join(':'),
  })
}

export function compareSavingTransition(
  previous: SavingPresentationTransition | null,
  current: SavingPresentationTransition,
): SavingPresentationTransition | null {
  return previous?.fingerprint === current.fingerprint ? null : current
}
