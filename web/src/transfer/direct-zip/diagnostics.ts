import {
  observeDirectZipDiagnostic,
  type DirectZipDiagnosticDecisionSnapshot,
  type DirectZipDiagnosticsObserver,
  type DirectZipEpochOffsetClass,
} from '../../output/direct-zip/diagnostics'
import type { DirectZipWriterObserver, DirectZipWriterTraceEventV1 } from '../../output/direct-zip/writer'

const INITIAL_DECISIONS: DirectZipDiagnosticDecisionSnapshot = Object.freeze({
  prefixCopy: 'not-evaluated',
  peakSpace: 'not-evaluated',
  permission: 'not-evaluated',
  identity: 'not-evaluated',
  space: 'not-evaluated',
  cleanup: 'not-evaluated',
})

/** One translation boundary keeps transfer decisions aligned with diagnostics' closed vocabulary. */
export class DirectZipTransferDiagnosticsV1 {
  readonly #observer: DirectZipDiagnosticsObserver | undefined
  readonly #operationId: string
  readonly #sessionId: string
  #decisions: DirectZipDiagnosticDecisionSnapshot = INITIAL_DECISIONS

  constructor(input: {
    readonly observer?: DirectZipDiagnosticsObserver
    readonly operationId: string
    readonly sessionId: string
  }) {
    this.#observer = input.observer
    this.#operationId = input.operationId
    this.#sessionId = input.sessionId
  }

  writerObserver(): DirectZipWriterObserver {
    return event => this.#writerEvent(event)
  }

  session(
    milestone: 'session-started' | 'session-paused' | 'session-settled',
    phase: DirectZipWriterTraceEventV1['phase'],
    error?: unknown,
  ): void {
    this.#emit(milestone, phase, 'not-positioned', error)
  }

  #writerEvent(event: DirectZipWriterTraceEventV1): void {
    if (event.kind === 'checkpoint-policy-decided') this.#checkpointDecision(event.decision)
    if (event.kind === 'writer-gated') this.#gateDecision(event.decision)
    this.#emit(event.kind, event.phase, event.offsetClass ?? 'not-positioned', event.error)
  }

  #checkpointDecision(decision: string | undefined): void {
    let prefixCopy: DirectZipDiagnosticDecisionSnapshot['prefixCopy'] = 'not-evaluated'
    let peakSpace: DirectZipDiagnosticDecisionSnapshot['peakSpace'] = 'not-evaluated'
    if (decision === 'admit') {
      prefixCopy = 'admit'
      peakSpace = 'within-budget'
    } else if (decision === 'decline:evidence-unavailable') {
      prefixCopy = 'decline-evidence-unavailable'
      peakSpace = 'evidence-unavailable'
    }
    else if (decision === 'decline:prefix-copy-budget') prefixCopy = 'decline-prefix-copy-budget'
    else if (decision === 'decline:cumulative-copy-budget') {
      prefixCopy = 'decline-cumulative-copy-budget'
      peakSpace = 'within-budget'
    } else if (decision === 'decline:modeled-peak-temporary-budget') {
      prefixCopy = 'admit'
      peakSpace = 'confirmation-required'
    }
    this.#decisions = Object.freeze({ ...this.#decisions, prefixCopy, peakSpace })
  }

  #gateDecision(decision: string | undefined): void {
    let updates: Partial<DirectZipDiagnosticDecisionSnapshot> = {}
    switch (decision) {
      case 'authorization-required': updates = { permission: 'authorization-required' }; break
      case 'destination-space-required':
        updates = {
          space: 'destination-space-required',
          peakSpace: 'destination-space-required',
        }
        break
      case 'target-verification-required': updates = { identity: 'target-verification-required' }; break
      case 'target-deleted': updates = { identity: 'restart-required' }; break
      case 'needs-attention': updates = { identity: 'needs-attention' }; break
    }
    this.#decisions = Object.freeze({ ...this.#decisions, ...updates })
  }

  #emit(
    milestone: Parameters<DirectZipDiagnosticsObserver['observe']>[0]['milestone'],
    checkpointPhase: DirectZipWriterTraceEventV1['phase'],
    epochOffsetClass: DirectZipEpochOffsetClass,
    rawException?: unknown,
  ): void {
    observeDirectZipDiagnostic(this.#observer, () => ({
      operationId: this.#operationId,
      sessionId: this.#sessionId,
      planKind: 'direct-resumable-zip',
      milestone,
      checkpointPhase,
      epochOffsetClass,
      decisions: this.#decisions,
      ...(rawException === undefined ? {} : { rawException }),
    }))
  }
}
