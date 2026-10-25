import { describe, expect, it } from 'vitest'

import { createSelectionSpec } from '../../../src/transfer/intent'
import {
  WorkspaceCostObservationAccumulatorV1,
  offerArtifacts,
  recommendZipRoutes,
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
  runtimeDirectZipSupport,
  treeProof,
  workspaceOffer,
} from './fixture'

describe('ZIP route recommendation policy V1', () => {
  it('requires runtime authority independently of optional route-ranking policy', async () => {
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
    const directWithoutRanking = requireActions(await offerArtifacts(
      projection(selection, treeProof(), 10n),
      COMPLETE_DISCOVERY,
      environment({
        targets: [directZipTarget(), handoffTarget()],
        workspace: workspaceOffer(),
        directZipSupport: runtimeDirectZipSupport(),
      }),
    ))
    expect(directWithoutRanking.zip?.primary.route.kind).toBe('direct-resumable-zip')
    expect(directWithoutRanking.zip?.secondary?.route.kind).toBe('workspace-then-publish')
    expect(directWithoutRanking.zip?.recommendation).toEqual({
      kind: 'no-recommendation', reason: 'recommendation-policy-unavailable',
    })
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
        directZipSupport: runtimeDirectZipSupport(),
        zipRecommendationPolicy: {
          kind: 'available',
          workspacePeakBytesThreshold: workspaceCostObservation.peakOwnedBytes,
        },
      }),
    ))

    expect(actions.primary.choice.plan.kind).toBe('direct-tree')
    expect(actions.zip?.primary.route.kind).toBe('workspace-then-publish')
    expect(actions.zip?.secondary?.route.kind).toBe('direct-resumable-zip')
    expect(actions.zip?.recommendation).toMatchObject({
      kind: 'recommended', reason: 'workspace-within-policy-budget',
    })
    expect(actions.zip?.primary.choice.choiceId)
      .toBe('vQj0Uda3oyRmvsZcz2qN0T9-f5m99Lcn0NK-9rS2_-k')
    expect(actions.zip?.secondary?.choice.choiceId)
      .toBe('0dkx9vDTzvH7B7a9EUoJBOWLCWgmVwLoFH3jjRmfHFU')
    expect(actions.zip?.primary.sizeProjection).toMatchObject({
      raw: { kind: 'exact', bytes: 10n },
      artifact: { kind: 'exact', bytes: workspaceCostObservation.archiveBytes },
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
        kind: 'available',
        workspacePeakBytesThreshold: workspaceCostObservation.peakOwnedBytes - 1n,
      },
    })?.primary.route.kind).toBe('direct-resumable-zip')
  })

  it('counts the one archive plus metadata without a retained raw copy or directory spool', () => {
    const observation = costObservation()

    expect(Object.keys(observation).sort()).toEqual([
      'archiveBytes', 'durableMetadataBytes', 'peakOwnedBytes', 'version',
    ])
    expect(observation.archiveBytes).toBeGreaterThan(10n)
    expect(observation.peakOwnedBytes).toBe(
      observation.archiveBytes + observation.durableMetadataBytes,
    )
  })
})

it('keeps large ZIP64 recommendations available beyond the former workspace limits', () => {
  const accumulator = new WorkspaceCostObservationAccumulatorV1()
  const exactSize = 20n * 1024n ** 3n
  accumulator.observe({ kind: 'file', path: ['large.bin'], exactSize })
  const observation = accumulator.complete()
  expect(observation.archiveBytes).toBeGreaterThan(exactSize)
  expect(observation.peakOwnedBytes).toBe(observation.archiveBytes + observation.durableMetadataBytes)
})

it('refines ZIP ranking from discovered whole-output bytes without completing discovery', async () => {
  const selection = await selectionSpec()
  const policy = { kind: 'available' as const,
    workspacePeakBytesThreshold: 100n }
  const runtime = environment({ targets: [fsaTarget(), directZipTarget(), handoffTarget()],
    workspace: workspaceOffer(), directZipSupport: runtimeDirectZipSupport(), zipRecommendationPolicy: policy })
  const early = requireActions(await offerArtifacts(projection(selection, treeProof(), 10n),
    { kind: 'discovering' }, runtime))
  expect(early.zip?.recommendation).toMatchObject({
    kind: 'recommended', reason: 'workspace-within-discovered-budget',
  })
  expect(early.primary.route.kind).toBe('direct-tree')
  const later = requireActions(await offerArtifacts(projection(selection, treeProof(), 101n),
    { kind: 'discovering' }, runtime))
  expect(later.zip?.primary.route.kind).toBe('direct-resumable-zip')
  expect(later.primary.route.kind).toBe('direct-tree')
  expect(later.zip?.secondary?.choice.choiceId).toBe(early.zip?.primary.choice.choiceId)
})

it('snapshots growing archive costs without freezing or completing the accumulator', () => {
  const accumulator = new WorkspaceCostObservationAccumulatorV1()
  accumulator.observe({ kind: 'file', path: ['one'], exactSize: 10n })
  const early = accumulator.snapshot()
  accumulator.observe({ kind: 'file', path: ['two'], exactSize: 100n })
  expect(accumulator.snapshot().archiveBytes).toBeGreaterThan(early.archiveBytes)
  expect(accumulator.complete()).toEqual(accumulator.snapshot())
  expect(() => accumulator.complete()).toThrow('only once')
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
