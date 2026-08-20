import { describe, expect, it } from 'vitest'

import { createSelectionSpec } from '../../../src/transfer/intent'
import type {
  ArtifactActionsOffer,
  ArtifactResolutionObservation,
  OfferedArtifactChoice,
  SelectionProjectionV1,
} from '../../../src/output/planning'
import {
  materializationRouteIdentity,
  offerArtifacts,
  reconcileArtifactChoice,
} from '../../../src/output/planning'
import {
  COMPLETE_DISCOVERY,
  environment,
  fsaTarget,
  handoffTarget,
  identity,
  managedTarget,
  projection,
  singleFileProof,
  treeProof,
  workspaceOffer,
} from './fixture'

describe('artifact choice reconciliation', () => {
  it('progresses waiting to resolved once within one projection epoch', async () => {
    const selection = await selectionSpec()
    const unsettled = projection(selection, treeProof({ kind: 'unsettled' }), 1n)
    const offered = await offeredChoice(unsettled, environment({ targets: [fsaTarget()] }))
    const initial = initialObservation(unsettled)

    const waiting = await reconcile(offered, unsettled, { kind: 'discovering' },
      environment({ targets: [fsaTarget()] }), initial)
    expect(waiting).toMatchObject({
      kind: 'waiting',
      observation: { resolvedArtifactDigest: null },
    })

    const settled = projection(selection, treeProof(), 2n, 1n)
    const resolved = await reconcile(offered, settled, COMPLETE_DISCOVERY,
      environment({ targets: [fsaTarget()] }), waiting.observation)
    expect(resolved).toMatchObject({
      kind: 'resolved',
      action: {
        kind: 'resolved-artifact-action',
        selectionDigest: selection.digest,
        choice: offered.choice,
        artifact: { kind: 'directory-tree' },
      },
    })
    if (resolved.kind !== 'resolved') throw new Error('expected resolved choice')
    expect(resolved.action.artifact.digest).toBe(resolved.action.resolvedArtifactDigest)
  })

  it('retains the choice through retry and replacement-epoch recomputation', async () => {
    const selection = await selectionSpec()
    const firstProjection = projection(selection, treeProof({ kind: 'unsettled' }), 1n, 1n)
    const route = environment({ targets: [fsaTarget()] })
    const offered = await offeredChoice(firstProjection, route)

    const retry = await reconcile(offered, firstProjection, {
      kind: 'retryable-failure', reason: 'receiver-reconnecting',
    }, route, initialObservation(firstProjection))
    expect(retry).toMatchObject({ kind: 'retry-required', reason: 'receiver-reconnecting' })

    const replacement = projection(selection, treeProof({ kind: 'unsettled' }), 2n, 2n)
    const waiting = await reconcile(offered, replacement, { kind: 'discovering' }, route, retry.observation)
    expect(waiting).toMatchObject({ kind: 'waiting', observation: { projectionEpoch: 2n } })

    const settledReplacement = projection(selection, treeProof(), 3n, 2n)
    const resolved = await reconcile(offered, settledReplacement, COMPLETE_DISCOVERY, route, waiting.observation)
    expect(resolved.kind).toBe('resolved')
  })

  it('accepts repeated current-epoch resolution but rejects every illegal digest progression', async () => {
    const selection = await selectionSpec()
    const current = projection(selection, treeProof(), 1n, 1n)
    const route = environment({ targets: [fsaTarget()] })
    const offered = await offeredChoice(current, route)
    const first = await reconcile(offered, current, COMPLETE_DISCOVERY, route, initialObservation(current))
    expect(first.kind).toBe('resolved')
    const repeated = await reconcile(offered, current, COMPLETE_DISCOVERY, route, first.observation)
    expect(repeated.kind).toBe('resolved')

    const changedSelection = { ...current, selectionDigest: identity(90, 32) } as SelectionProjectionV1
    await expect(reconcile(offered, changedSelection, COMPLETE_DISCOVERY, route, first.observation))
      .rejects.toMatchObject({
        name: 'ArtifactPlanningContractError',
        code: 'same-epoch-selection-digest-changed',
      })

    const unresolved = projection(selection, treeProof({ kind: 'unsettled' }), 1n, 1n)
    await expect(reconcile(offered, unresolved, { kind: 'discovering' }, route, first.observation))
      .rejects.toMatchObject({
        code: 'same-epoch-resolved-artifact-digest-changed',
      })

    await expect(reconcile(offered, unresolved, COMPLETE_DISCOVERY, route, initialObservation(unresolved)))
      .rejects.toMatchObject({
        code: 'complete-projection-left-choice-unresolved',
      })
  })

  it.each([
    ['selection-empty', 'selection-empty'],
    ['artifact-shape-incompatible', 'artifact-shape-incompatible'],
    ['semantic-route-unavailable', 'semantic-route-unavailable'],
  ] as const)('classifies %s without conflating it with observation replacement', async (scenario, reason) => {
    const selection = await selectionSpec()
    const initialProjection = projection(selection, treeProof(), 1n, 1n)
    const offered = await offeredChoice(initialProjection, environment({ targets: [fsaTarget()] }))
    let currentProjection = projection(selection, treeProof(), 1n, 2n)
    if (scenario === 'selection-empty') {
      currentProjection = projection(selection, { kind: 'none' }, 0n, 2n)
    } else if (scenario === 'artifact-shape-incompatible') {
      currentProjection = projection(selection, singleFileProof(), 1n, 2n)
    }
    const currentEnvironment = scenario === 'semantic-route-unavailable'
      ? environment({ targets: [managedTarget()] })
      : environment({ targets: [fsaTarget()] })

    const result = await reconcile(
      offered,
      currentProjection,
      COMPLETE_DISCOVERY,
      currentEnvironment,
      initialObservation(initialProjection),
    )
    expect(result).toMatchObject({ kind: 'invalidated', reason })
  })

  it('classifies selection change before attempting route replacement', async () => {
    const selection = await selectionSpec()
    const initialProjection = projection(selection, treeProof(), 1n, 1n)
    const offered = await offeredChoice(initialProjection, environment({ targets: [fsaTarget()] }))
    const changed = {
      ...projection(selection, treeProof(), 1n, 2n),
      selectionDigest: identity(91, 32),
    } as SelectionProjectionV1

    const result = await reconcile(offered, changed, COMPLETE_DISCOVERY,
      environment({ targets: [fsaTarget()] }), initialObservation(initialProjection))
    expect(result).toMatchObject({ kind: 'invalidated', reason: 'selection-changed' })
  })

  it('keeps lower-bound refinement compatible only while the selected hard plan remains eligible', async () => {
    const selection = await selectionSpec()
    const limitedTarget = { ...fsaTarget(), hardMaximumOutputBytes: 10n }
    const firstProjection = projection(selection, treeProof(), 5n, 1n)
    const offered = await offeredChoice(firstProjection, environment({ targets: [limitedTarget] }))

    const fits = await reconcile(offered, projection(selection, treeProof(), 10n, 2n),
      COMPLETE_DISCOVERY, environment({ targets: [limitedTarget] }), initialObservation(firstProjection))
    expect(fits.kind).toBe('resolved')

    const exceeded = await reconcile(offered, projection(selection, treeProof(), 11n, 3n),
      COMPLETE_DISCOVERY, environment({ targets: [limitedTarget] }), fits.observation)
    expect(exceeded).toMatchObject({ kind: 'invalidated', reason: 'hard-limit-exceeded' })
  })

  it('prefers the installed route across re-probes and ignores quota-only observations', async () => {
    const selection = await selectionSpec()
    const firstProjection = projection(selection, singleFileProof(), 1n, 1n)
    const initialEnvironment = environment({
      targets: [handoffTarget('handoff-b')],
      workspace: { ...workspaceOffer(), quotaAvailabilityEstimateBytes: 100n },
    })
    const offered = await offeredChoice(firstProjection, initialEnvironment)
    const refreshed = environment({
      targets: [handoffTarget('handoff-a'), handoffTarget('handoff-b')],
      workspace: { ...workspaceOffer(), quotaAvailabilityEstimateBytes: 0n },
    })

    const result = await reconcile(offered, projection(selection, singleFileProof(), 2n, 2n),
      COMPLETE_DISCOVERY, refreshed, initialObservation(firstProjection))
    expect(result).toMatchObject({
      kind: 'resolved',
      action: {
        route: {
          kind: 'workspace-then-publish',
          publicationTarget: { routeId: 'handoff-b' },
          workspace: { quotaAvailabilityEstimateBytes: 0n },
        },
      },
    })
  })

  it('resolves the frozen plan family even when a higher-priority route appears', async () => {
    const selection = await selectionSpec()
    const firstProjection = projection(selection, singleFileProof(), 1n, 1n)
    const initialEnvironment = environment({ targets: [handoffTarget()], workspace: workspaceOffer() })
    const offered = await offeredChoice(firstProjection, initialEnvironment)
    const refreshed = environment({
      targets: [managedTarget(), handoffTarget()],
      workspace: workspaceOffer(),
    })

    const result = await reconcile(offered, projection(selection, singleFileProof(), 2n, 2n),
      COMPLETE_DISCOVERY, refreshed, initialObservation(firstProjection))
    expect(result).toMatchObject({
      kind: 'resolved',
      action: { choice: { plan: { kind: 'workspace-then-publish' } } },
    })
  })
})

async function selectionSpec() {
  return createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
}

async function offeredChoice(
  currentProjection: SelectionProjectionV1,
  currentEnvironment: Parameters<typeof offerArtifacts>[2],
): Promise<OfferedArtifactChoice> {
  const offers = await offerArtifacts(currentProjection, { kind: 'discovering' }, currentEnvironment)
  return requireActions(offers).primary
}

function initialObservation(projectionValue: SelectionProjectionV1): ArtifactResolutionObservation {
  return Object.freeze({
    projectionEpoch: projectionValue.epoch,
    selectionDigest: projectionValue.selectionDigest,
    resolvedArtifactDigest: null,
  })
}

function reconcile(
  offered: OfferedArtifactChoice,
  currentProjection: SelectionProjectionV1,
  discovery: Parameters<typeof reconcileArtifactChoice>[0]['discovery'],
  currentEnvironment: Parameters<typeof reconcileArtifactChoice>[0]['environment'],
  previousObservation: ArtifactResolutionObservation,
) {
  return reconcileArtifactChoice({
    choice: offered.choice,
    preferredRoute: materializationRouteIdentity(offered.route),
    expectedSelectionDigest: previousObservation.selectionDigest,
    projection: currentProjection,
    discovery,
    environment: currentEnvironment,
    previousObservation,
  })
}

function requireActions(value: Awaited<ReturnType<typeof offerArtifacts>>): ArtifactActionsOffer {
  if (value.kind !== 'artifact-actions') throw new Error(`expected actions, received ${value.kind}`)
  return value
}
