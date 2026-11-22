import type { Language } from './preferences/model'
import { receiverCopy } from './locales/receiver'
import { activationOwnsChoice } from '../application/receive/activation/model'
import type { ArtifactChoice, ArtifactOperation, OfferedArtifactChoice } from '../output/model/artifacts/choices'
import type { ArtifactOffers, OfferUnavailableReason, ZipRouteGroup } from '../output/model/artifacts/offers'
import type { ProjectedByteCount } from '../output/model/artifacts/projections'
import type { AuthorityActivationSnapshot } from '../application/receive/activation/model'
import { formatBytes } from './progress-presentation'

export interface PresentedArtifactChoice {
  readonly offeredChoice: OfferedArtifactChoice | null
  readonly choice: ArtifactChoice
  readonly operation: ArtifactOperation
  readonly label: string
  readonly description: string
  readonly importance: OfferedArtifactChoice['importance']
  readonly packageExplanation: string | null
  readonly selectedBytes: string
  readonly resultBytes: string
}

export type ZipModePresentation =
  | Readonly<{
      kind: 'routes'
      title: string
      description: string
      primary: PresentedArtifactChoice
      secondary: PresentedArtifactChoice | null
      recommendation: string
    }>
  | Readonly<{
      kind: 'native-fallback'
      title: string
      description: string
    }>

export type ArtifactOfferPresentation =
  | Readonly<{
      kind: 'status'
      interactive: false
      title: string
      description: string
      nativeFallback: string | null
    }>
  | Readonly<{
      kind: 'retry'
      interactive: true
      title: string
      description: string
      label: string
    }>
  | Readonly<{
      kind: 'choices'
      interactive: true
      defaultChoices: readonly PresentedArtifactChoice[]
      zipMode: ZipModePresentation | null
    }>

export type ArtifactActivationPresentation =
  | Readonly<{
      kind: 'waiting'
      choice: PresentedArtifactChoice
      title: string
      description: string
    }>
  | Readonly<{
      kind: 'retry'
      choice: PresentedArtifactChoice
      title: string
      description: string
      label: string
    }>
  | Readonly<{
      kind: 'committing'
      choice: PresentedArtifactChoice
      title: string
      description: string
    }>

export function presentArtifactOffers(offers: ArtifactOffers, language: Language = 'en'): ArtifactOfferPresentation {
  const t = receiverCopy[language].artifacts
  switch (offers.kind) {
    case 'confirming-selected-content':
      return Object.freeze({
        kind: 'status',
        interactive: false,
        title: t.confirming,
        description: t.confirmingHint,
        nativeFallback: null,
      })
    case 'retry-confirmation':
      return Object.freeze({
        kind: 'retry',
        interactive: true,
        title: t.unableToConfirm,
        description: t.retryHint,
        label: t.retry,
      })
    case 'selection-empty':
      return Object.freeze({
        kind: 'status',
        interactive: false,
        title: t.nothingSelected,
        description: t.selectFirst,
        nativeFallback: null,
      })
    case 'no-safe-destination':
      return Object.freeze({
        kind: 'status',
        interactive: false,
        title: unavailableTitle(offers.reason, language),
        description: unavailableDescription(offers.reason, language),
        nativeFallback: offers.fallback.kind === 'native-recommended'
          ? t.nativeFallback
          : null,
      })
    case 'artifact-actions': {
      const choices = [offers.primary, ...offers.alternatives]
      const defaultChoices = choices
        .filter(candidate => candidate.choice.artifactKind !== 'zip-archive')
        .map(candidate => presentArtifactChoice(candidate, language))
      const zipMode = presentZipMode(offers.zip, choices, language)
      return Object.freeze({
        kind: 'choices',
        interactive: true,
        defaultChoices: Object.freeze(defaultChoices),
        zipMode,
      })
    }
  }
}

export function presentArtifactChoice(offered: OfferedArtifactChoice, language: Language = 'en'): PresentedArtifactChoice {
  return presentChoice(offered.choice, offered.importance, offered.suggestedName, offered, language)
}

export function presentArtifactActivation(
  activation: AuthorityActivationSnapshot,
  offered: OfferedArtifactChoice | null,
  language: Language = 'en',
): ArtifactActivationPresentation | null {
  const t = receiverCopy[language].artifacts
  if (!activationOwnsChoice(activation)) return null
  const choice = presentChoice(
    activation.choice,
    offered?.importance ?? 'primary',
    offered?.suggestedName ?? null,
    offered, language,
  )
  switch (activation.kind) {
    case 'waiting-authority':
      return Object.freeze({
        kind: 'waiting',
        choice,
        title: t.waitingLocation,
        description: activation.resolution.kind === 'resolved'
          ? t.completePrompt
          : t.keepPrompt,
      })
    case 'waiting-resolution':
      return Object.freeze({
        kind: 'waiting',
        choice,
        title: t.confirming,
        description: t.retainingChoice,
      })
    case 'retry-required':
      return Object.freeze({
        kind: 'retry',
        choice,
        title: t.confirmationPaused,
        description: t.retryPaused,
        label: t.retry,
      })
    case 'cleanup-required':
      return Object.freeze({
        kind: 'retry',
        choice,
        title: t.cleanupNeeded,
        description: activation.failedStage === 'settlement'
          ? t.retrySettlement
          : t.retryRelease,
        label: t.retry,
      })
    case 'committing':
      return Object.freeze({
        kind: 'committing',
        choice,
        title: t.preparing,
        description: t.bindingLocation,
      })
  }
}

export function activationLocksSelection(
  activation: AuthorityActivationSnapshot,
): boolean {
  if (activationOwnsChoice(activation)) return true
  return activation.kind === 'terminal' && activation.outcome.kind === 'bound-operation'
}



function presentChoice(
  choice: ArtifactChoice,
  importance: OfferedArtifactChoice['importance'],
  suggestedName: string | null,
  offeredChoice: OfferedArtifactChoice | null,
  language: Language,
): PresentedArtifactChoice {
  const t = receiverCopy[language].artifacts
  return Object.freeze({
    offeredChoice,
    choice,
    operation: choice.operation,
    label: artifactChoiceLabel(choice, suggestedName, language),
    description: artifactChoiceDescription(choice, language),
    importance,
    packageExplanation: choice.artifactKind === 'zip-archive' ? t.zipDescription : null,
    selectedBytes: projectedBytes(t.selection, offeredChoice?.sizeProjection.raw, language),
    resultBytes: projectedBytes(
      choice.artifactKind === 'zip-archive' ? t.zip : t.result,
      offeredChoice?.sizeProjection.artifact, language,
    ),
  })
}

function artifactChoiceLabel(choice: ArtifactChoice, suggestedName: string | null, language: Language): string {
  const t = receiverCopy[language].artifacts
  if (choice.artifactKind === 'zip-archive') {
    switch (choice.plan.kind) {
      case 'direct-resumable-zip': return t.zipToFolder
      case 'workspace-then-publish': return t.zipAfterReceive
      case 'portable-handoff': return t.checkZip
      case 'direct-tree':
        break
    }
  }
  switch (choice.operation) {
    case 'download-original':
      return suggestedName === null ? t.downloadFile : t.downloadNamed(suggestedName)
    case 'save-single-to-folder':
      return t.saveFolder
    case 'save-directory-tree':
      return t.preserveHierarchy
    case 'download-zip':
      return suggestedName === null ? t.downloadZip : t.downloadNamed(suggestedName)
    case 'check-then-download':
      return t.checkDownload
  }
}

function artifactChoiceDescription(choice: ArtifactChoice, language: Language): string {
  const t = receiverCopy[language].artifacts
  if (choice.operation === 'save-single-to-folder') {
    return t.fileToFolderHint
  }
  if (choice.operation === 'save-directory-tree') {
    return t.folderHint
  }
  if (choice.operation === 'check-then-download') {
    return choice.artifactKind === 'zip-archive'
      ? t.checkZipHint
      : t.checkFileHint
  }
  if (choice.artifactKind === 'zip-archive') {
    if (choice.plan.kind === 'direct-resumable-zip') {
      return t.directZipHint
    }
    return choice.recovery === 'workspace-resumable'
      ? t.workspaceZipHint
      : t.handoffZipHint
  }
  return choice.recovery === 'workspace-resumable'
    ? t.workspaceFileHint
    : t.completeFileHint
}

function presentZipMode(
  group: ZipRouteGroup | null,
  choices: readonly OfferedArtifactChoice[],
  language: Language,
): ZipModePresentation | null {
  const t = receiverCopy[language].artifacts
  if (group !== null) {
    return Object.freeze({
      kind: 'routes',
      title: t.zip,
      description: t.zipDescription,
      primary: presentArtifactChoice(group.primary, language),
      secondary: group.secondary === null ? null : presentArtifactChoice(group.secondary, language),
      recommendation: zipRecommendationCopy(group, language),
    })
  }
  return choices.some(choice => choice.choice.artifactKind === 'directory-tree')
    ? Object.freeze({
        kind: 'native-fallback',
        title: t.zip,
        description: t.nativeFallback,
      })
    : null
}

function zipRecommendationCopy(group: ZipRouteGroup, language: Language): string {
  const t = receiverCopy[language].artifacts
  if (group.recommendation.kind === 'recommended') {
    if (group.recommendation.reason === 'workspace-within-discovered-budget') {
      return t.zipEstimate
    }
    return group.recommendation.reason === 'workspace-within-policy-budget'
      ? t.recommendWorkspace
      : t.recommendFolder
  }
  switch (group.recommendation.reason) {
    case 'only-one-route-available': return t.onlyRoute
    case 'no-browser-zip-route': return t.nativeFallback
    case 'discovery-incomplete': return t.stillDiscovering
    case 'workspace-cost-unavailable': return t.unknownWorkspace
    case 'recommendation-policy-unavailable': return t.unknownComparison
  }
}

function projectedBytes(label: string, value: ProjectedByteCount | undefined, language: Language): string {
  const t = receiverCopy[language].artifacts
  if (value === undefined) return t.sizeUnavailable(label)
  const exactness = value.kind === t.exact ? t.exact : t.estimated
  return t.projection(label, formatBytes(value.bytes), exactness)
}

function unavailableTitle(reason: OfferUnavailableReason, language: Language): string {
  const t = receiverCopy[language].artifacts
  switch (reason) {
    case 'portable-limit-exceeded':
      return t.sizeLimit
    case 'workspace-limit-exceeded':
      return t.storageLimit
    case 'permission-denied':
      return t.permission
    case 'capability-changed':
      return t.changed
    case 'selection-empty':
      return t.nothingSelected
    case 'shape-unsettled':
      return t.unsettled
    case 'discovery-retry-required':
      return t.retryRequired
    case 'no-safe-destination':
      return t.noDestination
  }
}

function unavailableDescription(reason: OfferUnavailableReason, language: Language): string {
  const t = receiverCopy[language].artifacts
  switch (reason) {
    case 'portable-limit-exceeded':
      return t.sizeLimitHint
    case 'workspace-limit-exceeded':
      return t.storageLimitHint
    case 'permission-denied':
      return t.permissionHint
    case 'capability-changed':
      return t.changedHint
    case 'selection-empty':
      return t.selectFirst
    case 'shape-unsettled':
      return t.unsettledHint
    case 'discovery-retry-required':
      return t.retryRequiredHint
    case 'no-safe-destination':
      return t.noDestinationHint
  }
}
