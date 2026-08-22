import type {
  ArtifactChoice,
  ArtifactOffers,
  ArtifactOperation,
  OfferUnavailableReason,
  OfferedArtifactChoice,
  ProjectedByteCount,
  ZipRouteGroup,
} from '../output/planning'
import type { V2AuthorityActivationSnapshot } from './controller/activation-model'
import { formatBytes } from './v2-progress-presentation'

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
      title: 'ZIP package'
      description: string
      primary: PresentedArtifactChoice
      secondary: PresentedArtifactChoice | null
      recommendation: string
    }>
  | Readonly<{
      kind: 'native-fallback'
      title: 'ZIP package'
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
      label: 'Retry confirmation'
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
      label: 'Retry confirmation'
    }>
  | Readonly<{
      kind: 'committing'
      choice: PresentedArtifactChoice
      title: string
      description: string
    }>

const ZIP_PACKAGE_EXPLANATION =
  'Package only: creates one ZIP package and stores files without compression. The ZIP is usable only after closing and verification finish.'

const NATIVE_ZIP_FALLBACK =
  'This browser has no safe ZIP route for the selection. Use the native receiver for this ZIP.'

export function presentArtifactOffers(offers: ArtifactOffers): ArtifactOfferPresentation {
  switch (offers.kind) {
    case 'confirming-selected-content':
      return Object.freeze({
        kind: 'status',
        interactive: false,
        title: 'Confirming selected content…',
        description: 'WindShare is determining the final result. No save dialog will open automatically.',
        nativeFallback: null,
      })
    case 'retry-confirmation':
      return Object.freeze({
        kind: 'retry',
        interactive: true,
        title: 'The selection could not be confirmed yet.',
        description: 'Authenticated progress was kept. Retry to continue confirming the same selection.',
        label: 'Retry confirmation',
      })
    case 'selection-empty':
      return Object.freeze({
        kind: 'status',
        interactive: false,
        title: 'Nothing is selected.',
        description: 'Select a file or folder before receiving.',
        nativeFallback: null,
      })
    case 'no-safe-destination':
      return Object.freeze({
        kind: 'status',
        interactive: false,
        title: unavailableTitle(offers.reason),
        description: unavailableDescription(offers.reason),
        nativeFallback: offers.fallback.kind === 'native-recommended'
          ? NATIVE_ZIP_FALLBACK
          : null,
      })
    case 'artifact-actions': {
      const choices = [offers.primary, ...offers.alternatives]
      const defaultChoices = choices
        .filter(candidate => candidate.choice.artifactKind !== 'zip-archive')
        .map(presentArtifactChoice)
      const zipMode = presentZipMode(offers.zip, choices)
      return Object.freeze({
        kind: 'choices',
        interactive: true,
        defaultChoices: Object.freeze(defaultChoices),
        zipMode,
      })
    }
  }
}

export function presentArtifactChoice(offered: OfferedArtifactChoice): PresentedArtifactChoice {
  return presentChoice(offered.choice, offered.importance, offered.suggestedName, offered)
}

export function presentArtifactActivation(
  activation: V2AuthorityActivationSnapshot,
  offered: OfferedArtifactChoice | null,
): ArtifactActivationPresentation | null {
  if (!activationPresentsChoice(activation)) return null
  const choice = presentChoice(
    activation.choice,
    offered?.importance ?? 'primary',
    offered?.suggestedName ?? null,
    offered,
  )
  switch (activation.kind) {
    case 'waiting-authority':
      return Object.freeze({
        kind: 'waiting',
        choice,
        title: 'Waiting for the save location…',
        description: activation.resolution.kind === 'resolved'
          ? 'The result is ready. Complete the open browser prompt to continue.'
          : 'Keep the browser prompt open while WindShare confirms the selected content.',
      })
    case 'waiting-resolution':
      return Object.freeze({
        kind: 'waiting',
        choice,
        title: 'Confirming selected content…',
        description: 'Your save choice is kept while authenticated discovery finishes.',
      })
    case 'retry-required':
      return Object.freeze({
        kind: 'retry',
        choice,
        title: 'Selection confirmation paused.',
        description: 'Your save choice and any completed browser prompt are kept. Retry confirmation to continue.',
        label: 'Retry confirmation',
      })
    case 'cleanup-required':
      return Object.freeze({
        kind: 'retry',
        choice,
        title: 'Output cleanup needs attention.',
        description: activation.failedStage === 'settlement'
          ? 'Retry to record a durable disposition for the interrupted output.'
          : 'The interrupted output was settled. Retry to release its local authority.',
        label: 'Retry confirmation',
      })
    case 'committing':
      return Object.freeze({
        kind: 'committing',
        choice,
        title: 'Preparing the selected result…',
        description: 'WindShare is binding the confirmed result to the save location.',
      })
  }
}

export function activationLocksSelection(
  activation: V2AuthorityActivationSnapshot,
): boolean {
  if (activationPresentsChoice(activation)) return true
  return activation.kind === 'terminal' && activation.outcome.kind === 'bound-operation'
}

export function activationPresentsChoice(
  activation: V2AuthorityActivationSnapshot,
): activation is Extract<V2AuthorityActivationSnapshot, {
  readonly kind: 'waiting-authority' | 'waiting-resolution' | 'retry-required' | 'committing' |
    'cleanup-required'
}> {
  switch (activation.kind) {
    case 'waiting-authority':
    case 'waiting-resolution':
    case 'retry-required':
    case 'committing':
    case 'cleanup-required':
      return true
    case 'inactive':
    case 'terminal':
      return false
  }
}

function presentChoice(
  choice: ArtifactChoice,
  importance: OfferedArtifactChoice['importance'],
  suggestedName: string | null,
  offeredChoice: OfferedArtifactChoice | null,
): PresentedArtifactChoice {
  return Object.freeze({
    offeredChoice,
    choice,
    operation: choice.operation,
    label: artifactChoiceLabel(choice, suggestedName),
    description: artifactChoiceDescription(choice),
    importance,
    packageExplanation: choice.artifactKind === 'zip-archive' ? ZIP_PACKAGE_EXPLANATION : null,
    selectedBytes: projectedBytes('Selected content', offeredChoice?.sizeProjection.raw),
    resultBytes: projectedBytes(
      choice.artifactKind === 'zip-archive' ? 'ZIP package' : 'Result',
      offeredChoice?.sizeProjection.artifact,
    ),
  })
}

function artifactChoiceLabel(choice: ArtifactChoice, suggestedName: string | null): string {
  if (choice.artifactKind === 'zip-archive') {
    switch (choice.plan.kind) {
      case 'direct-resumable-zip': return 'Save ZIP to a folder'
      case 'workspace-then-publish': return 'Save ZIP after receiving completes'
      case 'portable-handoff': return 'Check then download ZIP'
      case 'direct-tree':
      case 'direct-atomic':
        break
    }
  }
  switch (choice.operation) {
    case 'download-original':
      return suggestedName === null ? 'Download selected file' : `Download ${suggestedName}`
    case 'save-single-to-folder':
      return 'Save to folder'
    case 'save-directory-tree':
      return 'Save using original folder hierarchy'
    case 'download-zip':
      return suggestedName === null ? 'Download ZIP' : `Download ${suggestedName}`
    case 'check-then-download':
      return 'Check then download'
  }
}

function artifactChoiceDescription(choice: ArtifactChoice): string {
  if (choice.operation === 'save-single-to-folder') {
    return 'Saves the file directly in the folder you choose. The file may be visible before it finishes, and retained progress can continue later.'
  }
  if (choice.operation === 'save-directory-tree') {
    return 'Keeps the selected folder hierarchy. Completed files remain visible, and retained progress can continue later.'
  }
  if (choice.operation === 'check-then-download') {
    return choice.artifactKind === 'zip-archive'
      ? 'Checks that the complete ZIP fits before receiving any file content. The browser takes over when the package is ready.'
      : 'Checks that the complete file fits before receiving it. The browser takes over when the file is ready.'
  }
  if (choice.artifactKind === 'zip-archive') {
    if (choice.plan.kind === 'direct-resumable-zip') {
      return 'Creates the named ZIP in the folder you choose. An incomplete target is visible until closing and verification finish; do not move or modify it meanwhile.'
    }
    return choice.recovery === 'workspace-resumable'
      ? 'Receives every selected item before asking where to save the complete ZIP. Retained progress can continue later.'
      : 'Hands off one complete ZIP only after every selected item succeeds.'
  }
  return choice.recovery === 'workspace-resumable'
    ? 'Receives the complete file before saving it. Retained progress can continue later.'
    : 'Saves the complete file without publishing an incomplete result.'
}

function presentZipMode(
  group: ZipRouteGroup | null,
  choices: readonly OfferedArtifactChoice[],
): ZipModePresentation | null {
  if (group !== null) {
    return Object.freeze({
      kind: 'routes',
      title: 'ZIP package',
      description: ZIP_PACKAGE_EXPLANATION,
      primary: presentArtifactChoice(group.primary),
      secondary: group.secondary === null ? null : presentArtifactChoice(group.secondary),
      recommendation: zipRecommendationCopy(group),
    })
  }
  return choices.some(choice => choice.choice.artifactKind === 'directory-tree')
    ? Object.freeze({
        kind: 'native-fallback',
        title: 'ZIP package',
        description: NATIVE_ZIP_FALLBACK,
      })
    : null
}

function zipRecommendationCopy(group: ZipRouteGroup): string {
  if (group.recommendation.kind === 'recommended') {
    return group.recommendation.reason === 'workspace-within-reviewed-budget'
      ? 'Recommended: receive completely first because the checked local cost is within the reviewed budget.'
      : 'Recommended: save to a folder because complete-first cost is unknown or exceeds the reviewed budget.'
  }
  switch (group.recommendation.reason) {
    case 'only-one-route-available': return 'This is the only safe browser ZIP route currently available.'
    case 'no-browser-zip-route': return NATIVE_ZIP_FALLBACK
    case 'discovery-incomplete': return 'No route is recommended until selected-content discovery is complete.'
    case 'workspace-cost-unavailable': return 'No route is recommended because complete-first cost is unavailable.'
    case 'recommendation-policy-unavailable': return 'No route is recommended because measured comparison data is unavailable.'
  }
}

function projectedBytes(label: string, value: ProjectedByteCount | undefined): string {
  if (value === undefined) return `${label}: size unavailable`
  const exactness = value.kind === 'exact' ? 'exact' : 'estimated lower bound'
  return `${label}: ${formatBytes(value.bytes)} (${exactness})`
}

function unavailableTitle(reason: OfferUnavailableReason): string {
  switch (reason) {
    case 'portable-limit-exceeded':
      return 'The selection is too large for the available download option.'
    case 'workspace-limit-exceeded':
      return 'There is not enough browser storage for this complete result.'
    case 'permission-denied':
      return 'Permission is required to save this result.'
    case 'capability-changed':
      return 'The available save options changed.'
    case 'selection-empty':
      return 'Nothing is selected.'
    case 'shape-unsettled':
      return 'The selected content is still being confirmed.'
    case 'discovery-retry-required':
      return 'Selection confirmation must be retried.'
    case 'no-safe-destination':
      return 'This browser cannot safely create the selected result.'
  }
}

function unavailableDescription(reason: OfferUnavailableReason): string {
  switch (reason) {
    case 'portable-limit-exceeded':
      return 'Choose a smaller selection or use a browser that can save the complete result another way.'
    case 'workspace-limit-exceeded':
      return 'Free browser storage or choose the original folder hierarchy when that action is available.'
    case 'permission-denied':
      return 'Grant access and choose the artifact action again.'
    case 'capability-changed':
      return 'Review the available artifact actions and choose again.'
    case 'selection-empty':
      return 'Select a file or folder before receiving.'
    case 'shape-unsettled':
      return 'Wait for confirmation before choosing the final result.'
    case 'discovery-retry-required':
      return 'Retry confirmation to keep the authenticated progress already collected.'
    case 'no-safe-destination':
      return 'Use another supported browser or change the selection.'
  }
}
