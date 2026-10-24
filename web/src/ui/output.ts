import { EMPTY_RECEIVE_RUNTIME, type ReceiveRuntimeSnapshot, type ReceiveRuntimeState } from '../application/receive/state'
import {
  presentArtifactActivation, presentArtifactOffers,
  type ArtifactActivationPresentation, type ArtifactOfferPresentation,
} from './artifact-presentation'
import { presentReceiveLifecycle, type ReceiveLifecyclePresentation } from './lifecycle-presentation'
import { presentTransferResult, type TransferResultPresentation } from './transfer-result'

export interface OutputPresentationSnapshot extends Omit<ReceiveRuntimeSnapshot, 'transferResult' | 'activeOfferedChoice'> {
  readonly offerPresentation: ArtifactOfferPresentation | null
  readonly activationPresentation: ArtifactActivationPresentation | null
  readonly lifecyclePresentation: ReceiveLifecyclePresentation | null
  readonly transferResultPresentation: TransferResultPresentation | null
}

export function presentReceiveOutput(facts: ReceiveRuntimeSnapshot): OutputPresentationSnapshot {
  const { activeOfferedChoice, transferResult, ...snapshot } = facts
  const lifecyclePresentation = facts.lifecycle === null || facts.resolvedArtifact === null || facts.plan === null
    ? null
    : presentReceiveLifecycle({
        state: facts.lifecycle, artifact: facts.resolvedArtifact, plan: facts.plan,
        workspaceUsage: facts.workspaceUsage, activeControls: facts.activeControls,
        interruption: facts.receiveInterruption, repairSummary: facts.repairSummary,
        recoverySummary: facts.recoverySummary, writerOpenPause: facts.writerOpenPause,
        directZipProgress: facts.directZipProgress,
        browserDelivery: facts.browserFolderProgress?.summary ?? null,
      })
  return Object.freeze({
    ...snapshot,
    workspaceUsage: lifecyclePresentation?.usage == null ? null : Object.freeze({
      ownedBytes: lifecyclePresentation.usage.ownedBytes,
      ...(lifecyclePresentation.usage.maximumBytes === undefined ? {} : { maximumBytes: lifecyclePresentation.usage.maximumBytes }),
    }),
    offerPresentation: facts.offers === null ? null : presentArtifactOffers(facts.offers),
    activationPresentation: presentArtifactActivation(facts.activation, activeOfferedChoice),
    lifecyclePresentation,
    transferResultPresentation: transferResult === null ? null :
      presentTransferResult(transferResult.worker, facts.repairSummary, transferResult.lifecycle),
  })
}

export const EMPTY_OUTPUT_PRESENTATION = presentReceiveOutput(EMPTY_RECEIVE_RUNTIME)

/** Rendering can be replaced or detached without changing the task's authority. */
export class OutputPresentationController {
  readonly #listeners = new Set<() => void>()
  readonly #unsubscribe: () => void
  #snapshot: OutputPresentationSnapshot

  constructor(runtime: Pick<ReceiveRuntimeState, 'getSnapshot' | 'subscribe'>) {
    this.#snapshot = presentReceiveOutput(runtime.getSnapshot())
    this.#unsubscribe = runtime.subscribe(() => {
      this.#snapshot = presentReceiveOutput(runtime.getSnapshot())
      for (const listener of this.#listeners) listener()
    })
  }

  readonly getSnapshot = (): OutputPresentationSnapshot => this.#snapshot
  readonly subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener)
    return () => this.#listeners.delete(listener)
  }

  close(): void {
    this.#unsubscribe()
    this.#listeners.clear()
  }
}
