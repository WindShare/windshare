import {
  checkpointMatchesNamespace,
  validateFileCheckpointPage,
  type CheckpointNamespaceBinding,
  type FileCheckpointPage,
  type FileCheckpointScan,
} from '../persistence/journal'
import {
  FILE_CHECKPOINT_COMMIT_QUARANTINED,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  validateFileCheckpoint,
  type FileCheckpointV2,
} from '../persistence/checkpoint'

export type FileCheckpointCandidateObservation =
  | Readonly<{ kind: 'verified'; committed: FileCheckpointV2 }>
  | Readonly<{ kind: 'quarantined'; checkpoint: FileCheckpointV2 }>
  | Readonly<{ kind: 'ownership-unknown' }>

export interface FileCheckpointRecoveryRepository {
  readonly binding: CheckpointNamespaceBinding
  scanCandidates(scan: FileCheckpointScan): Promise<FileCheckpointPage>
  readCommitted(recordId: string): Promise<FileCheckpointV2 | undefined>
  resolveCandidate(
    candidate: FileCheckpointV2,
    observation: Exclude<FileCheckpointCandidateObservation, { kind: 'ownership-unknown' }>,
  ): Promise<void>
}

export interface FileCheckpointCandidateProbe {
  observe(
    candidate: FileCheckpointV2,
    committed: FileCheckpointV2 | undefined,
  ): Promise<FileCheckpointCandidateObservation>
}

export interface FileCheckpointRecoveryReport {
  readonly resolved: number
  readonly unknownRecordIds: readonly string[]
}

/**
 * Candidate resolution is idempotent: the repository commits or quarantines a
 * candidate atomically. A crash can replay the probe, but cannot invent range truth.
 */
export async function recoverFileCheckpointCandidates(
  repository: FileCheckpointRecoveryRepository,
  probe: FileCheckpointCandidateProbe,
): Promise<FileCheckpointRecoveryReport> {
  let cursor: string | undefined
  let resolved = 0
  const unknownRecordIds: string[] = []

  do {
    const scan: FileCheckpointScan = {
      direction: 'ascending',
      ...(cursor === undefined ? {} : { cursor }),
    }
    const page = validateFileCheckpointPage(
      await repository.scanCandidates(scan),
      scan,
      repository.binding,
    )
    for (const candidate of page.records) {
      const candidateResolved = await recoverCandidate(repository, probe, candidate)
      if (candidateResolved) resolved += 1
      else unknownRecordIds.push(candidate.recordId)
    }
    cursor = page.nextCursor
  } while (cursor !== undefined)

  return Object.freeze({
    resolved,
    unknownRecordIds: Object.freeze(unknownRecordIds),
  })
}

async function recoverCandidate(
  repository: FileCheckpointRecoveryRepository,
  probe: FileCheckpointCandidateProbe,
  candidate: FileCheckpointV2,
): Promise<boolean> {
  if (!checkpointMatchesNamespace(candidate, repository.binding)) {
    throw new TypeError('candidate checkpoint escaped its recovery namespace')
  }
  const committed = await repository.readCommitted(candidate.recordId)
  if (committed !== undefined &&
      !checkpointMatchesNamespace(committed, repository.binding)) {
    throw new TypeError('committed checkpoint escaped its recovery namespace')
  }
  const observation = await probe.observe(candidate, committed)
  if (observation.kind === 'ownership-unknown') {
    // The aggregate owns the frozen receive.operation.recovery trace because only it
    // has enough operation context to report a contract-complete decision.
    return false
  }
  assertResolvedCandidateIdentity(candidate, observation, repository.binding)
  await repository.resolveCandidate(candidate, observation)
  return true
}

function assertResolvedCandidateIdentity(
  candidate: FileCheckpointV2,
  observation: Exclude<FileCheckpointCandidateObservation, { kind: 'ownership-unknown' }>,
  binding: CheckpointNamespaceBinding,
): void {
  const resolved = observation.kind === 'verified'
    ? observation.committed
    : observation.checkpoint
  validateFileCheckpoint(resolved)
  const expectedCommitState = observation.kind === 'verified'
    ? FILE_CHECKPOINT_COMMIT_VERIFIED
    : FILE_CHECKPOINT_COMMIT_QUARANTINED
  if (resolved.recordId !== candidate.recordId ||
      resolved.commitState !== expectedCommitState ||
      !checkpointMatchesNamespace(resolved, binding)) {
    throw new TypeError('checkpoint probe returned a foreign resolved record')
  }
}
