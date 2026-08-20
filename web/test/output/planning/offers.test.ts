import { describe, expect, it, vi } from 'vitest'

import { createSelectionSpec } from '../../../src/transfer/intent'
import type { ArtifactActionsOffer } from '../../../src/output/planning'
import {
  createEnvironmentOffers,
  offerArtifacts,
} from '../../../src/output/planning'
import {
  COMPLETE_DISCOVERY,
  environment,
  fsaTarget,
  handoffTarget,
  identity,
  portableOffer,
  precreatedBrowserFileTarget,
  projection,
  singleFileProof,
  treeProof,
  workspaceOffer,
} from './fixture'

describe('artifact offers', () => {
  it('returns data-only confirming, retry, and empty states without a picker port', async () => {
    const selection = await selectionSpec()
    const picker = vi.fn()
    const offers = createEnvironmentOffers({
      targets: [handoffTarget()],
      ...({ pickerAdapter: picker } as object),
    })
    const unknown = projection(selection, { kind: 'unknown' })

    const confirming = await offerArtifacts(unknown, { kind: 'discovering' }, offers)
    const retry = await offerArtifacts(unknown, {
      kind: 'retryable-failure', reason: 'catalog-temporarily-unavailable',
    }, offers)
    const empty = await offerArtifacts(projection(selection, { kind: 'none' }), COMPLETE_DISCOVERY, offers)

    expect(confirming).toMatchObject({
      kind: 'confirming-selected-content',
      interactive: false,
      selectionDigest: selection.digest,
      decision: { name: 'receive.offer.disabled', offer_unavailable_reason: 'shape-unsettled' },
    })
    expect(retry).toMatchObject({ kind: 'retry-confirmation', interactive: true })
    expect(empty).toMatchObject({ kind: 'selection-empty', interactive: false })
    expect(picker).not.toHaveBeenCalled()
    expect(containsFunction(confirming)).toBe(false)
    expect(containsFunction(retry)).toBe(false)
    expect(containsFunction(empty)).toBe(false)
  })

  it('separates a proven tree choice from nullable executable evidence', async () => {
    const selection = await selectionSpec()
    const offers = requireActions(await offerArtifacts(
      projection(selection, treeProof({ kind: 'unsettled' }), 1n),
      { kind: 'discovering' },
      environment({ targets: [fsaTarget()] }),
    ))

    expect(offers.primary).toMatchObject({
      kind: 'offered-artifact-choice',
      choice: {
        kind: 'artifact-choice',
        operation: 'save-directory-tree',
        artifactKind: 'directory-tree',
        plan: { kind: 'direct-tree', target: { legalProfile: 'fsa-tree' } },
      },
      route: { kind: 'direct-tree', target: { routeId: 'fsa' } },
      suggestedName: null,
    })
    expect(offers.primary).not.toHaveProperty('artifact')
    expect(offers.primary.choice).not.toHaveProperty('artifact')
    expect(offers.primary.choice).not.toHaveProperty('projectionEpoch')
    expect(offers.primary.choice.plan).not.toHaveProperty('target.routeId')
  })

  it('publishes retry instead of another picker choice when tree resolution is interrupted', async () => {
    const selection = await selectionSpec()
    const offers = await offerArtifacts(
      projection(selection, treeProof({ kind: 'unsettled' }), 1n),
      { kind: 'retryable-failure', reason: 'receiver-reconnecting' },
      environment({ targets: [fsaTarget()] }),
    )

    expect(offers).toMatchObject({
      kind: 'retry-confirmation',
      reason: 'discovery-retry-required',
      selectionDigest: selection.digest,
    })
  })

  it('makes DirectoryTree primary and ZIP separately explicit for settled tree proof', async () => {
    const selection = await selectionSpec()
    const actions = requireActions(await offerArtifacts(
      projection(selection, treeProof(), 1024n),
      COMPLETE_DISCOVERY,
      environment({ targets: [fsaTarget(), handoffTarget()], workspace: workspaceOffer() }),
    ))

    expect(actions.primary.choice).toMatchObject({
      operation: 'save-directory-tree',
      artifactKind: 'directory-tree',
      plan: { kind: 'direct-tree' },
    })
    expect(actions.alternatives).toHaveLength(1)
    expect(actions.alternatives[0]?.choice).toMatchObject({
      operation: 'download-zip',
      artifactKind: 'zip-archive',
      recovery: 'workspace-resumable',
      preparation: { manifest: 'exact-zip', hardAdmission: 'workspace-budget' },
      plan: { kind: 'workspace-then-publish' },
    })
  })

  it('offers only explicit ZIP when no directory target exists and never legalizes a save picker', async () => {
    const selection = await selectionSpec()
    const zipOnly = requireActions(await offerArtifacts(
      projection(selection, treeProof(), 1024n),
      COMPLETE_DISCOVERY,
      environment({ targets: [handoffTarget()], portable: portableOffer() }),
    ))
    const unsafe = await offerArtifacts(
      projection(selection, treeProof(), 1024n),
      COMPLETE_DISCOVERY,
      environment({ targets: [precreatedBrowserFileTarget()] }),
    )

    expect(zipOnly.primary.choice).toMatchObject({
      operation: 'check-then-download',
      artifactKind: 'zip-archive',
      plan: { kind: 'portable-handoff' },
      preparation: { manifest: 'exact-artifact', hardAdmission: 'portable-artifact' },
    })
    expect(unsafe).toMatchObject({ kind: 'no-safe-destination', reason: 'no-safe-destination' })
  })

  it('treats quota estimates as observations rather than choice semantics', async () => {
    const selection = await selectionSpec()
    const first = requireActions(await offerArtifacts(
      projection(selection, singleFileProof(), 1024n),
      COMPLETE_DISCOVERY,
      environment({
        targets: [handoffTarget()],
        workspace: { ...workspaceOffer(), quotaAvailabilityEstimateBytes: 1n },
      }),
    )).primary
    const refreshed = requireActions(await offerArtifacts(
      projection(selection, singleFileProof(), 1024n),
      COMPLETE_DISCOVERY,
      environment({
        targets: [handoffTarget()],
        workspace: { ...workspaceOffer(), quotaAvailabilityEstimateBytes: 0n },
      }),
    )).primary

    expect(first.choice).toEqual(refreshed.choice)
    expect(first.route).not.toEqual(refreshed.route)
  })
})

async function selectionSpec() {
  return createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
}

function requireActions(value: Awaited<ReturnType<typeof offerArtifacts>>): ArtifactActionsOffer {
  if (value.kind !== 'artifact-actions') throw new Error(`expected actions, received ${value.kind}`)
  return value
}

function containsFunction(value: unknown, seen = new Set<object>()): boolean {
  if (typeof value === 'function') return true
  if (typeof value !== 'object' || value === null || seen.has(value)) return false
  seen.add(value)
  for (const key of Object.keys(value)) {
    if (containsFunction((value as Record<string, unknown>)[key], seen)) return true
  }
  return false
}
