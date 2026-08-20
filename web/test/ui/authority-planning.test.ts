import { describe, expect, it } from 'vitest'

import {
  ArtifactPlanningContractError,
  materializationRouteIdentity,
  offerArtifacts,
  type ArtifactChoiceReconcileOutcome,
  type ArtifactOffers,
} from '../../src/output/planning'
import { createSelectionSpec } from '../../src/transfer/intent'
import type { SelectionProjectionState } from '../../src/transfer/projection'
import {
  AuthorityPlanningPipeline,
  planningSubject,
} from '../../src/ui/controller/authority-planning'
import type { V2ActiveProjection } from '../../src/ui/controller/projection-observation'
import type { V2JoinedBrowserShare } from '../../src/ui/v2-gateway'
import {
  COMPLETE_DISCOVERY,
  projection,
  singleFileProof,
} from '../output/planning/fixture'
import {
  FakeJoinedShare,
  MANAGED_ENVIRONMENT,
  deferred,
  identityText,
  turns,
  waitFor,
  type Deferred,
} from './v2-receiver-orchestration-fixture'

describe('authority planning pipeline', () => {
  it('publishes only the newest planning request', async () => {
    const fixture = await planningFixture(false)
    const planning: Deferred<ArtifactOffers>[] = []
    const applied: number[] = []
    let current: V2ActiveProjection | undefined
    const pipeline = new AuthorityPlanningPipeline({
      currentProjection: () => current,
      offersApplied: request => { applied.push(request.revision) },
      planningFailed: error => { throw error },
      reconciliationApplied: () => undefined,
      reconciliationFailed: (_subject, error) => { throw error },
      reconciliationEvidenceAdvanced: () => undefined,
      planner: () => {
        const gate = deferred<ArtifactOffers>()
        planning.push(gate)
        return gate.promise
      },
    })

    current = fixture.active[0]
    pipeline.observe(fixture.active[0]!)
    current = fixture.active[1]
    pipeline.observe(fixture.active[1]!)
    planning[1]!.resolve(fixture.offers[1]!)
    planning[0]!.resolve(fixture.offers[0]!)
    await turns()

    expect(applied).toEqual([2])
    expect(pipeline.latestOffers?.request.active).toBe(fixture.active[1])
  })

  it('applies only the newest reconciliation request', async () => {
    const fixture = await planningFixture(false)
    const reconciliation: Deferred<ArtifactChoiceReconcileOutcome>[] = []
    const applied: number[] = []
    let current: V2ActiveProjection | undefined
    const pipeline = new AuthorityPlanningPipeline({
      currentProjection: () => current,
      offersApplied: () => undefined,
      planningFailed: error => { throw error },
      reconciliationApplied: (_subject, request) => { applied.push(request.revision) },
      reconciliationFailed: (_subject, error) => { throw error },
      reconciliationEvidenceAdvanced: () => undefined,
      reconciler: () => {
        const gate = deferred<ArtifactChoiceReconcileOutcome>()
        reconciliation.push(gate)
        return gate.promise
      },
    })

    current = fixture.active[0]
    pipeline.observe(fixture.active[0]!, fixture.subject)
    current = fixture.active[1]
    pipeline.observe(fixture.active[1]!, fixture.subject)
    reconciliation[1]!.resolve(observation(fixture.active[1]!, null))
    reconciliation[0]!.resolve(observation(fixture.active[0]!, null))
    await turns()

    expect(applied).toEqual([2])
  })

  it.each(['newest-first', 'oldest-first'] as const)(
    'validates every same-epoch artifact observation when completion is %s',
    async order => {
      const fixture = await planningFixture(true)
      const reconciliation: Deferred<ArtifactChoiceReconcileOutcome>[] = []
      const failures: unknown[] = []
      let current: V2ActiveProjection | undefined
      const pipeline = new AuthorityPlanningPipeline({
        currentProjection: () => current,
        offersApplied: () => undefined,
        planningFailed: error => { throw error },
        reconciliationApplied: () => undefined,
        reconciliationFailed: (_subject, error) => { failures.push(error) },
        reconciliationEvidenceAdvanced: () => undefined,
        reconciler: () => {
          const gate = deferred<ArtifactChoiceReconcileOutcome>()
          reconciliation.push(gate)
          return gate.promise
        },
      })
      const subject = fixture.subject

      current = fixture.active[0]
      pipeline.observe(fixture.active[0]!, subject)
      reconciliation[0]!.resolve(observation(fixture.active[0]!, null))
      await turns()
      current = fixture.active[1]
      pipeline.observe(fixture.active[1]!, subject)
      current = fixture.active[2]
      pipeline.observe(fixture.active[2]!, subject)
      await waitFor(() => reconciliation.length === 3)
      const older = observation(fixture.active[1]!, identityText(81, 32))
      const newest = observation(fixture.active[2]!, identityText(82, 32))
      if (order === 'newest-first') {
        reconciliation[2]!.resolve(newest)
        reconciliation[1]!.resolve(older)
      } else {
        reconciliation[1]!.resolve(older)
        reconciliation[2]!.resolve(newest)
      }
      await waitFor(() => failures.length === 1)

      expect(failures[0]).toMatchObject<Partial<ArtifactPlanningContractError>>({
        code: 'same-epoch-resolved-artifact-digest-changed',
      })
    },
  )

  it.each(['newest-first', 'oldest-first'] as const)(
    'rejects resolved-to-null evidence when completion is %s',
    async order => {
      const fixture = await planningFixture(true)
      const reconciliation: Deferred<ArtifactChoiceReconcileOutcome>[] = []
      const failures: unknown[] = []
      let current: V2ActiveProjection | undefined
      const pipeline = new AuthorityPlanningPipeline({
        currentProjection: () => current,
        offersApplied: () => undefined,
        planningFailed: error => { throw error },
        reconciliationApplied: () => undefined,
        reconciliationFailed: (_subject, error) => { failures.push(error) },
        reconciliationEvidenceAdvanced: () => undefined,
        reconciler: () => {
          const gate = deferred<ArtifactChoiceReconcileOutcome>()
          reconciliation.push(gate)
          return gate.promise
        },
      })

      current = fixture.active[0]
      pipeline.observe(fixture.active[0]!, fixture.subject)
      reconciliation[0]!.resolve(observation(fixture.active[0]!, null))
      await turns()
      current = fixture.active[1]
      pipeline.observe(fixture.active[1]!, fixture.subject)
      current = fixture.active[2]
      pipeline.observe(fixture.active[2]!, fixture.subject)
      await waitFor(() => reconciliation.length === 3)
      const resolved = observation(fixture.active[1]!, identityText(83, 32))
      const unresolved = observation(fixture.active[2]!, null)
      if (order === 'newest-first') {
        reconciliation[2]!.resolve(unresolved)
        reconciliation[1]!.resolve(resolved)
      } else {
        reconciliation[1]!.resolve(resolved)
        reconciliation[2]!.resolve(unresolved)
      }
      await waitFor(() => failures.length === 1)

      expect(failures[0]).toMatchObject<Partial<ArtifactPlanningContractError>>({
        code: 'same-epoch-resolved-artifact-digest-changed',
      })
    },
  )

  it('rejects a selection digest mutation within one replacement epoch', async () => {
    const fixture = await planningFixture(true)
    const reconciliation: Deferred<ArtifactChoiceReconcileOutcome>[] = []
    const failures: unknown[] = []
    let current: V2ActiveProjection | undefined
    const pipeline = new AuthorityPlanningPipeline({
      currentProjection: () => current,
      offersApplied: () => undefined,
      planningFailed: error => { throw error },
      reconciliationApplied: () => undefined,
      reconciliationFailed: (_subject, error) => { failures.push(error) },
      reconciliationEvidenceAdvanced: () => undefined,
      reconciler: () => {
        const gate = deferred<ArtifactChoiceReconcileOutcome>()
        reconciliation.push(gate)
        return gate.promise
      },
    })

    current = fixture.active[0]
    pipeline.observe(fixture.active[0]!, fixture.subject)
    reconciliation[0]!.resolve(observation(fixture.active[0]!, null))
    await turns()
    current = fixture.active[1]
    pipeline.observe(fixture.active[1]!, fixture.subject)
    const changed = observation(fixture.active[1]!, null)
    reconciliation[1]!.resolve(Object.freeze({
      ...changed,
      observation: Object.freeze({
        ...changed.observation,
        selectionDigest: identityText(90, 32),
      }),
    }))
    await waitFor(() => failures.length === 1)

    expect(failures[0]).toMatchObject<Partial<ArtifactPlanningContractError>>({
      code: 'same-epoch-selection-digest-changed',
    })
  })
})

async function planningFixture(sameEpoch: boolean) {
  const joined = new FakeJoinedShare(true)
  const selection = await createSelectionSpec({
    shareInstance: joined.descriptor.shareInstanceId,
    syntheticRoot: joined.descriptor.syntheticRootId,
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const active: V2ActiveProjection[] = []
  const offers: ArtifactOffers[] = []
  for (let index = 0; index < 3; index += 1) {
    const projected = projection(
      selection,
      singleFileProof(),
      128n,
      sameEpoch ? 1n : BigInt(index + 1),
    )
    const state: SelectionProjectionState = Object.freeze({
      projection: projected,
      discovery: COMPLETE_DISCOVERY,
    })
    active.push({
      revision: index + 1,
      joined: joined as unknown as V2JoinedBrowserShare,
      selection,
      frozenSelection: joined.selection.snapshot(),
      epoch: projected.epoch,
      controller: new AbortController(),
      protocolSessionId: identityText(40 + index),
      state,
      environment: MANAGED_ENVIRONMENT,
    })
    offers.push(await offerArtifacts(projected, COMPLETE_DISCOVERY, MANAGED_ENVIRONMENT))
  }
  const first = offers[0]
  if (first?.kind !== 'artifact-actions') throw new Error('fixture did not offer an artifact')
  return {
    active,
    offers,
    subject: planningSubject({
      key: {},
      choice: first.primary.choice,
      installedRoute: materializationRouteIdentity(first.primary.route),
      selectionDigest: selection.digest,
    }),
  }
}

function observation(
  active: V2ActiveProjection,
  resolvedArtifactDigest: string | null,
): ArtifactChoiceReconcileOutcome {
  return Object.freeze({
    kind: 'waiting',
    reason: 'shape-unsettled',
    observation: Object.freeze({
      projectionEpoch: active.epoch,
      selectionDigest: active.selection.digest,
      resolvedArtifactDigest,
    }),
  })
}
