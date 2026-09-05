import { describe, expect, it, vi } from 'vitest'

import type { CompatibleNameRepairSummary } from '../../src/output/file-system-access/compatible-name/model'
import type { RecoverySummary } from '../../src/output/file-system-access/recovery-summary'
import {
  WorkspaceCostObservationAccumulatorV1,
  materializationRouteIdentity,
  offerArtifacts,
  reconcileArtifactChoice,
  type ArtifactChoice,
  type ArtifactOffers,
  type EnvironmentOffers,
  type OfferedArtifactChoice,
  type ResolvedArtifactAction,
} from '../../src/output/planning'
import type { ReceiveLifecycleState } from '../../src/output/workspace'
import { PersistentPreservingWriterOpenError } from '../../src/output/persistent-tree/recovery'
import { createSelectionSpec, type ReceiveIntent } from '../../src/transfer/intent'
import { snapshotMaterializationRootRelativePath } from '../../src/transfer/job/coordinate/direct-tree'
import { PersistentWriterOpenPauseRequestedError } from '../../src/transfer/settlement/persistent-file-transaction'
import type { TransferWorkerSettlement } from '../../src/transfer/outcome'
import type { SelectionProjectionState } from '../../src/transfer/projection'
import type {
  V2AuthorityActivationSnapshot,
  V2LiveAuthorityActivationSnapshot,
} from '../../src/ui/controller/activation-model'
import {
  activationLocksSelection,
  presentArtifactOffers,
} from '../../src/ui/v2-artifact-presentation'
import { ActiveReceiveSettlementPresentation } from '../../src/ui/controller/active-receive-settlement-presentation'
import { V2OutputPresentationController } from '../../src/ui/v2-output'
import {
  COMPLETE_DISCOVERY,
  directZipTarget,
  environment,
  fsaTarget,
  handoffTarget,
  identity,
  managedTarget,
  portableOffer,
  projection,
  reviewedDirectZipSupport,
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
    expect(treePresentation.defaultChoices[0]).toMatchObject({
      operation: 'save-directory-tree',
      label: 'Save using original folder hierarchy',
      choice: { artifactKind: 'directory-tree' },
    })
    const zipRoutes = requireZipRoutes(treePresentation)
    expect(zipRoutes.primary).toMatchObject({
      label: 'Save ZIP after receiving completes',
      packageExplanation: expect.stringMatching(/Package only:.*without compression/u),
    })
    expect(zipRoutes.secondary).toBeNull()

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
    expect(download.defaultChoices[0]?.label).toBe('Download report.txt')
    expect(folder.defaultChoices[0]?.label).toBe('Save to folder')
    expect(checked.defaultChoices[0]?.label).toBe('Check then download')

    const userCopy = collectText([treePresentation, download, folder, checked])
    expect(userCopy).not.toMatch(/backend|OPFS|stream|admission|partial.?ZIP/iu)
  })

  it('presents stable ZIP identities without borrowing workspace precision for Direct ZIP', async () => {
    const selection = await selectionSpec()
    const cost = new WorkspaceCostObservationAccumulatorV1()
    cost.observe({ kind: 'directory', path: ['photos'] })
    cost.observe({ kind: 'file', path: ['photos', 'one.jpg'], exactSize: 1_024n })
    const workspaceCostObservation = cost.complete()
    const projected = {
      ...projection(selection, treeProof(), 1_024n),
      workspaceCostObservation,
    }
    const support = reviewedDirectZipSupport()
    const offers = await offerArtifacts(projected, COMPLETE_DISCOVERY, environment({
      targets: [fsaTarget(), directZipTarget(), handoffTarget()],
      workspace: workspaceOffer(),
      directZipSupport: support,
      zipRecommendationPolicy: {
        version: 1,
        kind: 'available',
        workspacePeakBytesThreshold: workspaceCostObservation.peakOwnedBytes,
        policyDigest: support.recommendationPolicyDigest,
      },
    }))
    const zip = requireZipRoutes(requireChoices(presentArtifactOffers(offers)))

    expect(zip.primary.choice.choiceId).toBe('RW0aXukzHVFiMjNEaoYb8qGKTN-AKAhw7u-Yi_-WsoQ')
    expect(zip.secondary?.choice.choiceId).toBe('0dkx9vDTzvH7B7a9EUoJBOWLCWgmVwLoFH3jjRmfHFU')
    expect(zip.primary.selectedBytes).toBe('Selected content: 1.0 KiB (exact)')
    expect(zip.primary.resultBytes).toMatch(/^ZIP package: .* \(exact\)$/u)
    expect(zip.secondary?.selectedBytes).toBe('Selected content: 1.0 KiB (exact)')
    expect(zip.secondary?.resultBytes)
      .toBe('ZIP package: 1.0 KiB (estimated lower bound)')
    expect(zip.recommendation).toContain('Recommended: receive completely first')
  })

  it('uses explicit lower-bound wording and a native fallback when no browser ZIP route is safe', async () => {
    const selection = await selectionSpec()
    const offers = await offerArtifacts(
      projection(selection, treeProof({ kind: 'unsettled' }), 1_024n),
      { kind: 'discovering' },
      environment({ targets: [fsaTarget()] }),
    )
    const presentation = requireChoices(presentArtifactOffers(offers))

    expect(presentation.defaultChoices[0]?.selectedBytes)
      .toBe('Selected content: 1.0 KiB (estimated lower bound)')
    expect(presentation.zipMode).toMatchObject({ kind: 'native-fallback' })
    expect(collectText(presentation.zipMode)).not.toMatch(/FSA|OPFS|backend/iu)
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

})

describe('receive interruption presentation', () => {
  it.each([
    { control: 'pause', foreground: 'Pausing', background: 'Pausing in the background' },
    { control: 'stop', foreground: 'Stopping', background: 'Stopping in the background' },
  ] as const)('presents $control without changing durable lifecycle truth', async ({
    control,
    foreground,
    background,
  }) => {
    vi.useFakeTimers()
    try {
      const observation = await singleFileObservation(19n)
      const outputs = new V2OutputPresentationController()
      outputs.updateProjection(1, observation.state, observation.offers)
      outputs.updateActivation(waitingResolution(
        observation.offered,
        observation.state.projection,
        1,
      ))
      const intent = receiveIntent(observation.action)
      const receiving = lifecycle(intent, 1n, {
        kind: 'receiving',
        activeLeaseId: 'lease',
      })
      expect(outputs.adoptReceiveIntent(
        observation.offered.choice,
        intent,
        receiving,
      )).toBe(true)
      const presentation = new ActiveReceiveSettlementPresentation({
        outputs,
        operationIsCurrent: () => true,
      })

      presentation.begin(control)
      expect(outputs.getSnapshot()).toMatchObject({
        lifecycle: { kind: 'receiving', generation: 1n },
        lifecyclePresentation: { title: foreground },
        receiveInterruption: { control, phase: 'waiting' },
      })

      let expired = false
      presentation.schedule(25, () => { expired = true })
      await vi.advanceTimersByTimeAsync(25)
      expect(expired).toBe(true)
      expect(outputs.getSnapshot()).toMatchObject({
        lifecycle: { kind: 'receiving', generation: 1n },
        lifecyclePresentation: { title: background, tone: 'warning' },
        receiveInterruption: { control, phase: 'background' },
      })

      const stable = lifecycle(intent, 2n, {
        kind: 'restart-required',
        reason: 'portable-aborted',
      })
      expect(outputs.updateLifecycle(stable)).toBe(true)
      expect(outputs.getSnapshot()).toMatchObject({
        lifecycle: { kind: 'restart-required', generation: 2n },
        lifecyclePresentation: { title: 'Start again required' },
        receiveInterruption: null,
      })
    } finally {
      vi.useRealTimers()
    }
  })
})

describe('derived output lifecycle and recovery presentation', () => {
  it('publishes the validated recovery summary from a live DirectTree pause result', async () => {
    const observation = await directTreeObservation(9n)
    const outputs = new V2OutputPresentationController()
    outputs.updateProjection(1, observation.state, observation.offers)
    outputs.updateActivation(waitingResolution(
      observation.offered,
      observation.state.projection,
      1,
    ))
    const intent = receiveIntent(observation.action)
    const checkpointSetDigest = identity(94, 32)
    const paused = lifecycle(intent, 2n, {
      kind: 'resumable-receive',
      payloadKind: 'file-set',
      checkpointSetDigest,
      completedFileCount: 2n,
      completedBytes: 1_024n,
      selectionFacts: Object.freeze({
        discoveredFileCount: 5n,
        discoveredBytes: 4_096n,
        discovery: 'complete',
      }),
    })
    const recoverySummary = recoverySummaryFixture(2n, checkpointSetDigest)
    expect(outputs.adoptReceiveIntent(
      observation.offered.choice,
      intent,
      paused,
    )).toBe(true)

    expect(outputs.adoptTransferResult({
      worker: pausedWorker(),
      lifecycle: paused,
      intent,
      transferJobId: 'paused-transfer-job',
      recoverySummary,
    })).toBe(true)
    expect(outputs.getSnapshot()).toMatchObject({
      recoverySummary,
      lifecyclePresentation: {
        description: expect.stringContaining('Completed: 2 files'),
        actions: [
          { kind: 'continue', label: 'Continue and preserve partial files' },
          { kind: 'redownload', label: 'Restart incomplete files' },
        ],
      },
    })
  })

  it('retains the exact preserving-open failure and modeled cost in the paused presentation', async () => {
    const observation = await directTreeObservation(10n)
    const outputs = new V2OutputPresentationController()
    outputs.updateProjection(1, observation.state, observation.offers)
    outputs.updateActivation(waitingResolution(
      observation.offered,
      observation.state.projection,
      1,
    ))
    const intent = receiveIntent(observation.action)
    const checkpointSetDigest = identity(95, 32)
    const paused = lifecycle(intent, 2n, {
      kind: 'resumable-receive',
      payloadKind: 'file-set',
      checkpointSetDigest,
      completedFileCount: 2n,
      completedBytes: 1_024n,
      selectionFacts: Object.freeze({
        discoveredFileCount: 5n,
        discoveredBytes: 4_096n,
        discovery: 'complete',
      }),
    })
    outputs.adoptReceiveIntent(observation.offered.choice, intent, paused)
    const abortReason = new PersistentWriterOpenPauseRequestedError(
      new PersistentPreservingWriterOpenError({
        materializationRelativePath: snapshotMaterializationRootRelativePath([
          'photos',
          'blocked.bin',
        ]),
        cost: {
          prefixCopyBytes: 128n * 1024n * 1024n,
          writeAmplificationBytes: 128n * 1024n * 1024n,
          temporaryBytes: 96n * 1024n * 1024n,
        },
        purpose: 'automatic-checkpoint',
        cause: new DOMException('Native writer open failed', 'QuotaExceededError'),
      }),
    )

    expect(outputs.adoptTransferResult({
      worker: pausedWorker(),
      lifecycle: paused,
      intent,
      transferJobId: 'paused-transfer-job',
      recoverySummary: recoverySummaryFixture(2n, checkpointSetDigest),
      abortReason,
    })).toBe(true)
    expect(outputs.getSnapshot()).toMatchObject({
      writerOpenPause: {
        materializationRelativePath: ['photos', 'blocked.bin'],
        purpose: 'automatic-checkpoint',
        cost: {
          prefixCopyBytes: 134_217_728n,
          writeAmplificationBytes: 134_217_728n,
          temporaryBytes: 100_663_296n,
        },
      },
      lifecyclePresentation: {
        writerOpenPause: {
          title: 'Could not reopen photos/blocked.bin',
          description: expect.stringMatching(/128\.0 MiB.*128\.0 MiB.*96\.0 MiB/u),
        },
      },
    })
  })

  it('keeps compatible-name repair separate, persistent, monotonic, and terminally qualified', async () => {
    const observation = await singleFileObservation(7n)
    const outputs = new V2OutputPresentationController()
    outputs.updateProjection(1, observation.state, observation.offers)
    outputs.updateActivation(waitingResolution(
      observation.offered,
      observation.state.projection,
      1,
    ))
    const intent = receiveIntent(observation.action)
    const receiving = lifecycle(intent, 1n, {
      kind: 'receiving',
      activeLeaseId: 'lease',
    })
    expect(outputs.adoptReceiveIntent(
      observation.offered.choice,
      intent,
      receiving,
    )).toBe(true)

    const firstReplacement = repairSummary(0, [], 'active')
    expect(outputs.updateRepairSummary('another-operation', firstReplacement)).toBe(false)
    expect(outputs.updateRepairSummary(intent.operationId, firstReplacement, 1_000)).toBe(true)
    expect(outputs.getSnapshot().lifecyclePresentation?.compatibleNameRepair).toMatchObject({
      replacementCount: 0,
      actionMode: 'receiving-notice',
    })

    const committed = repairSummary(2, [
      ['folder', 'pyvenv.cfg'],
      ['folder', 'nested'],
    ], 'active')
    const receivingUpdate = lifecycle(intent, 2n, {
      kind: 'receiving',
      activeLeaseId: 'lease',
    })
    expect(outputs.updateLifecycle(receivingUpdate, 2_000, null, [], committed)).toBe(true)
    expect(outputs.getSnapshot().repairSummary?.committedCount).toBe(2)
    expect(outputs.getSnapshot().lifecyclePresentation?.compatibleNameRepair)
      .toMatchObject({ replacementCount: 2 })
    expect(() => outputs.updateRepairSummary(intent.operationId, firstReplacement))
      .toThrow(/count cannot move backward/u)

    const terminalRepair = repairSummary(2, committed.logicalPathSample, 'completed')
    const published = lifecycle(intent, 3n, {
      kind: 'published',
      receiptDigest: identity(92, 32),
      cleanupState: 'clean',
    })
    expect(outputs.updateLifecycle(published, 3_000, null, [], terminalRepair)).toBe(true)
    expect(outputs.getSnapshot()).toMatchObject({
      lifecycle: { kind: 'published' },
      lifecyclePresentation: {
        title: 'Completed with compatible names',
        compatibleNameRepair: { actionMode: 'routine-restoration' },
      },
    })
    expect(outputs.adoptTransferResult({
      worker: successfulWorker(),
      lifecycle: published,
      intent,
      transferJobId: 'transfer-job',
      repairSummary: terminalRepair,
    })).toBe(true)
    expect(outputs.getSnapshot().transferResultPresentation?.title)
      .toBe('Completed with compatible names')

    outputs.reset()
    expect(outputs.getSnapshot().repairSummary).toBeNull()
  })

  it('treats an absent terminal repair summary as verified empty-cleanup authority', async () => {
    const observation = await singleFileObservation(8n)
    const outputs = new V2OutputPresentationController()
    outputs.updateProjection(1, observation.state, observation.offers)
    outputs.updateActivation(waitingResolution(
      observation.offered,
      observation.state.projection,
      1,
    ))
    const intent = receiveIntent(observation.action)
    const receiving = lifecycle(intent, 1n, {
      kind: 'receiving',
      activeLeaseId: 'lease',
    })
    expect(outputs.adoptReceiveIntent(
      observation.offered.choice,
      intent,
      receiving,
    )).toBe(true)
    expect(outputs.updateRepairSummary(
      intent.operationId,
      repairSummary(0, [], 'active'),
    )).toBe(true)
    expect(outputs.getSnapshot().lifecyclePresentation?.compatibleNameRepair)
      .toMatchObject({ replacementCount: 0 })

    const published = lifecycle(intent, 2n, {
      kind: 'published',
      receiptDigest: identity(93, 32),
      cleanupState: 'clean',
    })
    expect(outputs.updateLifecycle(published, 2_000, null, [], null)).toBe(true)
    expect(outputs.getSnapshot()).toMatchObject({
      repairSummary: null,
      lifecyclePresentation: {
        title: 'Saved',
        compatibleNameRepair: null,
      },
    })
    expect(outputs.adoptTransferResult({
      worker: successfulWorker(),
      lifecycle: published,
      intent,
      transferJobId: 'transfer-job',
    })).toBe(true)
    expect(outputs.getSnapshot()).toMatchObject({
      repairSummary: null,
      transferResultPresentation: { title: 'Transfer completed' },
    })
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

async function directTreeObservation(epoch: bigint): Promise<Readonly<{
  state: SelectionProjectionState
  offers: ArtifactOffers
  offered: OfferedArtifactChoice
  action: ResolvedArtifactAction
}>> {
  const selection = await selectionSpec()
  const projected = projection(selection, treeProof(), 4_096n, epoch)
  const outputEnvironment = environment({ targets: [fsaTarget()] })
  const offers = await offerArtifacts(projected, COMPLETE_DISCOVERY, outputEnvironment)
  if (offers.kind !== 'artifact-actions') {
    throw new Error(`expected artifact choices, received ${offers.kind}`)
  }
  const offered = [offers.primary, ...offers.alternatives]
    .find(candidate => candidate.route.kind === 'direct-tree')
  if (offered === undefined) throw new Error('expected DirectTree artifact choice')
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
    preClickRanking: Object.freeze([offered.choice.choiceId]),
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
    preClickRanking: Object.freeze([offered.choice.choiceId]),
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

function lifecycle(
  intent: ReceiveIntent,
  generation: bigint,
  payload: Readonly<Record<string, unknown>>,
): ReceiveLifecycleState {
  return Object.freeze({
    ...payload,
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    generation,
  }) as ReceiveLifecycleState
}

function repairSummary(
  committedCount: number,
  logicalPathSample: readonly (readonly string[])[],
  footerState: NonNullable<CompatibleNameRepairSummary['latestObservedFooter']>['state'],
  sidecarPending = false,
): CompatibleNameRepairSummary {
  const terminalSettlement = sidecarPending ? 'pending' : 'complete'
  return Object.freeze({
    committedCount,
    logicalPathSample: Object.freeze(logicalPathSample.map(path => Object.freeze([...path]))),
    pairDisplayNames: Object.freeze({
      script: 'restore.windshare-abc234.ps1',
      sidecar: 'restore.windshare-abc234.data',
    }),
    placement: 'inside-logical-root',
    latestObservedFooter: Object.freeze({ committedCount, state: footerState }),
    sidecarSync: sidecarPending ? 'pending' : 'current',
    terminalSettlement: footerState === 'active' ? 'none' : terminalSettlement,
  })
}

function successfulWorker(): TransferWorkerSettlement {
  return Object.freeze({
    status: 'Succeeded',
    failures: Object.freeze([]),
    failureCount: 0,
    fileFailureCount: 0,
    omittedFailureCount: 0,
    fileOutcomes: Object.freeze({
      sourceDriftFiles: 0,
      revisionConflictFiles: 0,
      checkpointInvalidFiles: 0,
      ownedObjectUnknownFiles: 0,
      collisionFiles: 0,
      failedFiles: 0,
    }),
  })
}

function pausedWorker(): TransferWorkerSettlement {
  return Object.freeze({ ...successfulWorker(), status: 'Paused' })
}

function recoverySummaryFixture(
  lifecycleGeneration: bigint,
  checkpointSetDigest: string,
): RecoverySummary {
  return Object.freeze({
    lifecycleGeneration,
    checkpointSetDigest,
    discoveredFileCount: 5n,
    discoveredBytes: 4_096n,
    discovery: 'complete',
    completedFileCount: 2n,
    completedBytes: 1_024n,
    incompleteFileCount: 2n,
    verifiedPartialFileCount: 2n,
    verifiedPartialBytes: 512n,
    unstartedFileCount: 1n,
    unstartedBytes: 1_024n,
    preservingRemainingBytes: 2_560n,
    restartRemainingBytes: 3_072n,
    restartRedownloadBytes: 512n,
    maximumPreservingTemporaryBytes: 384n,
  })
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

function requireZipRoutes(value: ReturnType<typeof requireChoices>) {
  if (value.zipMode?.kind !== 'routes') throw new Error('expected ZIP routes')
  return value.zipMode
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
