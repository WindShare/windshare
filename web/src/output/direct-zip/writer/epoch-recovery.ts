import {
  chainDirectZipEpochDigestV1,
  directZipEpochGenesisRoot,
} from '../format'
import { equalDirectZipBytes } from '../format/canonical'
import {
  checkpointWithCompletion,
  checkpointWithObservation,
  snapshotCheckpoint,
} from './checkpoint-state'
import type { DirectZipCompletionValidationInput } from './closing'
import {
  DirectZipWriterGateError,
  gateFromPredecessor,
  gateFromRecovery,
  gateFromTruncate,
  recoveryDecisionLabel,
} from './gates'
import type {
  DirectZipCompletionProofV1,
  DirectZipEpochCandidateV1,
  DirectZipWriterCheckpointV1,
} from './model'
import type {
  DirectZipCandidateObservationV1,
  DirectZipCloseAttemptV1,
  DirectZipTargetVerificationPort,
  DirectZipWriterCutSink,
  DirectZipWriterPageSink,
  DirectZipWriterTraceEventV1,
} from './ports'
import {
  decideDirectZipCandidateRecoveryV1,
  type DirectZipCandidateRecoveryDecisionV1,
} from './recovery'

export type DirectZipCandidateResolutionV1 =
  | Readonly<{ kind: 'promoted'; checkpoint: DirectZipWriterCheckpointV1 }>
  | Readonly<{ kind: 'replay'; checkpoint: DirectZipWriterCheckpointV1 }>

type DirectZipWriterEmit = (
  kind: DirectZipWriterTraceEventV1['kind'],
  extra?: Omit<DirectZipWriterTraceEventV1, 'kind' | 'operationId' | 'checkpointGeneration' |
    'phase' | 'archiveOffset'>,
) => void

/** Candidate recovery owns every slow-read decision; callers only adopt its authoritative result. */
export class DirectZipEpochRecoveryCoordinator {
  readonly #pages: DirectZipWriterPageSink
  readonly #cuts: DirectZipWriterCutSink
  readonly #target: DirectZipTargetVerificationPort
  readonly #emit: DirectZipWriterEmit
  readonly #adoptCheckpoint: (checkpoint: DirectZipWriterCheckpointV1) => Promise<void>
  readonly #restoreCommittedCheckpoint: () => Promise<void>
  readonly #validateCompletion: (
    checkpoint: DirectZipWriterCheckpointV1,
    input: DirectZipCompletionValidationInput,
  ) => Promise<DirectZipCompletionProofV1>

  constructor(input: Readonly<{
    pages: DirectZipWriterPageSink
    cuts: DirectZipWriterCutSink
    target: DirectZipTargetVerificationPort
    emit: DirectZipWriterEmit
    adoptCheckpoint: (checkpoint: DirectZipWriterCheckpointV1) => Promise<void>
    restoreCommittedCheckpoint: () => Promise<void>
    validateCompletion: (
      checkpoint: DirectZipWriterCheckpointV1,
      completion: DirectZipCompletionValidationInput,
    ) => Promise<DirectZipCompletionProofV1>
  }>) {
    this.#pages = input.pages
    this.#cuts = input.cuts
    this.#target = input.target
    this.#emit = input.emit
    this.#adoptCheckpoint = input.adoptCheckpoint
    this.#restoreCommittedCheckpoint = input.restoreCommittedCheckpoint
    this.#validateCompletion = input.validateCompletion
  }

  async verifyPredecessor(checkpoint: DirectZipWriterCheckpointV1): Promise<void> {
    const result = await this.#target.verifyPredecessor(checkpoint)
    if (result.kind === 'accepted-fast') {
      this.#emit('predecessor-verified', { decision: 'accepted-fast' })
      return
    }
    if (result.kind === 'digest-readback-required') {
      if (!await this.#readbackCommittedEpochs(checkpoint)) {
        throw new DirectZipWriterGateError(
          'target-verification-required',
          'committed epoch digest readback changed',
        )
      }
      this.#emit('predecessor-verified', { decision: 'digest-readback' })
      return
    }
    throw gateFromPredecessor(result.kind)
  }

  async resolveCandidate(
    candidate: DirectZipEpochCandidateV1,
    predecessor: DirectZipWriterCheckpointV1,
    closeAttempt?: DirectZipCloseAttemptV1,
    completionInput?: DirectZipCompletionValidationInput,
  ): Promise<DirectZipCandidateResolutionV1> {
    let observation = closeAttempt === undefined
      ? await this.#target.observeCandidate(candidate)
      : await this.#target.observeCandidate(candidate, closeAttempt)
    if ((closeAttempt === undefined || closeAttempt.kind === 'threw') &&
        observation.candidateIntegrity === 'writer-bounded-proof') {
      observation = Object.freeze({ ...observation, candidateIntegrity: 'not-read' })
    }
    for (;;) {
      const decision = decideDirectZipCandidateRecoveryV1(observation)
      this.#emit('candidate-resolved', {
        candidateId: candidate.candidateId,
        epochId: candidate.epochId,
        decision: recoveryDecisionLabel(decision),
      })
      if (decision.kind === 'verify-candidate-range') {
        const digest = await this.#target.digestRange(candidate.rangeStart, candidate.stagedEnd)
        observation = Object.freeze({
          ...observation,
          candidateIntegrity: equalDirectZipBytes(digest, candidate.contentDigest)
            ? 'verified'
            : 'mismatch',
        })
        continue
      }
      if (decision.kind === 'verify-predecessor-epochs') {
        observation = Object.freeze({
          ...observation,
          predecessorIntegrity: await this.#readbackCommittedEpochs(predecessor)
            ? 'verified'
            : 'mismatch',
        })
        continue
      }
      return this.#applyDecision(candidate, predecessor, observation, decision, completionInput)
    }
  }

  async #applyDecision(
    candidate: DirectZipEpochCandidateV1,
    predecessor: DirectZipWriterCheckpointV1,
    observation: DirectZipCandidateObservationV1,
    decision: DirectZipCandidateRecoveryDecisionV1,
    completionInput?: DirectZipCompletionValidationInput,
  ): Promise<DirectZipCandidateResolutionV1> {
    if (decision.kind === 'promote-candidate') {
      if (observation.observationDigest === undefined) {
        throw new DirectZipWriterGateError(
          'target-verification-required',
          'candidate observation digest is absent',
        )
      }
      let checkpoint = checkpointWithObservation(candidate.proposed, observation.observationDigest)
      let completion: DirectZipCompletionProofV1 | undefined
      if (completionInput !== undefined) {
        checkpoint = checkpointWithCompletion(checkpoint, Object.freeze({
          exactArchiveBytes: checkpoint.committedLength,
          preClosingEpochRoot: Uint8Array.from(completionInput.seal.preClosingEpochRoot),
        }))
        completion = await this.#validateCompletion(checkpoint, completionInput)
      }
      await this.#cuts.promoteCandidate({
        candidate,
        checkpoint,
        ...(completion === undefined ? {} : { completion }),
      })
      await this.#adoptCheckpoint(checkpoint)
      this.#emit('checkpoint-promoted', {
        candidateId: candidate.candidateId,
        epochId: candidate.epochId,
      })
      return Object.freeze({ kind: 'promoted', checkpoint: snapshotCheckpoint(checkpoint) })
    }
    if (decision.kind === 'replay-predecessor') {
      await this.#cuts.retireCandidate({
        candidate,
        disposition: 'replay-predecessor',
        checkpoint: predecessor,
      })
      await this.#restoreCommittedCheckpoint()
      return Object.freeze({ kind: 'replay', checkpoint: snapshotCheckpoint(predecessor) })
    }
    if (decision.kind === 'truncate-and-replay') {
      const result = await this.#target.truncateToPredecessor(predecessor, candidate)
      if (result.kind !== 'truncated') throw gateFromTruncate(result.kind)
      const checkpoint = checkpointWithObservation(Object.freeze({
        ...predecessor,
        generation: predecessor.generation + 1n,
      }), result.observationDigest)
      await this.#cuts.retireCandidate({
        candidate,
        disposition: 'truncate-and-replay',
        checkpoint,
      })
      await this.#adoptCheckpoint(checkpoint)
      return Object.freeze({ kind: 'replay', checkpoint: snapshotCheckpoint(checkpoint) })
    }
    throw gateFromRecovery(decision)
  }

  async #readbackCommittedEpochs(checkpoint: DirectZipWriterCheckpointV1): Promise<boolean> {
    let expectedStart = 0n
    let expectedPredecessorRoot = directZipEpochGenesisRoot()
    let sawProof = false
    for await (const proof of this.#pages.committedEpochProofs(checkpoint)) {
      if (proof.start !== expectedStart || proof.end <= proof.start ||
          proof.end > checkpoint.committedLength ||
          !equalDirectZipBytes(proof.predecessorRoot, expectedPredecessorRoot)) return false
      const actual = await this.#target.digestRange(proof.start, proof.end)
      if (!equalDirectZipBytes(actual, proof.contentDigest)) return false
      const epochRoot = chainDirectZipEpochDigestV1({
        predecessorRoot: proof.predecessorRoot,
        start: proof.start,
        end: proof.end,
        contentDigest: proof.contentDigest,
      })
      if (!equalDirectZipBytes(epochRoot, proof.epochRoot)) return false
      expectedStart = proof.end
      expectedPredecessorRoot = Uint8Array.from(proof.epochRoot)
      sawProof = true
    }
    return sawProof && expectedStart === checkpoint.committedLength &&
      equalDirectZipBytes(expectedPredecessorRoot, checkpoint.epochRoot)
  }
}
