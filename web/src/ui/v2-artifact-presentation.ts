import type {
  ArtifactAction,
  ArtifactOffers,
  ArtifactOperation,
  OfferUnavailableReason,
} from '../output/planning'

export interface PresentedArtifactAction {
  readonly action: ArtifactAction
  readonly operation: ArtifactOperation
  readonly label: string
  readonly description: string
  readonly importance: ArtifactAction['importance']
  readonly packageExplanation: string | null
}

export type ArtifactOfferPresentation =
  | Readonly<{
      kind: 'status'
      interactive: false
      title: string
      description: string
    }>
  | Readonly<{
      kind: 'retry'
      interactive: true
      title: string
      description: string
      label: 'Retry confirmation'
    }>
  | Readonly<{
      kind: 'actions'
      interactive: true
      primary: PresentedArtifactAction
      alternatives: readonly PresentedArtifactAction[]
    }>

const ZIP_PACKAGE_EXPLANATION =
  'Creates one ZIP package without compression. It becomes available only after every selected item is received.'

export function presentArtifactOffers(offers: ArtifactOffers): ArtifactOfferPresentation {
  switch (offers.kind) {
    case 'confirming-selected-content':
      return Object.freeze({
        kind: 'status',
        interactive: false,
        title: 'Confirming selected content…',
        description: 'WindShare is determining the final result. No save dialog will open automatically.',
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
      })
    case 'no-safe-destination':
      return Object.freeze({
        kind: 'status',
        interactive: false,
        title: unavailableTitle(offers.reason),
        description: unavailableDescription(offers.reason),
      })
    case 'artifact-actions':
      return Object.freeze({
        kind: 'actions',
        interactive: true,
        primary: presentArtifactAction(offers.primary),
        alternatives: Object.freeze(offers.alternatives.map(presentArtifactAction)),
      })
  }
}

export function presentArtifactAction(action: ArtifactAction): PresentedArtifactAction {
  return Object.freeze({
    action,
    operation: action.operation,
    label: artifactActionLabel(action),
    description: artifactActionDescription(action),
    importance: action.importance,
    packageExplanation: action.artifactKind === 'zip-archive' ? ZIP_PACKAGE_EXPLANATION : null,
  })
}

function artifactActionLabel(action: ArtifactAction): string {
  switch (action.operation) {
    case 'download-original':
      return `Download ${requiredSuggestedName(action)}`
    case 'save-single-to-folder':
      return 'Save to folder'
    case 'save-directory-tree':
      return 'Save using original folder hierarchy'
    case 'download-zip':
      return `Download ${requiredSuggestedName(action)}`
    case 'check-then-download':
      return 'Check then download'
  }
}

function artifactActionDescription(action: ArtifactAction): string {
  if (action.operation === 'save-single-to-folder') {
    return 'Saves the file directly in the folder you choose. The file may be visible before it finishes, and retained progress can continue later.'
  }
  if (action.operation === 'save-directory-tree') {
    return 'Keeps the selected folder hierarchy. Completed files remain visible, and retained progress can continue later.'
  }
  if (action.operation === 'check-then-download') {
    return action.artifactKind === 'zip-archive'
      ? 'Checks that the complete ZIP fits before receiving any file content. The browser takes over when the package is ready.'
      : 'Checks that the complete file fits before receiving it. The browser takes over when the file is ready.'
  }
  if (action.artifactKind === 'zip-archive') {
    return action.recovery === 'workspace-resumable'
      ? 'Receives every selected item before saving the complete ZIP. Retained progress can continue later.'
      : 'Saves one complete ZIP only after every selected item succeeds.'
  }
  return action.recovery === 'workspace-resumable'
    ? 'Receives the complete file before saving it. Retained progress can continue later.'
    : 'Saves the complete file without publishing an incomplete result.'
}

function requiredSuggestedName(action: ArtifactAction): string {
  if (action.suggestedName === null) {
    throw new TypeError(`${action.operation} requires a suggested artifact name`)
  }
  return action.suggestedName
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
