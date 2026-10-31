import type { FailureFact } from './fact'
import {
  fingerprintFailureFact,
  type FailureFingerprint,
} from './fingerprint'
import {
  INCIDENT_SCOPE_KINDS,
  failureFactForRef,
  failureFactRelationForRef,
  type FailureFactRef,
  type FailureFactRelation,
  type IncidentScopeIdentity,
} from './scope'
import {
  DEFAULT_INCIDENT_POLICY,
  createIncidentPolicy,
  type IncidentPolicy,
} from './policy'

export const MAX_DIAGNOSTIC_CAUSES_PER_CLASSIFICATION = 4

export interface FailureFactBucket {
  readonly causeSignature?: string
  readonly fingerprint: FailureFingerprint
  readonly count: bigint
  readonly representative: FailureFact
}

export interface SealedFailureFacts {
  readonly scope: IncidentScopeIdentity
  readonly trigger: Readonly<{
    ref: FailureFactRef
    fact: FailureFact
  }>
  readonly factCount: bigint
  readonly contributorBuckets: readonly FailureFactBucket[]
  readonly contributorOverflowCount: bigint
  readonly consequenceBuckets: readonly FailureFactBucket[]
  readonly consequenceOverflowCount: bigint
}

export interface FailureFactAccumulator {
  readonly factCount: bigint
  record(ref: FailureFactRef): void
  seal(trigger: FailureFactRef): SealedFailureFacts
  snapshotConsequences(): AggregatedConsequences
}

export interface AggregatedConsequences {
  readonly factCount: bigint
  readonly buckets: readonly FailureFactBucket[]
  readonly overflowCount: bigint
}

interface MutableBucket {
  readonly causeSignature?: string
  readonly fingerprint: FailureFingerprint
  count: bigint
  representative: FailureFactRef
  fallbackRepresentative?: FailureFactRef
}

interface RefPlacement {
  readonly relation: FailureFactRelation
  readonly key: string
  readonly retained: boolean
  readonly fingerprint: FailureFingerprint
}

class ExactFailureFactAccumulator implements FailureFactAccumulator {
  readonly #contributorBuckets = new Map<string, MutableBucket>()
  readonly #consequenceBuckets = new Map<string, MutableBucket>()
  readonly #placements = new WeakMap<FailureFactRef, RefPlacement>()
  readonly #scope: IncidentScopeIdentity
  readonly #policy: IncidentPolicy
  #factCount = 0n
  #contributorOverflowCount = 0n
  #consequenceOverflowCount = 0n
  #sealed?: SealedFailureFacts

  constructor(
    scope: IncidentScopeIdentity,
    policy: IncidentPolicy,
  ) {
    this.#scope = scope
    this.#policy = policy
  }

  get factCount(): bigint { return this.#factCount }

  record(ref: FailureFactRef): void {
    if (this.#sealed !== undefined) {
      throw new Error('Sealed failure fact accumulators reject new facts')
    }
    this.#requireScope(ref)
    if (this.#placements.has(ref)) {
      throw new TypeError('Failure fact reference was already recorded')
    }

    const fact = failureFactForRef(ref)
    const relation = failureFactRelationForRef(ref)
    const fingerprint = fingerprintFailureFact(fact)
    const buckets = relation === 'contributor'
      ? this.#contributorBuckets
      : this.#consequenceBuckets
    const capacity = relation === 'contributor'
      ? this.#policy.maxContributorBucketsPerIncident
      : this.#policy.maxConsequenceBucketsPerIncident
    const causeSignature = fact.evidence?.signature
    const key = fingerprint + ':' + (causeSignature ?? '-')
    const existing = buckets.get(key)
    if (existing !== undefined) {
      existing.count += 1n
      // A single private fallback lets a nominated primary become the trigger
      // without losing the one public representative promised for its bucket.
      if (relation === 'contributor') existing.fallbackRepresentative ??= ref
      this.#placements.set(ref, { relation, key, retained: true, fingerprint })
    } else if (buckets.size < capacity &&
        [...buckets.values()].filter(bucket => bucket.fingerprint === fingerprint).length < MAX_DIAGNOSTIC_CAUSES_PER_CLASSIFICATION) {
      buckets.set(key, {
        ...(causeSignature === undefined ? {} : { causeSignature }),
        fingerprint,
        count: 1n,
        representative: ref,
      })
      this.#placements.set(ref, { relation, key, retained: true, fingerprint })
    } else {
      if (relation === 'contributor') {
        this.#contributorOverflowCount += 1n
      } else {
        this.#consequenceOverflowCount += 1n
      }
      this.#placements.set(ref, { relation, key, retained: false, fingerprint })
    }
    this.#factCount += 1n
  }

  snapshotConsequences(): AggregatedConsequences {
    const buckets = sealBuckets(this.#consequenceBuckets)
    return Object.freeze({
      factCount: buckets.reduce((count, bucket) => count + bucket.count, this.#consequenceOverflowCount),
      buckets,
      overflowCount: this.#consequenceOverflowCount,
    })
  }

  seal(trigger: FailureFactRef): SealedFailureFacts {
    if (this.#sealed !== undefined) {
      if (this.#sealed.trigger.ref !== trigger) {
        throw new Error('Failure facts were already sealed with another trigger')
      }
      return this.#sealed
    }
    this.#requireScope(trigger)
    const placement = this.#placements.get(trigger)
    if (placement === undefined) {
      throw new TypeError('Incident trigger was not recorded')
    }
    if (placement.relation !== 'contributor') {
      throw new TypeError('Incident trigger must be a contributor')
    }

    this.#removeTriggerFromContributors(trigger, placement)

    this.#sealed = Object.freeze({
      scope: this.#scope,
      trigger: Object.freeze({
        ref: trigger,
        fact: failureFactForRef(trigger),
      }),
      factCount: this.#factCount,
      contributorBuckets: sealBuckets(this.#contributorBuckets),
      contributorOverflowCount: this.#contributorOverflowCount,
      consequenceBuckets: sealBuckets(this.#consequenceBuckets),
      consequenceOverflowCount: this.#consequenceOverflowCount,
    })
    return this.#sealed
  }

  #removeTriggerFromContributors(
    trigger: FailureFactRef,
    placement: RefPlacement,
  ): void {
    if (!placement.retained) {
      if (this.#contributorOverflowCount === 0n) {
        throw new Error('Contributor overflow accounting invariant failed')
      }
      this.#contributorOverflowCount -= 1n
      return
    }

    const bucket = this.#contributorBuckets.get(placement.key)
    if (bucket === undefined || bucket.count === 0n) {
      throw new Error('Contributor accounting invariant failed')
    }
    bucket.count -= 1n
    if (bucket.representative !== trigger) return
    if (bucket.count === 0n) {
      this.#contributorBuckets.delete(placement.key)
      return
    }
    if (bucket.fallbackRepresentative === undefined) {
      throw new Error('Contributor representative invariant failed')
    }
    bucket.representative = bucket.fallbackRepresentative
  }

  #requireScope(ref: FailureFactRef): void {
    if (ref.scope !== this.#scope) {
      throw new TypeError('Failure fact reference belongs to another incident scope')
    }
  }
}

export function createFailureFactAccumulator(
  scope: IncidentScopeIdentity,
  policy: IncidentPolicy = DEFAULT_INCIDENT_POLICY,
): FailureFactAccumulator {
  if (
    typeof scope !== 'object' ||
    scope === null ||
    !isMember(INCIDENT_SCOPE_KINDS, scope.scopeKind) ||
    typeof scope.scopeSequence !== 'bigint' ||
    scope.scopeSequence <= 0n
  ) {
    throw new TypeError('Incident scope identity is invalid')
  }
  return new ExactFailureFactAccumulator(
    scope,
    policy === DEFAULT_INCIDENT_POLICY
      ? DEFAULT_INCIDENT_POLICY
      : createIncidentPolicy(policy),
  )
}

function sealBuckets(
  buckets: ReadonlyMap<string, MutableBucket>,
): readonly FailureFactBucket[] {
  return Object.freeze(
    [...buckets.values()]
      .filter((bucket) => bucket.count > 0n)
      .sort((left, right) =>
        compareFingerprints(left.fingerprint, right.fingerprint),
      )
      .map((bucket) => Object.freeze({
        ...(bucket.causeSignature === undefined ? {} : { causeSignature: bucket.causeSignature }),
        fingerprint: bucket.fingerprint,
        count: bucket.count,
        representative: failureFactForRef(bucket.representative),
      })),
  )
}

function compareFingerprints(
  left: FailureFingerprint,
  right: FailureFingerprint,
): number {
  if (left < right) return -1
  if (left > right) return 1
  return 0
}

function isMember<const Value extends string>(
  values: readonly Value[],
  value: unknown,
): value is Value {
  return typeof value === 'string' && values.includes(value as Value)
}
