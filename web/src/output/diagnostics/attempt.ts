import type {
  FailureFact,
  FailureFactRef,
  FailureFactRelation,
  FailureFactSink,
  IncidentScopeHandle,
  RecoveryDisposition,
} from '../../diagnostics/incident'
import {
  createOutputFailureSink,
  type OutputFailureSinks,
  type OutputFailureStage,
} from './facts'
import { createLocalOutputFailureAttemptAuthority } from './local-output-failure'
import { createLateLocalOutputFailureAttemptAuthority } from './local-output-failure'

const OUTPUT_ATTEMPT_RELATION: Readonly<Record<OutputFailureStage, FailureFactRelation>> =
  Object.freeze({
    output_reservation: 'contributor',
    output_write: 'contributor',
    output_commit: 'contributor',
    checkpoint: 'contributor',
    settlement: 'contributor',
    publication: 'contributor',
    continuation: 'contributor',
    reopen: 'contributor',
    cleanup: 'consequence',
  })

const OUTPUT_ATTEMPT_RECOVERY: Readonly<Record<OutputFailureStage, RecoveryDisposition>> =
  Object.freeze({
    output_reservation: 'none',
    output_write: 'none',
    output_commit: 'none',
    checkpoint: 'none',
    settlement: 'none',
    publication: 'none',
    continuation: 'none',
    reopen: 'none',
    cleanup: 'needs_attention',
  })

export interface LateOutputFailureConsequenceCapability {
  readonly sinks: OutputFailureSinks
  revoke(): void
}

/**
 * A sealed incident rejects new causes but may still receive its owned cleanup result.
 * This deliberately exposes no contributor stages and requires explicit revocation.
 */
export function createLateOutputCleanupCapability(
  scope: IncidentScopeHandle | undefined,
): LateOutputFailureConsequenceCapability {
  const facts = scope?.facts
  let active = facts !== undefined
  const localOutputFailures = createLateLocalOutputFailureAttemptAuthority(scope)
  const cleanup = createOutputFailureSink({
    facts: Object.freeze({
      record: (fact: FailureFact, relation: FailureFactRelation) => {
        if (!active || facts === undefined) {
          throw new DOMException('Late output consequence capability is revoked', 'InvalidStateError')
        }
        if (relation !== 'consequence') {
          throw new TypeError('Late output capability accepts cleanup consequences only')
        }
        return facts.record(fact, relation)
      },
    }),
    stage: 'cleanup',
    relation: 'consequence',
    recoveryDisposition: 'needs_attention',
  })
  return Object.freeze({
    sinks: Object.freeze({ attempt: localOutputFailures.source, cleanup }),
    revoke: () => {
      active = false
      localOutputFailures.revoke()
    },
  })
}

export interface AttemptOutputFailureCapability {
  readonly sinks: OutputFailureSinks
  firstContributor(): FailureFactRef | undefined
  revoke(): void
}

/**
 * The presentation attempt owns this revocable capability. Native output code can
 * classify at its boundary, but cannot outlive or redirect the owning incident scope.
 */
export function createAttemptOutputFailureCapability(
  scope: IncidentScopeHandle | undefined,
): AttemptOutputFailureCapability {
  const facts = scope?.facts
  let active = facts !== undefined
  let firstContributor: FailureFactRef | undefined
  const localOutputFailures = createLocalOutputFailureAttemptAuthority(scope)
  const guardedFacts: FailureFactSink = Object.freeze({
    record: (fact: FailureFact, relation: FailureFactRelation) => {
      if (!active || facts === undefined) {
        throw new DOMException('Output failure capability is revoked', 'InvalidStateError')
      }
      const ref = facts.record(fact, relation)
      if (relation === 'contributor') firstContributor ??= ref
      return ref
    },
  })
  const sink = <Stage extends OutputFailureStage>(stage: Stage) =>
    createOutputFailureSink({
      facts: guardedFacts,
      stage,
      relation: OUTPUT_ATTEMPT_RELATION[stage],
      recoveryDisposition: OUTPUT_ATTEMPT_RECOVERY[stage],
    })
  const sinks: OutputFailureSinks = Object.freeze({
    attempt: localOutputFailures.source,
    outputReservation: sink('output_reservation'),
    outputWrite: sink('output_write'),
    outputCommit: sink('output_commit'),
    checkpoint: sink('checkpoint'),
    settlement: sink('settlement'),
    publication: sink('publication'),
    continuation: sink('continuation'),
    reopen: sink('reopen'),
    cleanup: sink('cleanup'),
  })
  return Object.freeze({
    sinks,
    firstContributor: () => firstContributor,
    revoke: () => {
      active = false
      localOutputFailures.revoke()
    },
  })
}
