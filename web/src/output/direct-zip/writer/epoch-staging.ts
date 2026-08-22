import {
  DIRECT_ZIP_WRITER_CANDIDATE_VERSION,
  type DirectZipEpochCandidateV1,
  type DirectZipWriterCheckpointV1,
} from './model'
import type {
  DirectZipWriterCutSink,
  DirectZipWriterIdentityPort,
  DirectZipWriterTraceEventV1,
} from './ports'

type DirectZipWriterEmit = (
  kind: DirectZipWriterTraceEventV1['kind'],
  extra?: Omit<DirectZipWriterTraceEventV1, 'kind' | 'operationId' | 'checkpointGeneration' |
    'phase' | 'archiveOffset'>,
) => void

/** Candidate bytes cannot become closeable until this module durably publishes their lineage. */
export class DirectZipEpochStagingCoordinator {
  readonly #cuts: DirectZipWriterCutSink
  readonly #identities: DirectZipWriterIdentityPort
  readonly #emit: DirectZipWriterEmit

  constructor(input: Readonly<{
    cuts: DirectZipWriterCutSink
    identities: DirectZipWriterIdentityPort
    emit: DirectZipWriterEmit
  }>) {
    this.#cuts = input.cuts
    this.#identities = input.identities
    this.#emit = input.emit
  }

  async stage(input: Readonly<{
    kind: DirectZipEpochCandidateV1['kind']
    epochId: string
    predecessor: DirectZipWriterCheckpointV1
    rangeStart: bigint
    stagedEnd: bigint
    contentDigest: Uint8Array
    expectedEpochRoot: Uint8Array
    proposed: DirectZipWriterCheckpointV1
  }>): Promise<DirectZipEpochCandidateV1> {
    const candidate = Object.freeze({
      version: DIRECT_ZIP_WRITER_CANDIDATE_VERSION,
      kind: input.kind,
      candidateId: requireDirectZipWriterIdentity(
        this.#identities.nextCandidateId(),
        'candidate',
      ),
      epochId: input.epochId,
      operationId: input.predecessor.operationId,
      predecessorGeneration: input.predecessor.generation,
      predecessorLength: input.predecessor.committedLength,
      predecessorObservationDigest: Uint8Array.from(
        input.predecessor.targetObservationDigest,
      ),
      rangeStart: input.rangeStart,
      stagedEnd: input.stagedEnd,
      contentDigest: Uint8Array.from(input.contentDigest),
      expectedEpochRoot: Uint8Array.from(input.expectedEpochRoot),
      proposed: input.proposed,
    })
    await this.#cuts.stageCandidate(candidate)
    this.#emit('candidate-staged', {
      candidateId: candidate.candidateId,
      epochId: candidate.epochId,
    })
    return candidate
  }
}

export function requireDirectZipWriterIdentity(value: string, label: string): string {
  if (typeof value !== 'string' || value.length === 0) {
    throw new TypeError(`direct ZIP ${label} ID is invalid`)
  }
  return value
}
