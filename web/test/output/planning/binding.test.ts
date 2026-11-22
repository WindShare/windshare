import { describe, expect, it } from 'vitest'

import {
  createFSANamedEntryReservation,
  createNativeNamedEntryReservation,
  createPortableBinding,
  createSelectionSpec,
  createWorkspaceBinding,
} from '../../../src/transfer/intent'
import type { CandidateMaterializationBinding } from '../../../src/output/planning/binding'
import type { EnvironmentOffers, ResolvedArtifactAction } from '../../../src/output/planning/contracts'
import { bindReceiveIntent } from '../../../src/output/planning/binding'
import { materializationRouteIdentity } from '../../../src/output/planning/guarantees'
import { offerArtifacts } from '../../../src/output/planning/offers'
import { reconcileArtifactChoice } from '../../../src/output/planning/reconciliation'
import {
  COMPLETE_DISCOVERY,
  environment,
  fsaTarget,
  handoffTarget,
  identity,
  managedTarget,
  portableOffer,
  projection,
  singleFileProof,
  workspaceOffer,
} from './fixture'

describe('receive intent binding', () => {
  it('binds all browser plan families to canonical operation and artifact identities', async () => {
    const selection = await selectionSpec()
    const directTree = await resolvedAction(selection, environment({ targets: [fsaTarget()] }))
    const workspace = await resolvedAction(selection, environment({
      targets: [handoffTarget()], workspace: workspaceOffer(),
    }))
    const portable = await resolvedAction(selection, environment({
      targets: [handoffTarget()], portable: portableOffer(),
    }))

    const cases: readonly Readonly<{
      action: ResolvedArtifactAction
      candidate: CandidateMaterializationBinding
      planKind: ResolvedArtifactAction['route']['kind']
    }>[] = [
      {
        action: directTree,
        candidate: {
          kind: 'destination-reservation',
          targetRouteId: 'fsa',
          reservation: await createFSANamedEntryReservation({
            operationId: identity(20), reservationId: identity(21), artifact: directTree.artifact,
            authorityRef: identity(22, 32), logicalReservedName: 'report.txt', physicalName: 'report.txt', collisionIndex: 0,
          }),
        },
        planKind: 'direct-tree',
      },
      {
        action: workspace,
        candidate: {
          kind: 'workspace-binding',
          workspaceRouteId: 'workspace',
          publicationTargetRouteId: 'handoff',
          workspace: await createWorkspaceBinding({
            operationId: identity(26), workspaceId: identity(27), artifact: workspace.artifact,
            repositoryRef: identity(28, 32),
          }),
        },
        planKind: 'workspace-then-publish',
      },
      {
        action: portable,
        candidate: {
          kind: 'portable-binding',
          portableRouteId: 'portable',
          handoffTargetRouteId: 'handoff',
          portable: await createPortableBinding({
            operationId: identity(29), portablePlanId: identity(30), artifact: portable.artifact,
          }),
        },
        planKind: 'portable-handoff',
      },
    ]

    for (const testCase of cases) {
      const bound = await bindReceiveIntent({
        selection,
        action: testCase.action,
        candidate: testCase.candidate,
      })
      expect(bound).toMatchObject({
        intent: {
          artifact: { digest: testCase.action.resolvedArtifactDigest },
          plan: { kind: testCase.planKind },
        },
        decision: { name: 'receive.intent.frozen', plan_kind: testCase.planKind },
      })
      expect(bound.intent.operationId).toBe(operationID(testCase.candidate))
      expect(bound.decision.operation_id).toBe(bound.intent.operationId)
    }
  })

  it('rejects selection and resolved-artifact digest mismatches before plan binding', async () => {
    const selection = await selectionSpec()
    const action = await resolvedAction(selection, environment({
      targets: [handoffTarget()], workspace: workspaceOffer(),
    }))
    const workspace = await createWorkspaceBinding({
      operationId: identity(40), workspaceId: identity(41), artifact: action.artifact,
      repositoryRef: identity(42, 32),
    })
    const candidate = {
      kind: 'workspace-binding' as const,
      workspaceRouteId: 'workspace',
      publicationTargetRouteId: 'handoff',
      workspace,
    }
    const otherSelection = await createSelectionSpec({
      shareInstance: identity(3), syntheticRoot: identity(4),
      rules: { mode: 'node-id', defaultSelected: true, rules: [] },
    })

    await expect(bindReceiveIntent({ selection: otherSelection, action, candidate }))
      .rejects.toThrow(/selection/u)
    await expect(bindReceiveIntent({
      selection,
      action: { ...action, resolvedArtifactDigest: identity(99, 32) },
      candidate,
    })).rejects.toThrow(/artifact evidence/u)
  })

  it.each([
    ['workspace route', { workspaceRouteId: 'other' }],
    ['workspace publication route', { publicationTargetRouteId: 'other' }],
  ] as const)('rejects an inexact %s identity', async (_label, mutation) => {
    const selection = await selectionSpec()
    const action = await resolvedAction(selection, environment({
      targets: [handoffTarget()], workspace: workspaceOffer(),
    }))
    const workspace = await createWorkspaceBinding({
      operationId: identity(50), workspaceId: identity(51), artifact: action.artifact,
      repositoryRef: identity(52, 32),
    })

    await expect(bindReceiveIntent({
      selection,
      action,
      candidate: {
        kind: 'workspace-binding',
        workspaceRouteId: 'workspace',
        publicationTargetRouteId: 'handoff',
        workspace,
        ...mutation,
      },
    })).rejects.toThrow(/route identities/u)
  })

  it.each([
    ['portable route', { portableRouteId: 'other' }],
    ['portable handoff route', { handoffTargetRouteId: 'other' }],
  ] as const)('rejects an inexact %s identity', async (_label, mutation) => {
    const selection = await selectionSpec()
    const action = await resolvedAction(selection, environment({
      targets: [handoffTarget()], portable: portableOffer(),
    }))
    const portable = await createPortableBinding({
      operationId: identity(53), portablePlanId: identity(54), artifact: action.artifact,
    })

    await expect(bindReceiveIntent({
      selection,
      action,
      candidate: {
        kind: 'portable-binding',
        portableRouteId: 'portable',
        handoffTargetRouteId: 'handoff',
        portable,
        ...mutation,
      },
    })).rejects.toThrow(/route identities/u)
  })

  it('rejects an inexact direct route and a reservation that overstates its guarantees', async () => {
    const selection = await selectionSpec()
    const action = await resolvedAction(selection, environment({ targets: [fsaTarget()] }))
    const honest = await createFSANamedEntryReservation({
      operationId: identity(60), reservationId: identity(61), artifact: action.artifact,
      authorityRef: identity(62, 32), logicalReservedName: 'report.txt', physicalName: 'report.txt', collisionIndex: 0,
    })
    await expect(bindReceiveIntent({
      selection,
      action,
      candidate: { kind: 'destination-reservation', targetRouteId: 'other', reservation: honest },
    })).rejects.toThrow(/route guarantees/u)

    const dishonest = await createNativeNamedEntryReservation({
      operationId: identity(63), reservationId: identity(64), artifact: action.artifact,
      authorityRef: identity(65, 32), logicalReservedName: 'report.txt', physicalName: 'report.txt', collisionIndex: 0,
    })
    await expect(bindReceiveIntent({
      selection,
      action,
      candidate: { kind: 'destination-reservation', targetRouteId: 'fsa', reservation: dishonest },
    })).rejects.toThrow(/route guarantees/u)
  })

  it('rejects a resolved route that no longer represents the frozen choice semantics', async () => {
    const selection = await selectionSpec()
    const action = await resolvedAction(selection, environment({
      targets: [managedTarget('managed-a')], workspace: workspaceOffer(),
    }))
    if (action.route.kind !== 'workspace-then-publish') throw new Error('expected workspace route')
    const forgedAction: ResolvedArtifactAction = {
      ...action,
      route: {
        ...action.route,
        publicationTarget: environment({ targets: [managedTarget('managed-b', 'user-chosen')] })
          .targets[0] as Extract<typeof action.route.publicationTarget, { kind: 'managed-atomic-file-target' }>,
      },
    }
    const workspace = await createWorkspaceBinding({
      operationId: identity(70), workspaceId: identity(71), artifact: action.artifact,
      repositoryRef: identity(72, 32),
    })

    await expect(bindReceiveIntent({
      selection,
      action: forgedAction,
      candidate: {
        kind: 'workspace-binding',
        workspaceRouteId: 'workspace',
        publicationTargetRouteId: 'managed-b',
        workspace,
      },
    })).rejects.toThrow(/frozen plan semantics/u)
  })
})

async function selectionSpec() {
  return createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
}

async function resolvedAction(
  selection: Awaited<ReturnType<typeof selectionSpec>>,
  currentEnvironment: EnvironmentOffers,
): Promise<ResolvedArtifactAction> {
  const currentProjection = projection(selection, singleFileProof(), 1n)
  const offers = await offerArtifacts(currentProjection, COMPLETE_DISCOVERY, currentEnvironment)
  if (offers.kind !== 'artifact-actions') throw new Error('expected an offered choice')
  const outcome = await reconcileArtifactChoice({
    choice: offers.primary.choice,
    preferredRoute: materializationRouteIdentity(offers.primary.route),
    expectedSelectionDigest: selection.digest,
    projection: currentProjection,
    discovery: COMPLETE_DISCOVERY,
    environment: currentEnvironment,
    previousObservation: {
      projectionEpoch: currentProjection.epoch,
      selectionDigest: selection.digest,
      resolvedArtifactDigest: null,
    },
  })
  if (outcome.kind !== 'resolved') throw new Error(`expected resolution, received ${outcome.kind}`)
  return outcome.action
}

function operationID(candidate: CandidateMaterializationBinding): string {
  switch (candidate.kind) {
    case 'destination-reservation': return candidate.reservation.operationId
    case 'workspace-binding': return candidate.workspace.operationId
    case 'portable-binding': return candidate.portable.operationId
  }
}
