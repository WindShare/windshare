import {
  mapTracedOwnedOperation,
  type NetworkMatrixTracedOwnedOperation,
} from './owned-operation.ts'
import {
  NetworkMatrixSampleExecutionError,
  type NetworkMatrixSampleExecution,
  type NetworkMatrixSampleExecutionContext,
  type NetworkMatrixSampleExecutor,
} from './runner.ts'
import {
  parseNetworkMatrixAttemptEvidence,
  type NetworkMatrixAttemptEvidence,
} from './attempt-evidence.ts'

export interface ContainedPlaywrightCandidateEvidence {
  readonly processInstanceId: string
  readonly attemptEvidence: NetworkMatrixAttemptEvidence
}

/**
 * Implementations must create a new OS-contained Playwright process for every
 * call. The returned owner must kill the complete containment subtree and wait
 * for reaping. Children return candidate stats only; they never own sample or
 * run result files, so a page cannot overwrite the parent ledger. The broker
 * is the sole boundary that joins private browser stats with remote Pion proof.
 */
export interface NetworkMatrixContainedPlaywrightProcessBroker<TraceChannel> {
  start(
    context: NetworkMatrixSampleExecutionContext,
  ): NetworkMatrixTracedOwnedOperation<ContainedPlaywrightCandidateEvidence, TraceChannel>
}

export class ContainedPlaywrightNetworkMatrixSampleExecutor<TraceChannel>
implements NetworkMatrixSampleExecutor {
  readonly #broker: NetworkMatrixContainedPlaywrightProcessBroker<TraceChannel>

  constructor(broker: NetworkMatrixContainedPlaywrightProcessBroker<TraceChannel>) {
    this.#broker = broker
  }

  execute(
    context: NetworkMatrixSampleExecutionContext,
  ): NetworkMatrixTracedOwnedOperation<NetworkMatrixSampleExecution, TraceChannel> {
    return mapTracedOwnedOperation(this.#broker.start(context), (evidence) => {
      try {
        return Object.freeze({
          processInstanceId: evidence.processInstanceId,
          observation: Object.freeze({
            sampleOutcome: 'observed' as const,
            attemptEvidence: parseNetworkMatrixAttemptEvidence(
              evidence.attemptEvidence,
              context.identity.profileId,
            ),
          }),
        })
      } catch (cause) {
        throw new NetworkMatrixSampleExecutionError(
          'evidence-collection-failed',
          cause instanceof Error ? cause.message : 'attempt evidence collection failed',
        )
      }
    })
  }
}
