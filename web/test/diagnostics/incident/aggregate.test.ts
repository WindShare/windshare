import { describe, expect, it } from 'vitest'

import {
  DEFAULT_INCIDENT_POLICY,
  createFailureFactAccumulator,
  createIncidentPolicy,
  createIncidentScopeIssuer,
  unclassifiedFailureFact,
  type FailureStage,
} from '../../../src/diagnostics/incident'

describe('failure fact aggregation', () => {
  it('retains one representative, exact duplicate counts, and limit-plus-one overflow', () => {
    const scope = createIncidentScopeIssuer().open('receive')
    const accumulator = createFailureFactAccumulator(
      scope.identity,
      createIncidentPolicy({
        maxContributorBucketsPerIncident: 1,
        maxConsequenceBucketsPerIncident: 1,
      }),
    )
    const triggerFact = fact('content_read')
    const duplicateFact = fact('content_read')
    const overflowFact = fact('output_write')
    const trigger = scope.facts.record(triggerFact, 'contributor')
    const duplicate = scope.facts.record(duplicateFact, 'contributor')
    const overflow = scope.facts.record(overflowFact, 'contributor')
    accumulator.record(trigger)
    accumulator.record(duplicate)
    accumulator.record(overflow)

    const sealed = accumulator.seal(trigger)
    expect(sealed.factCount).toBe(3n)
    expect(sealed.trigger.fact).toBe(triggerFact)
    expect(sealed.contributorBuckets).toEqual([{
      fingerprint: 'unclassified:content_read:terminal',
      count: 1n,
      representative: duplicateFact,
    }])
    expect(sealed.contributorOverflowCount).toBe(1n)
    expect(sealed.consequenceBuckets).toEqual([])
    expect(Object.isFrozen(sealed)).toBe(true)
    expect(Object.isFrozen(sealed.contributorBuckets)).toBe(true)
    expect(accumulator.seal(trigger)).toBe(sealed)
  })

  it('subtracts an overflowed trigger without losing retained contributors', () => {
    const scope = createIncidentScopeIssuer().open('receive')
    const accumulator = createFailureFactAccumulator(
      scope.identity,
      createIncidentPolicy({
        maxContributorBucketsPerIncident: 1,
        maxConsequenceBucketsPerIncident: 1,
      }),
    )
    const retainedFact = fact('content_read')
    const triggerFact = fact('output_write')
    const consequenceFact = fact('cleanup')
    const overflowConsequenceFact = fact('publication')
    const retained = scope.facts.record(retainedFact, 'contributor')
    const trigger = scope.facts.record(triggerFact, 'contributor')
    const consequence = scope.facts.record(consequenceFact, 'consequence')
    const overflowConsequence = scope.facts.record(
      overflowConsequenceFact,
      'consequence',
    )
    for (const ref of [retained, trigger, consequence, overflowConsequence]) {
      accumulator.record(ref)
    }

    const sealed = accumulator.seal(trigger)
    expect(sealed.factCount).toBe(4n)
    expect(sealed.contributorBuckets).toEqual([{
      fingerprint: 'unclassified:content_read:terminal',
      count: 1n,
      representative: retainedFact,
    }])
    expect(sealed.contributorOverflowCount).toBe(0n)
    expect(sealed.consequenceBuckets).toEqual([{
      fingerprint: 'unclassified:cleanup:terminal',
      count: 1n,
      representative: consequenceFact,
    }])
    expect(sealed.consequenceOverflowCount).toBe(1n)
  })

  it('adds every unretained fact occurrence to overflow', () => {
    const scope = createIncidentScopeIssuer().open('browse')
    const accumulator = createFailureFactAccumulator(
      scope.identity,
      createIncidentPolicy({ maxContributorBucketsPerIncident: 1 }),
    )
    const trigger = scope.facts.record(fact('browse'), 'contributor')
    const overflowOne = scope.facts.record(fact('join'), 'contributor')
    const overflowTwo = scope.facts.record(fact('join'), 'contributor')
    for (const ref of [trigger, overflowOne, overflowTwo]) accumulator.record(ref)

    const sealed = accumulator.seal(trigger)
    expect(sealed.contributorBuckets).toEqual([])
    expect(sealed.contributorOverflowCount).toBe(2n)
  })

  it('rejects duplicate, foreign, and consequence trigger references', () => {
    const firstScope = createIncidentScopeIssuer().open('browse')
    const secondScope = createIncidentScopeIssuer().open('join')
    const accumulator = createFailureFactAccumulator(firstScope.identity)
    const contributor = firstScope.facts.record(fact('browse'), 'contributor')
    const consequence = firstScope.facts.record(fact('cleanup'), 'consequence')
    const foreign = secondScope.facts.record(fact('join'), 'contributor')

    accumulator.record(contributor)
    expect(() => accumulator.record(contributor)).toThrow(/already recorded/)
    expect(() => accumulator.record(foreign)).toThrow(/another incident scope/)
    accumulator.record(consequence)
    expect(() => accumulator.seal(consequence)).toThrow(/contributor/)
  })
})

describe('incident capacity policy', () => {
  it('publishes exact defaults and rejects every invalid bound', () => {
    expect(DEFAULT_INCIDENT_POLICY).toEqual({
      maxConsoleReportsPerScope: 25,
      maxIncidentHistoryRecords: 64,
      maxContributorBucketsPerIncident: 16,
      maxConsequenceBucketsPerIncident: 16,
      maxFactRepresentativesPerBucket: 1,
      maxIncidentRecordBytes: 65_536,
      maxRecordListItems: 32,
      maxSafeStringUtf8Bytes: 128,
      maxLateIncidentLinks: 128,
      maxLateIncidentLinkAgeMilliseconds: 600_000,
      incidentSealDeadlineMilliseconds: 2_000,
    })
    expect(Object.isFrozen(DEFAULT_INCIDENT_POLICY)).toBe(true)
    expect(() => createIncidentPolicy({
      maxContributorBucketsPerIncident: 0,
    })).toThrow(RangeError)
    expect(() => createIncidentPolicy({
      maxFactRepresentativesPerBucket: 2 as 1,
    })).toThrow(/exactly one/)
    expect(() => createIncidentPolicy({
      maxIncidentRecordBytes: Number.MAX_SAFE_INTEGER + 1,
    })).toThrow(RangeError)
    expect(() => createIncidentPolicy({
      privateBudget: 1,
    } as unknown as Parameters<typeof createIncidentPolicy>[0])).toThrow(
      /unknown field/,
    )
  })
})

function fact(stage: FailureStage) {
  return unclassifiedFailureFact({
    stage,
    recoveryDisposition: 'terminal',
  })
}
