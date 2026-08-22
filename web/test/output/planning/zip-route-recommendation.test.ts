import { describe, expect, it } from 'vitest'

import { createSelectionSpec } from '../../../src/transfer/intent'
import {
  WorkspaceCostObservationAccumulatorV1,
  offerArtifacts,
  recommendZipRoutes,
  zipRouteRecommendationPolicyDigestV1,
  type ArtifactActionsOffer,
} from '../../../src/output/planning'
import {
  COMPLETE_DISCOVERY,
  directZipTarget,
  environment,
  fsaTarget,
  handoffTarget,
  identity,
  projection,
  reviewedDirectZipSupport,
  treeProof,
  workspaceOffer,
} from './fixture'

describe('ZIP route recommendation policy V1', () => {
  it('binds the inclusive reviewed workspace peak to its canonical digest', async () => {
    await expect(zipRouteRecommendationPolicyDigestV1(1_073_744_986n))
      .resolves.toBe('zHRGRc5-OvZ4Z8U2E1ORwNWnccnf_p35QB8iSXlixqI')
  })

  it('keeps direct unavailable and emits no recommendation without reviewed support and threshold', async () => {
    const selection = await selectionSpec()
    const actions = requireActions(await offerArtifacts(
      projection(selection, treeProof(), 10n),
      COMPLETE_DISCOVERY,
      environment({ targets: [handoffTarget()], workspace: workspaceOffer() }),
    ))

    expect(actions.zip?.primary.route.kind).toBe('workspace-then-publish')
    expect(actions.zip?.secondary).toBeNull()
    expect(actions.zip?.recommendation).toEqual({
      kind: 'no-recommendation', reason: 'only-one-route-available',
    })
    expect(actions.alternatives.some((choice) => choice.route.kind === 'direct-resumable-zip')).toBe(false)
    expect(() => environment({
      targets: [directZipTarget()],
      directZipSupport: reviewedDirectZipSupport(),
    })).toThrow(/reviewed support facts/u)
  })

  it('ranks both reviewed routes without changing their canonical choice identities', async () => {
    const selection = await selectionSpec()
    const base = projection(selection, treeProof(), 10n)
    const workspaceCostObservation = costObservation()
    const actions = requireActions(await offerArtifacts(
      { ...base, workspaceCostObservation },
      COMPLETE_DISCOVERY,
      environment({
        targets: [fsaTarget(), directZipTarget(), handoffTarget()],
        workspace: workspaceOffer(),
        directZipSupport: reviewedDirectZipSupport(),
        zipRecommendationPolicy: {
          version: 1,
          kind: 'available',
          workspacePeakBytesThreshold: workspaceCostObservation.peakOwnedBytes,
          policyDigest: reviewedDirectZipSupport().recommendationPolicyDigest,
        },
      }),
    ))

    expect(actions.primary.choice.plan.kind).toBe('direct-tree')
    expect(actions.zip?.primary.route.kind).toBe('workspace-then-publish')
    expect(actions.zip?.secondary?.route.kind).toBe('direct-resumable-zip')
    expect(actions.zip?.recommendation).toMatchObject({
      kind: 'recommended', reason: 'workspace-within-reviewed-budget',
    })
    expect(actions.zip?.primary.choice.choiceId)
      .toBe('RW0aXukzHVFiMjNEaoYb8qGKTN-AKAhw7u-Yi_-WsoQ')
    expect(actions.zip?.secondary?.choice.choiceId)
      .toBe('0dkx9vDTzvH7B7a9EUoJBOWLCWgmVwLoFH3jjRmfHFU')
    expect(actions.zip?.primary.sizeProjection).toMatchObject({
      raw: { kind: 'exact', bytes: 10n },
      artifact: { kind: 'exact', bytes: workspaceCostObservation.packageBytes },
    })
    expect(actions.zip?.secondary?.sizeProjection).toEqual({
      raw: { kind: 'exact', bytes: 10n },
      artifact: { kind: 'estimated-lower-bound', bytes: 10n },
    })
    const direct = actions.zip!.secondary!
    const workspace = actions.zip!.primary
    expect(recommendZipRoutes({
      direct,
      workspace,
      portable: null,
      discoveryComplete: true,
      workspaceCost: workspaceCostObservation,
      policy: {
        version: 1,
        kind: 'available',
        workspacePeakBytesThreshold: workspaceCostObservation.peakOwnedBytes - 1n,
        policyDigest: reviewedDirectZipSupport().recommendationPolicyDigest,
      },
    })?.primary.route.kind).toBe('direct-resumable-zip')
  })

  it('accumulates checked raw, ZIP, spool, metadata, and peak observations without a manifest', () => {
    const observation = costObservation()

    expect(observation).toMatchObject({ version: 1, rawBytes: 10n })
    expect(observation.packageBytes).toBeGreaterThan(observation.rawBytes)
    expect(observation.peakOwnedBytes).toBe(
      observation.rawBytes + observation.packageBytes +
      observation.centralDirectorySpoolBytes + observation.durableMetadataBytes,
    )
  })
})

function costObservation() {
  const accumulator = new WorkspaceCostObservationAccumulatorV1()
  accumulator.observe({ kind: 'directory', path: ['photos'] })
  accumulator.observe({ kind: 'file', path: ['photos', 'one.jpg'], exactSize: 10n })
  return accumulator.complete()
}

async function selectionSpec() {
  return createSelectionSpec({
    shareInstance: identity(1), syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
}

function requireActions(value: Awaited<ReturnType<typeof offerArtifacts>>): ArtifactActionsOffer {
  if (value.kind !== 'artifact-actions') throw new Error(`expected actions, received ${value.kind}`)
  return value
}
