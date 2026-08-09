import { describe, expect, it, vi } from 'vitest'

import {
  createFSANamedEntryReservation,
  createManagedAtomicReservation,
  createNativeNamedEntryReservation,
  createPortableBinding,
  createSelectionSpec,
  createWorkspaceBinding,
} from '../../../src/transfer/intent'
import type { ArtifactActionsOffer } from '../../../src/output/planning'
import {
  bindMaterialization,
  createEnvironmentOffers,
  offerArtifacts,
} from '../../../src/output/planning'
import {
  COMPLETE_DISCOVERY,
  environment,
  fsaTarget,
  handoffTarget,
  identity,
  managedTarget,
  portableOffer,
  precreatedBrowserFileTarget,
  projection,
  singleFileProof,
  treeProof,
  workspaceOffer,
} from './fixture'

describe('artifact offers and pure materialization binding', () => {
  it('returns data-only confirming, retry, and empty states without a picker port', async () => {
    const selection = await selectionSpec()
    const picker = vi.fn()
    const offers = createEnvironmentOffers({
      targets: [handoffTarget()],
      ...( { pickerAdapter: picker } as object),
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
      decision: { name: 'receive.offer.disabled', offer_unavailable_reason: 'shape-unsettled' },
    })
    expect(retry).toMatchObject({ kind: 'retry-confirmation', interactive: true })
    expect(empty).toMatchObject({ kind: 'selection-empty', interactive: false })
    expect(picker).not.toHaveBeenCalled()
    expect(containsFunction(confirming)).toBe(false)
    expect(containsFunction(retry)).toBe(false)
    expect(containsFunction(empty)).toBe(false)
  })

  it('makes DirectoryTree primary and ZIP separately explicit for tree proof', async () => {
    const selection = await selectionSpec()
    const offers = await offerArtifacts(
      projection(selection, treeProof(), 1024n),
      COMPLETE_DISCOVERY,
      environment({ targets: [fsaTarget(), handoffTarget()], workspace: workspaceOffer() }),
    )
    const actions = requireActions(offers)

    expect(actions.primary).toMatchObject({
      operation: 'save-directory-tree',
      artifactKind: 'directory-tree',
      plan: { kind: 'direct-tree', target: { legalProfile: 'fsa-tree' } },
    })
    expect(actions.decision).toMatchObject({
      name: 'receive.offer.computed', primary_artifact_kind: 'directory-tree',
    })
    expect(actions.alternatives).toHaveLength(1)
    expect(actions.alternatives[0]).toMatchObject({
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

    expect(zipOnly.primary).toMatchObject({
      operation: 'check-then-download',
      artifactKind: 'zip-archive',
      plan: { kind: 'portable-handoff' },
      preparation: { manifest: 'exact-artifact', hardAdmission: 'portable-artifact' },
    })
    expect(unsafe).toMatchObject({ kind: 'no-safe-destination', reason: 'no-safe-destination' })
  })

  it('keeps a proven Tree actionable before its naming basis settles', async () => {
    const selection = await selectionSpec()
    const offers = requireActions(await offerArtifacts(
      projection(selection, treeProof({ kind: 'unsettled' }), 1n),
      { kind: 'discovering' },
      environment({ targets: [fsaTarget()] }),
    ))

    expect(offers.primary).toMatchObject({
      operation: 'save-directory-tree', artifactKind: 'directory-tree', artifact: null,
    })
    expect(offers.alternatives).toEqual([])
  })

  it('binds each closed plan to the explicit artifact and honest guarantees', async () => {
    const selection = await selectionSpec()
    await expect(bindDirectTree(selection)).resolves.toMatchObject({
      kind: 'bound', intent: { artifact: { kind: 'directory-tree' }, plan: { kind: 'direct-tree' } },
      decision: { name: 'receive.intent.frozen' },
    })
    await expect(bindDirectAtomic(selection)).resolves.toMatchObject({
      kind: 'bound', intent: { artifact: { kind: 'original-file' }, plan: { kind: 'direct-atomic' } },
    })
    await expect(bindWorkspace(selection)).resolves.toMatchObject({
      kind: 'bound', intent: {
        artifact: { kind: 'original-file' },
        plan: { kind: 'workspace-then-publish', preparation: 'none' },
      },
    })
    await expect(bindPortable(selection)).resolves.toMatchObject({
      kind: 'bound', intent: {
        artifact: { kind: 'original-file' },
        plan: { kind: 'portable-handoff', preparation: 'exact-artifact' },
      },
    })
  })

  it('returns to artifact choice when capability changes instead of silently falling back', async () => {
    const selection = await selectionSpec()
    const initialProjection = projection(selection, treeProof(), 1024n)
    const initialEnvironment = environment({
      targets: [fsaTarget(), handoffTarget()], workspace: workspaceOffer(),
    })
    const initial = requireActions(await offerArtifacts(
      initialProjection, COMPLETE_DISCOVERY, initialEnvironment,
    ))
    const chosenZip = initial.alternatives[0]
    if (chosenZip?.artifact === undefined || chosenZip.artifact === null) {
      throw new Error('ZIP action must carry a resolved artifact')
    }
    const workspace = await createWorkspaceBinding({
      operationId: identity(60), workspaceId: identity(61), artifact: chosenZip.artifact,
      repositoryRef: identity(62, 32),
    })

    const result = await bindMaterialization({
      selection,
      chosenAction: chosenZip,
      currentProjection: initialProjection,
      currentDiscovery: COMPLETE_DISCOVERY,
      currentEnvironment: environment({ targets: [fsaTarget()] }),
      acquired: { kind: 'workspace-binding', workspaceOfferId: 'workspace', workspace },
    })

    expect(result).toMatchObject({
      kind: 'artifact-choice-required', unavailableReason: 'capability-changed',
    })
    if (result.kind === 'bound') throw new Error('capability loss must not bind a fallback artifact')
  })

  it('preserves the chosen artifact when refreshed authority has identical guarantees', async () => {
    const selection = await selectionSpec()
    const currentProjection = projection(selection, singleFileProof(), 128n)
    const chosenAction = requireActions(await offerArtifacts(
      currentProjection,
      COMPLETE_DISCOVERY,
      environment({ targets: [managedTarget('managed-old')] }),
    )).primary
    if (chosenAction.artifact === null) throw new Error('original artifact must be resolved')
    const reservation = await createManagedAtomicReservation({
      operationId: identity(66), reservationId: identity(67), artifact: chosenAction.artifact,
      authorityRef: identity(68, 32), nameAuthority: 'application-chosen',
      requestedName: 'report.txt', reservedName: 'report.txt', collisionIndex: 0,
    })

    const result = await bindMaterialization({
      selection,
      chosenAction,
      currentProjection,
      currentDiscovery: COMPLETE_DISCOVERY,
      currentEnvironment: environment({ targets: [managedTarget('managed-new')] }),
      acquired: {
        kind: 'destination-reservation',
        environmentTargetOfferId: 'managed-new',
        reservation,
      },
    })

    expect(result).toMatchObject({
      kind: 'bound', intent: { artifact: { kind: 'original-file' }, plan: { kind: 'direct-atomic' } },
    })
  })

  it('rejects a reservation whose authority overstates the offered target guarantees', async () => {
    const selection = await selectionSpec()
    const currentProjection = projection(selection, singleFileProof(), 128n)
    const currentEnvironment = environment({ targets: [fsaTarget()] })
    const chosenAction = requireActions(await offerArtifacts(
      currentProjection, COMPLETE_DISCOVERY, currentEnvironment,
    )).primary
    if (chosenAction.artifact === null) throw new Error('single-file tree must be resolved')
    const dishonestReservation = await createNativeNamedEntryReservation({
      operationId: identity(63), reservationId: identity(64), artifact: chosenAction.artifact,
      authorityRef: identity(65, 32), reservedName: 'report.txt', collisionIndex: 0,
    })

    await expect(bindMaterialization({
      selection,
      chosenAction,
      currentProjection,
      currentDiscovery: COMPLETE_DISCOVERY,
      currentEnvironment,
      acquired: {
        kind: 'destination-reservation',
        environmentTargetOfferId: 'fsa',
        reservation: dishonestReservation,
      },
    })).rejects.toThrow(/offered guarantees/u)
  })

  it('treats capacity estimates as observations, not hard admission receipts', async () => {
    const selection = await selectionSpec()
    const offers = requireActions(await offerArtifacts(
      projection(selection, singleFileProof(), 1024n),
      COMPLETE_DISCOVERY,
      environment({
        targets: [handoffTarget()],
        workspace: { ...workspaceOffer(), quotaAvailabilityEstimateBytes: 0n },
      }),
    ))

    expect(offers.primary).toMatchObject({
      plan: { kind: 'workspace-then-publish' },
      preparation: { hardAdmission: 'workspace-budget' },
    })
  })
})

async function bindDirectTree(selection: Awaited<ReturnType<typeof selectionSpec>>) {
  const currentProjection = projection(selection, singleFileProof(), 128n)
  const currentEnvironment = environment({ targets: [fsaTarget()] })
  const chosenAction = requireActions(await offerArtifacts(
    currentProjection, COMPLETE_DISCOVERY, currentEnvironment,
  )).primary
  if (chosenAction.artifact === null) throw new Error('single-file artifact must be resolved')
  const reservation = await createFSANamedEntryReservation({
    operationId: identity(70), reservationId: identity(71), artifact: chosenAction.artifact,
    authorityRef: identity(72, 32), reservedName: 'report.txt', collisionIndex: 0,
  })
  return bindMaterialization({
    selection, chosenAction, currentProjection, currentDiscovery: COMPLETE_DISCOVERY,
    currentEnvironment,
    acquired: { kind: 'destination-reservation', environmentTargetOfferId: 'fsa', reservation },
  })
}

async function bindDirectAtomic(selection: Awaited<ReturnType<typeof selectionSpec>>) {
  const currentProjection = projection(selection, singleFileProof(), 128n)
  const currentEnvironment = environment({ targets: [managedTarget()] })
  const chosenAction = requireActions(await offerArtifacts(
    currentProjection, COMPLETE_DISCOVERY, currentEnvironment,
  )).primary
  if (chosenAction.artifact === null) throw new Error('original artifact must be resolved')
  const reservation = await createManagedAtomicReservation({
    operationId: identity(73), reservationId: identity(74), artifact: chosenAction.artifact,
    authorityRef: identity(75, 32), nameAuthority: 'application-chosen',
    requestedName: 'report.txt', reservedName: 'report.txt', collisionIndex: 0,
  })
  return bindMaterialization({
    selection, chosenAction, currentProjection, currentDiscovery: COMPLETE_DISCOVERY,
    currentEnvironment,
    acquired: { kind: 'destination-reservation', environmentTargetOfferId: 'managed', reservation },
  })
}

async function bindWorkspace(selection: Awaited<ReturnType<typeof selectionSpec>>) {
  const currentProjection = projection(selection, singleFileProof(), 128n)
  const currentEnvironment = environment({ targets: [handoffTarget()], workspace: workspaceOffer() })
  const chosenAction = requireActions(await offerArtifacts(
    currentProjection, COMPLETE_DISCOVERY, currentEnvironment,
  )).primary
  if (chosenAction.artifact === null) throw new Error('original artifact must be resolved')
  const workspace = await createWorkspaceBinding({
    operationId: identity(76), workspaceId: identity(77), artifact: chosenAction.artifact,
    repositoryRef: identity(78, 32),
  })
  return bindMaterialization({
    selection, chosenAction, currentProjection, currentDiscovery: COMPLETE_DISCOVERY,
    currentEnvironment,
    acquired: { kind: 'workspace-binding', workspaceOfferId: 'workspace', workspace },
  })
}

async function bindPortable(selection: Awaited<ReturnType<typeof selectionSpec>>) {
  const currentProjection = projection(selection, singleFileProof(), 128n)
  const currentEnvironment = environment({ targets: [handoffTarget()], portable: portableOffer() })
  const chosenAction = requireActions(await offerArtifacts(
    currentProjection, COMPLETE_DISCOVERY, currentEnvironment,
  )).primary
  if (chosenAction.artifact === null) throw new Error('original artifact must be resolved')
  const portable = await createPortableBinding({
    operationId: identity(79), portablePlanId: identity(80), artifact: chosenAction.artifact,
  })
  return bindMaterialization({
    selection, chosenAction, currentProjection, currentDiscovery: COMPLETE_DISCOVERY,
    currentEnvironment,
    acquired: {
      kind: 'portable-binding', portableOfferId: 'portable', handoffTargetOfferId: 'handoff', portable,
    },
  })
}

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
