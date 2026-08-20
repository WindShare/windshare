import { describe, expect, it, vi } from 'vitest'

import {
  materializationRouteIdentity,
  offerArtifacts,
  reconcileArtifactChoice,
  type ArtifactChoice,
  type ArtifactOffers,
  type EnvironmentOffers,
  type OfferedArtifactChoice,
  type ResolvedArtifactAction,
} from '../../src/output/planning'
import { createSelectionSpec, type ReceiveIntent } from '../../src/transfer/intent'
import type { SelectionProjectionState } from '../../src/transfer/projection'
import type {
  V2AuthorityActivationSnapshot,
  V2LiveAuthorityActivationSnapshot,
} from '../../src/ui/controller/activation-model'
import {
  activationLocksSelection,
  presentArtifactOffers,
} from '../../src/ui/v2-artifact-presentation'
import { V2OutputPresentationController } from '../../src/ui/v2-output'
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
  treeProof,
  workspaceOffer,
} from '../output/planning/fixture'

describe('artifact product presentation', () => {
  it('keeps confirmation noninteractive and labels offered choices without backend details', async () => {
    const selection = await selectionSpec()
    const unknown = projection(selection, { kind: 'unknown' }, 0n, 1n)
    const confirming = await offerArtifacts(
      unknown,
      { kind: 'discovering' },
      environment({ targets: [handoffTarget()], portable: portableOffer() }),
    )
    expect(presentArtifactOffers(confirming)).toMatchObject({
      kind: 'status',
      interactive: false,
      title: 'Confirming selected content…',
    })

    const treeOffers = await offerArtifacts(
      projection(selection, treeProof(), 1_024n),
      COMPLETE_DISCOVERY,
      environment({ targets: [fsaTarget(), handoffTarget()], workspace: workspaceOffer() }),
    )
    const treePresentation = requireChoices(presentArtifactOffers(treeOffers))
    expect(treePresentation.primary).toMatchObject({
      operation: 'save-directory-tree',
      label: 'Save using original folder hierarchy',
      choice: { artifactKind: 'directory-tree' },
    })
    expect(treePresentation.alternatives[0]).toMatchObject({
      label: 'Download photos.zip',
      packageExplanation: expect.stringMatching(/one ZIP package without compression/u),
    })

    const single = projection(selection, singleFileProof(), 128n)
    const download = requireChoices(presentArtifactOffers(await offerArtifacts(
      single,
      COMPLETE_DISCOVERY,
      environment({ targets: [managedTarget()] }),
    )))
    const folder = requireChoices(presentArtifactOffers(await offerArtifacts(
      single,
      COMPLETE_DISCOVERY,
      environment({ targets: [fsaTarget()] }),
    )))
    const checked = requireChoices(presentArtifactOffers(await offerArtifacts(
      single,
      COMPLETE_DISCOVERY,
      environment({ targets: [handoffTarget()], portable: portableOffer() }),
    )))
    expect(download.primary.label).toBe('Download report.txt')
    expect(folder.primary.label).toBe('Save to folder')
    expect(checked.primary.label).toBe('Check then download')

    const userCopy = collectText([treePresentation, download, folder, checked])
    expect(userCopy).not.toMatch(/backend|OPFS|stream|admission|partial.?ZIP/iu)
  })
})

describe('derived output presentation', () => {
  it('applies only increasing owner revisions and never runs planning itself', async () => {
    const first = await singleFileObservation(1n)
    const second = await singleFileObservation(2n)
    const outputs = new V2OutputPresentationController()
    const listener = vi.fn()
    outputs.subscribe(listener)

    expect(outputs.updateProjection(2, second.state, second.offers)).toBe(true)
    expect(outputs.updateProjection(1, first.state, first.offers)).toBe(false)

    expect(outputs.getSnapshot()).toMatchObject({
      projectionRevision: 2,
      projection: { projection: { epoch: 2n } },
      offers: { projectionEpoch: 2n },
      offerPresentation: { kind: 'choices' },
    })
    expect(listener).toHaveBeenCalledOnce()
  })

  it('keeps the coordinator-owned choice visible through replacement observations', async () => {
    const selection = await selectionSpec()
    const firstProjection = projection(selection, treeProof({ kind: 'unsettled' }), 128n, 1n)
    const firstEnvironment = environment({ targets: [fsaTarget()] })
    const firstOffers = await offerArtifacts(
      firstProjection,
      { kind: 'discovering' },
      firstEnvironment,
    )
    const offered = requireOfferedChoice(firstOffers)
    const outputs = new V2OutputPresentationController()
    outputs.updateProjection(
      1,
      state(firstProjection, { kind: 'discovering' }),
      firstOffers,
    )
    outputs.updateActivation(waitingResolution(offered, firstProjection, 1))

    const replacementProjection = projection(selection, { kind: 'unknown' }, 0n, 2n)
    const replacementOffers = await offerArtifacts(
      replacementProjection,
      { kind: 'discovering' },
      firstEnvironment,
    )
    outputs.updateProjection(
      2,
      state(replacementProjection, { kind: 'discovering' }),
      replacementOffers,
    )
    outputs.updateActivation(Object.freeze({
      ...waitingResolution(offered, firstProjection, 1),
      observation: Object.freeze({
        revision: 2,
        protocolSessionId: 'protocol-session-2',
        projectionEpoch: replacementProjection.epoch,
      }),
    }))

    expect(outputs.getSnapshot()).toMatchObject({
      projectionRevision: 2,
      offerPresentation: { kind: 'status' },
      chosenChoice: { operation: 'save-directory-tree' },
      activationPresentation: {
        kind: 'waiting',
        choice: {
          operation: 'save-directory-tree',
          label: 'Save using original folder hierarchy',
        },
      },
    })
  })

  it('derives waiting, retry, commit, and lock state from coordinator snapshots', async () => {
    const observation = await singleFileObservation(4n)
    const outputs = new V2OutputPresentationController()
    outputs.updateProjection(1, observation.state, observation.offers)

    const waiting: V2AuthorityActivationSnapshot = Object.freeze({
      ...liveActivation(observation.offered, observation.action, 1),
      kind: 'waiting-authority',
      resolution: Object.freeze({ kind: 'resolved', action: observation.action }),
    })
    outputs.updateActivation(waiting)
    expect(outputs.getSnapshot().activationPresentation).toMatchObject({
      kind: 'waiting',
      title: 'Waiting for the save location…',
    })
    expect(activationLocksSelection(outputs.getSnapshot().activation)).toBe(true)
    expect(outputs.getSnapshot().resolvedArtifact?.digest).toBe(observation.action.artifact.digest)

    outputs.updateActivation(Object.freeze({
      ...liveActivation(observation.offered, observation.action, 2),
      kind: 'retry-required',
      authorityReady: true,
      reason: 'catalog-temporarily-unavailable',
    }))
    expect(outputs.getSnapshot().activationPresentation).toMatchObject({
      kind: 'retry',
      label: 'Retry confirmation',
      choice: { operation: 'download-original' },
    })

    outputs.updateActivation(Object.freeze({
      ...liveActivation(observation.offered, observation.action, 3),
      kind: 'committing',
      action: observation.action,
    }))
    expect(outputs.getSnapshot().activationPresentation).toMatchObject({
      kind: 'committing',
      choice: { operation: 'download-original' },
    })

    outputs.updateActivation(Object.freeze({
      ...liveActivation(observation.offered, observation.action, 4),
      kind: 'cleanup-required',
      operationId: identity(71),
      ownerKind: 'owned-effects',
      failedStage: 'detach',
      settlementComplete: true,
      detachComplete: false,
    }))
    expect(outputs.getSnapshot().activationPresentation).toMatchObject({
      kind: 'retry',
      title: 'Output cleanup needs attention.',
      choice: { operation: 'download-original' },
    })
    expect(activationLocksSelection(outputs.getSnapshot().activation)).toBe(true)

    outputs.updateActivation(Object.freeze({
      ...liveActivation(observation.offered, observation.action, 5),
      kind: 'terminal',
      outcome: Object.freeze({ kind: 'picker-refused' }),
    }))
    expect(outputs.getSnapshot().activationPresentation).toBeNull()
    expect(activationLocksSelection(outputs.getSnapshot().activation)).toBe(false)
    expect(outputs.getSnapshot().offerPresentation?.interactive).toBe(true)

    const boundOutputs = new V2OutputPresentationController()
    boundOutputs.updateProjection(1, observation.state, observation.offers)
    boundOutputs.updateActivation(Object.freeze({
      ...liveActivation(observation.offered, observation.action, 5),
      kind: 'committing',
      action: observation.action,
    }))
    boundOutputs.updateActivation(Object.freeze({
      ...liveActivation(observation.offered, observation.action, 6),
      kind: 'terminal',
      outcome: Object.freeze({ kind: 'bound-operation', operationId: identity(72) }),
    }))
    expect(activationLocksSelection(boundOutputs.getSnapshot().activation)).toBe(true)
    expect(boundOutputs.getSnapshot().resolvedArtifact?.digest)
      .toBe(observation.action.artifact.digest)
  })

  it('adopts a matching coordinator choice across epoch and session replacement', async () => {
    const observation = await singleFileObservation(5n)
    const outputs = new V2OutputPresentationController()
    outputs.updateProjection(1, observation.state, observation.offers)
    outputs.updateActivation(waitingResolution(
      observation.offered,
      observation.state.projection,
      1,
    ))

    const replacement = await singleFileObservation(6n)
    outputs.updateProjection(2, replacement.state, replacement.offers)
    outputs.updateActivation(Object.freeze({
      ...liveActivation(observation.offered, replacement.action, 2),
      kind: 'committing',
      action: replacement.action,
      observation: Object.freeze({
        revision: 2,
        protocolSessionId: 'replacement-session',
        projectionEpoch: replacement.state.projection.epoch,
      }),
    }))

    const intent = receiveIntent(observation.action)
    expect(outputs.adoptReceiveIntent(observation.offered.choice, intent)).toBe(true)
    expect(outputs.getSnapshot()).toMatchObject({
      receiveIntent: { operationId: intent.operationId },
      resolvedArtifact: { digest: observation.action.artifact.digest },
      plan: { kind: observation.offered.choice.plan.kind },
    })

    const differentChoice: ArtifactChoice = Object.freeze({
      ...observation.offered.choice,
      operation: 'check-then-download',
    })
    expect(outputs.adoptReceiveIntent(differentChoice, intent)).toBe(false)

    const mismatchedIntent = Object.freeze({
      ...intent,
      artifact: Object.freeze({ ...intent.artifact, kind: 'zip-archive' }),
    }) as ReceiveIntent
    expect(() => outputs.adoptReceiveIntent(observation.offered.choice, mismatchedIntent))
      .toThrow(/does not match the coordinator-owned artifact choice/u)

    const mismatchedActionIntent = Object.freeze({
      ...intent,
      artifact: Object.freeze({ ...intent.artifact, digest: identity(91, 32) }),
    }) as ReceiveIntent
    expect(() => outputs.adoptReceiveIntent(observation.offered.choice, mismatchedActionIntent))
      .toThrow(/does not match the coordinator-owned resolved action/u)
  })

  it('publishes a receive intent only after its prepared owner is installed', async () => {
    const observation = await singleFileObservation(7n)
    const outputs = new V2OutputPresentationController()
    outputs.updateProjection(1, observation.state, observation.offers)
    outputs.updateActivation(Object.freeze({
      ...liveActivation(observation.offered, observation.action, 1),
      kind: 'committing',
      action: observation.action,
    }))
    const intent = receiveIntent(observation.action)
    let owned = false
    const observedOwnership: boolean[] = []
    outputs.subscribe(() => {
      if (outputs.getSnapshot().receiveIntent !== null) observedOwnership.push(owned)
    })

    expect(outputs.adoptReceiveIntentAtomically(
      observation.offered.choice,
      intent,
      () => { owned = true },
    )).toBe(true)
    expect(observedOwnership).toEqual([true])
  })

  it('rejects mismatched projection facts without mutating visible state', async () => {
    const observation = await singleFileObservation(8n)
    const outputs = new V2OutputPresentationController()
    const mismatched = Object.freeze({
      ...observation.offers,
      selectionDigest: identity(90, 32),
    }) as ArtifactOffers

    expect(() => outputs.updateProjection(1, observation.state, mismatched))
      .toThrow(/do not belong to the supplied projection observation/u)
    expect(outputs.getSnapshot()).toBe(outputs.getSnapshot())
    expect(outputs.getSnapshot().projection).toBeNull()
  })
})

async function singleFileObservation(epoch: bigint): Promise<Readonly<{
  state: SelectionProjectionState
  offers: ArtifactOffers
  environment: EnvironmentOffers
  offered: OfferedArtifactChoice
  action: ResolvedArtifactAction
}>> {
  const selection = await selectionSpec()
  const projected = projection(selection, singleFileProof(), 128n, epoch)
  const outputEnvironment = environment({ targets: [managedTarget()] })
  const offers = await offerArtifacts(projected, COMPLETE_DISCOVERY, outputEnvironment)
  const offered = requireOfferedChoice(offers)
  const reconciled = await reconcileArtifactChoice({
    choice: offered.choice,
    preferredRoute: materializationRouteIdentity(offered.route),
    expectedSelectionDigest: projected.selectionDigest,
    projection: projected,
    discovery: COMPLETE_DISCOVERY,
    environment: outputEnvironment,
    previousObservation: null,
  })
  if (reconciled.kind !== 'resolved') {
    throw new Error(`expected resolved action, received ${reconciled.kind}`)
  }
  return Object.freeze({
    state: state(projected, COMPLETE_DISCOVERY),
    offers,
    environment: outputEnvironment,
    offered,
    action: reconciled.action,
  })
}

function waitingResolution(
  offered: OfferedArtifactChoice,
  projected: SelectionProjectionState['projection'],
  revision: number,
): V2AuthorityActivationSnapshot {
  return Object.freeze({
    activationId: 'activation-1',
    authenticatedShareInstanceId: identity(70),
    selectionDigest: projected.selectionDigest,
    choice: offered.choice,
    installedRoute: materializationRouteIdentity(offered.route),
    observation: Object.freeze({
      revision,
      protocolSessionId: `protocol-session-${revision}`,
      projectionEpoch: projected.epoch,
    }),
    kind: 'waiting-resolution',
  })
}

function liveActivation(
  offered: OfferedArtifactChoice,
  action: ResolvedArtifactAction,
  revision: number,
): V2LiveAuthorityActivationSnapshot {
  return Object.freeze({
    activationId: 'activation-1',
    authenticatedShareInstanceId: identity(70),
    selectionDigest: action.selectionDigest,
    choice: offered.choice,
    installedRoute: materializationRouteIdentity(offered.route),
    observation: Object.freeze({
      revision,
      protocolSessionId: `protocol-session-${revision}`,
      projectionEpoch: action.projectionEpoch,
    }),
  })
}

function receiveIntent(action: ResolvedArtifactAction): ReceiveIntent {
  return Object.freeze({
    operationId: identity(80),
    digest: identity(81, 32),
    artifact: action.artifact,
    plan: Object.freeze({ kind: action.choice.plan.kind }),
  }) as ReceiveIntent
}

async function selectionSpec() {
  return createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
}

function state(
  value: SelectionProjectionState['projection'],
  discovery: SelectionProjectionState['discovery'],
): SelectionProjectionState {
  return Object.freeze({ projection: value, discovery })
}

function requireChoices(value: ReturnType<typeof presentArtifactOffers>) {
  if (value.kind !== 'choices') throw new Error(`expected choices, received ${value.kind}`)
  return value
}

function requireOfferedChoice(offers: ArtifactOffers): OfferedArtifactChoice {
  if (offers.kind !== 'artifact-actions') {
    throw new Error(`expected artifact choices, received ${offers.kind}`)
  }
  return offers.primary
}

function collectText(value: unknown, seen = new Set<object>()): string {
  if (typeof value === 'string') return value
  if (typeof value !== 'object' || value === null || seen.has(value)) return ''
  seen.add(value)
  return Object.values(value).map((nested) => collectText(nested, seen)).join(' ')
}
