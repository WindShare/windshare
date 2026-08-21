import type {
  PersistentOutputCheckpointRecordFact,
  PersistentOutputCheckpointRecordProjection,
  PersistentOutputCapturedException,
  PersistentOutputExceptionProjection,
  PersistentOutputFailureFacts,
  PersistentOutputObservedFact,
  PersistentOutputStageFailureMilestone,
  PersistentOutputStageFailureProjectionV1,
  ProjectedObservedFact,
} from './stage-diagnostic-model'

export function projectPersistentOutputStageFailure(
  milestone: PersistentOutputStageFailureMilestone,
): PersistentOutputStageFailureProjectionV1 {
  return Object.freeze({
    schemaVersion: 1,
    sequence: milestone.sequence,
    stage: milestone.stage,
    correlation: Object.freeze({
      operationId: milestone.correlation.operationId,
      outputSessionId: milestone.correlation.outputSessionId,
      target: milestone.correlation.target,
      artifactId: milestone.correlation.artifactId,
      artifactPath: Object.freeze([...milestone.correlation.artifactPath]),
      ...(milestone.correlation.ownedObjectId === undefined
        ? {}
        : { ownedObjectId: milestone.correlation.ownedObjectId }),
      ...(milestone.correlation.checkpointRecordId === undefined
        ? {}
        : { checkpointRecordId: milestone.correlation.checkpointRecordId }),
      ...(milestone.correlation.checkpointGeneration === undefined
        ? {}
        : { checkpointGeneration: milestone.correlation.checkpointGeneration.toString() }),
    }),
    exception: projectException(milestone.exception),
    facts: projectFailureFacts(milestone.facts),
  })
}

function projectFailureFacts(
  facts: PersistentOutputFailureFacts,
): PersistentOutputStageFailureProjectionV1['facts'] {
  const fsa = facts.fsa === undefined
    ? undefined
    : Object.freeze({
        entry: projectObservedFact(facts.fsa.entry, value => value),
        ...(facts.fsa.committedBytes === undefined
          ? {}
          : { committedBytes: projectObservedFact(
              facts.fsa.committedBytes,
              value => typeof value === 'bigint' ? value.toString() : value,
            ) }),
        permissions: Object.freeze({
          target: facts.fsa.permissions.target,
          read: projectObservedFact(facts.fsa.permissions.read, value => value),
          readwrite: projectObservedFact(facts.fsa.permissions.readwrite, value => value),
        }),
        persistedHandle: projectObservedFact(facts.fsa.persistedHandle, value => value),
        ...(facts.fsa.writer === undefined
          ? {}
          : { writer: facts.fsa.writer.closeFailure === undefined
              ? Object.freeze({ state: facts.fsa.writer.state })
              : Object.freeze({
                  state: facts.fsa.writer.state,
                  closeFailure: projectException(facts.fsa.writer.closeFailure),
                }) }),
      })
  const checkpoint = facts.checkpoint === undefined
    ? undefined
    : Object.freeze({
        candidates: projectObservedFact(
          facts.checkpoint.candidates,
          records => Object.freeze(records.map(projectCheckpointRecord)),
        ),
        committed: projectObservedFact(
          facts.checkpoint.committed,
          records => Object.freeze(records.map(projectCheckpointRecord)),
        ),
      })
  return Object.freeze({
    ...(fsa === undefined ? {} : { fsa }),
    ...(checkpoint === undefined ? {} : { checkpoint }),
    ...(facts.probeFailures === undefined
      ? {}
      : { probeFailures: Object.freeze(facts.probeFailures.map(projectException)) }),
    observation: Object.freeze({
      ...facts.observation,
      unavailableProviders: Object.freeze(facts.observation.unavailableProviders.map(value =>
        value.exception === undefined
          ? Object.freeze({ provider: value.provider, reason: value.reason })
          : Object.freeze({
              provider: value.provider,
              reason: value.reason,
              exception: projectException(value.exception),
            }))),
    }),
  })
}

function projectCheckpointRecord(
  record: PersistentOutputCheckpointRecordFact,
): PersistentOutputCheckpointRecordProjection {
  return Object.freeze({
    recordId: record.recordId,
    checkpointGeneration: record.checkpointGeneration.toString(),
    commitState: record.commitState,
    checksum: record.checksum,
    verifiedEnd: record.verifiedEnd.toString(),
  })
}

function projectObservedFact<Input, Output>(
  fact: PersistentOutputObservedFact<Input>,
  project: (input: Input) => Output,
): ProjectedObservedFact<Output> {
  return fact.status === 'observed'
    ? Object.freeze({ status: 'observed', value: project(fact.value) })
    : Object.freeze({
        status: 'unavailable',
        exception: projectException(fact.exception),
      })
}

function projectException(
  exception: PersistentOutputCapturedException,
): PersistentOutputExceptionProjection {
  return exception.projection
}
