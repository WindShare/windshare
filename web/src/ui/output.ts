import type { Language, UIPreferences } from './preferences/model'
import {
  EMPTY_RETAINED_INVENTORY,
  type ActiveActivationSnapshot,
  type BrowserCapabilitiesState,
  type ReceiveAction,
  type ReceiveRuntimeSnapshot,
  type ReceiveSettlingPhase,
} from '../application/receive/contracts'
import type { ArtifactOffers, SaveBackendKind } from '../output/model'
import { EMPTY_TRANSFER_PROGRESS } from '../transfer/progress'
import type { ReceiveLifecycleState, SafeRenamedSummary, SourceRevisionFailures } from '../transfer/state'
import { presentActivation, type ArtifactActivationPresentation } from './artifact-presentation'

export interface OutputPresentationSnapshot {
  readonly language: Language
  readonly offers: ArtifactOffers | null
  readonly capabilitiesState: BrowserCapabilitiesState
  readonly activation: ActiveActivationSnapshot | null
  readonly activationPresentation: ArtifactActivationPresentation | null
  readonly activeTaskId: string | null
  readonly activeSingleFileId: string | null
  readonly activeSelectionKey: string | null
  readonly backend: SaveBackendKind | null
  readonly lifecycle: ReceiveLifecycleState | null
  readonly safeRenamed: SafeRenamedSummary | null
  readonly sourceOmittedEntries: bigint
  readonly sourceRevisionFailures: SourceRevisionFailures | null
  readonly actions: readonly ReceiveAction[]
  readonly outageCheckpointed: boolean
  readonly settling: ReceiveSettlingPhase | null
}

export const EMPTY_RECEIVE_RUNTIME: ReceiveRuntimeSnapshot = Object.freeze({
  offers: null,
  capabilitiesState: 'probing',
  activation: null,
  activeTaskId: null,
  activeSingleFileId: null,
  activeSelectionKey: null,
  backend: null,
  lifecycle: null,
  display: null,
  safeRenamed: null,
  sourceOmittedEntries: 0n,
  sourceRevisionFailures: null,
  progress: EMPTY_TRANSFER_PROGRESS,
  actions: Object.freeze([]),
  outageCheckpointed: false,
  settling: null,
  retained: EMPTY_RETAINED_INVENTORY,
})

export function presentReceiveOutput(
  runtime: ReceiveRuntimeSnapshot,
  language: Language = 'en',
): OutputPresentationSnapshot {
  return Object.freeze({
    language,
    offers: runtime.offers,
    capabilitiesState: runtime.capabilitiesState,
    activation: runtime.activation,
    activationPresentation: presentActivation(runtime.activation, language),
    activeTaskId: runtime.activeTaskId,
    activeSingleFileId: runtime.activeSingleFileId,
    activeSelectionKey: runtime.activeSelectionKey,
    backend: runtime.backend,
    lifecycle: runtime.lifecycle,
    safeRenamed: runtime.safeRenamed,
    sourceOmittedEntries: runtime.sourceOmittedEntries,
    sourceRevisionFailures: runtime.sourceRevisionFailures,
    actions: runtime.actions,
    outageCheckpointed: runtime.outageCheckpointed,
    settling: runtime.settling,
  })
}

export const EMPTY_OUTPUT_PRESENTATION = presentReceiveOutput(EMPTY_RECEIVE_RUNTIME)

export interface PresentationPreferences {
  getSnapshot(): Pick<UIPreferences, 'language'>
  subscribe(listener: () => void): () => void
}
