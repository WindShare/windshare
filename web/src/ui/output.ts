import type { Language, UIPreferences } from './preferences/model'
import { receiverCopy } from './locales/receiver'
import { EMPTY_RECEIVE_RUNTIME, type ReceiveRuntimeSnapshot, type ReceiveRuntimeState, type TransferResultProjection } from '../application/receive/state'
import {
  presentArtifactActivation, presentArtifactOffers,
  type ArtifactActivationPresentation, type ArtifactOfferPresentation,
} from './artifact-presentation'
import { presentReceiveLifecycle, type ReceiveLifecyclePresentation } from './lifecycle-presentation'
import { presentTransferResult, type TransferResultPresentation } from './transfer-result'

export interface OutputPresentationSnapshot extends Omit<ReceiveRuntimeSnapshot, 'transferResult' | 'activeOfferedChoice'> {
  readonly language: Language
  readonly offerPresentation: ArtifactOfferPresentation | null
  readonly activationPresentation: ArtifactActivationPresentation | null
  readonly lifecyclePresentation: ReceiveLifecyclePresentation | null
  readonly transferResultPresentation: TransferResultPresentation | null
}

export function presentReceiveOutput(facts: ReceiveRuntimeSnapshot, language: Language = 'en'): OutputPresentationSnapshot {
  const { activeOfferedChoice, transferResult, ...snapshot } = facts
  const lifecyclePresentation = facts.lifecycle === null || facts.resolvedArtifact === null || facts.plan === null
    ? null
    : presentReceiveLifecycle({
        state: facts.lifecycle, artifact: facts.resolvedArtifact, plan: facts.plan,
        workspaceUsage: facts.workspaceUsage, activeControls: facts.activeControls,
        interruption: facts.receiveInterruption,
        recoverySummary: facts.recoverySummary, writerOpenPause: facts.writerOpenPause,
      }, language)
  return Object.freeze({
    ...snapshot, language,
    workspaceUsage: lifecyclePresentation?.usage == null ? null : Object.freeze({
      ownedBytes: lifecyclePresentation.usage.ownedBytes,
      ...(lifecyclePresentation.usage.maximumBytes === undefined ? {} : { maximumBytes: lifecyclePresentation.usage.maximumBytes }),
    }),
    offerPresentation: facts.offers === null ? null : presentArtifactOffers(facts.offers, language),
    activationPresentation: presentArtifactActivation(facts.activation, activeOfferedChoice, language),
    lifecyclePresentation,
    transferResultPresentation: transferResult === null ? null :
      presentSourceCoverage(transferResult,
        presentTransferResult(transferResult.worker, transferResult.lifecycle, language), language),
  })
}

function presentSourceCoverage(
  transfer: TransferResultProjection,
  presentation: TransferResultPresentation,
  language: Language,
): TransferResultPresentation {
  const t = receiverCopy[language].progress
  const omitted = transfer.measure.sourceOmittedEntries
  if (omitted === 0n) return presentation
  return Object.freeze({
    title: presentation.tone === 'success' ? t.completedSkipped : presentation.title,
    tone: 'warning',
    lines: Object.freeze([...presentation.lines,
      t.omitted(omitted),
    ]),
  })
}

export const EMPTY_OUTPUT_PRESENTATION = presentReceiveOutput(EMPTY_RECEIVE_RUNTIME)

export interface PresentationPreferences {
  getSnapshot(): Pick<UIPreferences, 'language'>
  subscribe(listener: () => void): () => void
}

/** Rendering can be replaced or detached without changing the task's authority. */
export class OutputPresentationController {
  readonly #listeners = new Set<() => void>()
  readonly #unsubscribe: () => void
  readonly #unsubscribePreferences: (() => void) | undefined
  #snapshot: OutputPresentationSnapshot

  constructor(runtime: Pick<ReceiveRuntimeState, 'getSnapshot' | 'subscribe'>, preferences?: PresentationPreferences) {
    const project = () => presentReceiveOutput(runtime.getSnapshot(), preferences?.getSnapshot().language)
    const refresh = () => {
      this.#snapshot = project()
      for (const listener of this.#listeners) listener()
    }
    this.#snapshot = project()
    this.#unsubscribe = runtime.subscribe(refresh)
    this.#unsubscribePreferences = preferences?.subscribe(() => {
      if (preferences.getSnapshot().language !== this.#snapshot.language) refresh()
    })
  }

  readonly getSnapshot = (): OutputPresentationSnapshot => this.#snapshot
  readonly subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener)
    return () => this.#listeners.delete(listener)
  }

  close(): void {
    this.#unsubscribe()
    this.#unsubscribePreferences?.()
    this.#listeners.clear()
  }
}
