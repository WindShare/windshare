import { describe, expect, it, vi } from 'vitest'

import {
  offerArtifacts,
  type ArtifactOffers,
} from '../../src/output/planning'
import { fileId } from '../../src/catalog/model'
import { FaultScope, SourceFaultCode, sourceFault } from '../../src/transfer/fault'
import { createSelectionSpec, type ReceiveIntent } from '../../src/transfer/intent'
import { normalizedV2FileTransferFault } from '../../src/transfer/job/failures'
import { TransferFailureAccumulator, transferWorkerSettlement } from '../../src/transfer/outcome'
import {
  nextProjectionEpoch,
  type SelectionProjectionState,
} from '../../src/transfer/projection'
import {
  presentArtifactOffers,
} from '../../src/ui/v2-artifact-presentation'
import {
  V2OutputPresentationController,
  type ArtifactOfferPlanner,
} from '../../src/ui/v2-output'
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
  it('keeps confirming noninteractive and exposes retry without an artifact callback', async () => {
    const selection = await selectionSpec()
    const unknown = projection(selection, { kind: 'unknown' }, 0n, 1n)
    const offers = environment({ targets: [handoffTarget()], portable: portableOffer() })
    const controller = new V2OutputPresentationController()
    const authority = vi.fn()

    await controller.updateProjection(state(unknown, { kind: 'discovering' }), offers)
    expect(controller.getSnapshot().offerPresentation).toMatchObject({
      kind: 'status',
      interactive: false,
      title: 'Confirming selected content…',
    })
    await expect(controller.activateArtifact('check-then-download', authority))
      .resolves.toEqual({ kind: 'unavailable' })

    await controller.updateProjection(state(unknown, {
      kind: 'retryable-failure',
      reason: 'catalog-temporarily-unavailable',
    }), offers)
    expect(controller.getSnapshot().offerPresentation).toMatchObject({
      kind: 'retry',
      label: 'Retry confirmation',
    })
    const events: string[] = []
    await controller.retryConfirmation(() => { events.push('retry-started') })
    expect(events).toEqual(['retry-started'])
    expect(authority).not.toHaveBeenCalled()
  })

  it('labels every final artifact action and explains ZIP as one uncompressed complete package', async () => {
    const selection = await selectionSpec()
    const treeOffers = await offerArtifacts(
      projection(selection, treeProof(), 1_024n),
      COMPLETE_DISCOVERY,
      environment({ targets: [fsaTarget(), handoffTarget()], workspace: workspaceOffer() }),
    )
    const treePresentation = presentArtifactOffers(treeOffers)
    if (treePresentation.kind !== 'actions') throw new Error('expected tree actions')

    expect(treePresentation.primary.label).toBe('Save using original folder hierarchy')
    expect(treePresentation.alternatives[0]).toMatchObject({
      label: 'Download photos.zip',
      packageExplanation: expect.stringMatching(/one ZIP package without compression/u),
    })

    const singleProjection = projection(selection, singleFileProof(), 128n)
    const download = presentArtifactOffers(await offerArtifacts(
      singleProjection,
      COMPLETE_DISCOVERY,
      environment({ targets: [managedTarget()] }),
    ))
    const folder = presentArtifactOffers(await offerArtifacts(
      singleProjection,
      COMPLETE_DISCOVERY,
      environment({ targets: [fsaTarget()] }),
    ))
    const checked = presentArtifactOffers(await offerArtifacts(
      singleProjection,
      COMPLETE_DISCOVERY,
      environment({ targets: [handoffTarget()], portable: portableOffer() }),
    ))

    expect(requireActions(download).primary.label).toBe('Download report.txt')
    expect(requireActions(folder).primary.label).toBe('Save to folder')
    expect(requireActions(checked).primary.label).toBe('Check then download')

    const userCopy = collectText([treePresentation, download, folder, checked])
    expect(userCopy).not.toMatch(/backend|OPFS|stream|admission|partial.?ZIP/iu)
  })
})

describe('artifact action activation boundary', () => {
  it('starts authority synchronously in the final action click stack', async () => {
    const selection = await selectionSpec()
    const controller = new V2OutputPresentationController<string>()
    await controller.updateProjection(
      state(projection(selection, singleFileProof(), 128n), COMPLETE_DISCOVERY),
      environment({ targets: [managedTarget()] }),
    )
    let inClickStack = true
    const observations: boolean[] = []

    const activation = controller.activateArtifact('download-original', () => {
      observations.push(inClickStack)
      return 'authority'
    })
    inClickStack = false

    expect(observations).toEqual([true])
    await expect(activation).resolves.toMatchObject({
      kind: 'acquired',
      authority: 'authority',
      action: { operation: 'download-original' },
    })
  })

  it('restores the offered action when authority acquisition is cancelled', async () => {
    const selection = await selectionSpec()
    const controller = new V2OutputPresentationController()
    await controller.updateProjection(
      state(projection(selection, singleFileProof(), 128n), COMPLETE_DISCOVERY),
      environment({ targets: [managedTarget()] }),
    )
    const cancelled = deferred<never>()

    const activation = controller.activateArtifact('download-original', () => cancelled.promise)
    expect(controller.getSnapshot().chosenAction?.operation).toBe('download-original')
    cancelled.reject(new DOMException('cancelled', 'AbortError'))

    await expect(activation).rejects.toMatchObject({ name: 'AbortError' })
    expect(controller.getSnapshot().chosenAction).toBeNull()
    expect(controller.getSnapshot().offerPresentation?.kind).toBe('actions')
  })

  it('treats asynchronous projection as data only and never starts authority on completion', async () => {
    const selection = await selectionSpec()
    const projected = projection(selection, singleFileProof(), 128n)
    const expected = await offerArtifacts(
      projected,
      COMPLETE_DISCOVERY,
      environment({ targets: [managedTarget()] }),
    )
    const pending = deferred<ArtifactOffers>()
    const planner = vi.fn(() => pending.promise)
    const authority = vi.fn()
    const controller = new V2OutputPresentationController({ planner })

    const update = controller.updateProjection(
      state(projected, COMPLETE_DISCOVERY),
      environment({ targets: [managedTarget()] }),
    )
    pending.resolve(expected)
    await update

    expect(planner).toHaveBeenCalledOnce()
    expect(authority).not.toHaveBeenCalled()
    expect(controller.getSnapshot().offerPresentation?.kind).toBe('actions')
  })

  it('drops stale offer and authority completions across projection epochs', async () => {
    const selection = await selectionSpec()
    const firstProjection = projection(selection, singleFileProof(), 128n, 1n)
    const secondProjection = projection(selection, treeProof(), 256n, 2n)
    const firstEnvironment = environment({ targets: [managedTarget()] })
    const secondEnvironment = environment({ targets: [fsaTarget()] })
    const firstOffers = await offerArtifacts(firstProjection, COMPLETE_DISCOVERY, firstEnvironment)
    const secondOffers = await offerArtifacts(secondProjection, COMPLETE_DISCOVERY, secondEnvironment)
    const firstPlan = deferred<ArtifactOffers>()
    const secondPlan = deferred<ArtifactOffers>()
    const thirdPlan = deferred<ArtifactOffers>()
    const planned = [firstPlan, secondPlan, thirdPlan]
    const planner: ArtifactOfferPlanner = vi.fn(() => {
      const next = planned.shift()
      if (next === undefined) throw new Error('unexpected planning request')
      return next.promise
    })
    const release = vi.fn()
    const traces: unknown[] = []
    const controller = new V2OutputPresentationController<string>({
      planner,
      releaseStaleAuthority: release,
      trace: Object.freeze({
        get current() {
          return (event: unknown) => traces.push(event)
        },
      }),
    })

    const firstUpdate = controller.updateProjection(
      state(firstProjection, COMPLETE_DISCOVERY),
      firstEnvironment,
    )
    const secondUpdate = controller.updateProjection(
      state(secondProjection, COMPLETE_DISCOVERY),
      secondEnvironment,
    )
    secondPlan.resolve(secondOffers)
    await secondUpdate
    firstPlan.resolve(firstOffers)
    await expect(firstUpdate).resolves.toMatchObject({ kind: 'stale', projectionEpoch: 1n })
    expect(controller.getSnapshot().projection?.projection.epoch).toBe(2n)
    expect(controller.getSnapshot().offerPresentation).toMatchObject({
      kind: 'actions',
      primary: { operation: 'save-directory-tree' },
    })

    const authority = deferred<string>()
    const activation = controller.activateArtifact('save-directory-tree', () => authority.promise)
    const thirdProjection = projection(selection, singleFileProof(), 64n, 3n)
    const thirdUpdate = controller.updateProjection(
      state(thirdProjection, COMPLETE_DISCOVERY),
      firstEnvironment,
    )
    thirdPlan.resolve(await offerArtifacts(thirdProjection, COMPLETE_DISCOVERY, firstEnvironment))
    await thirdUpdate
    authority.resolve('stale-authority')
    await expect(activation).resolves.toMatchObject({ kind: 'stale', projectionEpoch: 2n })
    expect(release).toHaveBeenCalledWith('stale-authority')
    expect(traces).toContainEqual(expect.objectContaining({
      name: 'authority_transition',
      transition: 'stale_event_dropped',
      staleProjectionEpoch: 2n,
      currentProjectionEpoch: 3n,
      eventClass: 'authority_result',
    }))
  })

  it('rejects a bound intent from an older epoch before lifecycle state can be published', async () => {
    const selection = await selectionSpec()
    const projected = projection(selection, singleFileProof(), 128n, 4n)
    const controller = new V2OutputPresentationController<string>()
    await controller.updateProjection(
      state(projected, COMPLETE_DISCOVERY),
      environment({ targets: [managedTarget()] }),
    )
    await controller.activateArtifact('download-original', () => 'authority')
    const action = controller.getSnapshot().chosenAction
    if (action?.artifact === null || action?.artifact === undefined) throw new Error('missing artifact')
    const intent = {
      operationId: identity(80),
      digest: identity(81, 32),
      artifact: action.artifact,
      plan: { kind: action.plan.kind },
    } as ReceiveIntent

    const staleEpoch = projection(selection, singleFileProof(), 1n, 3n).epoch
    expect(controller.adoptReceiveIntent(staleEpoch, intent)).toBe(false)
    expect(controller.getSnapshot().receiveIntent).toBeNull()
  })

  it('fences exact transfer-result presentation by projection epoch and receive identity', async () => {
    const selection = await selectionSpec()
    const projected = projection(selection, singleFileProof(), 128n, 4n)
    const controller = new V2OutputPresentationController<string>()
    await controller.updateProjection(
      state(projected, COMPLETE_DISCOVERY),
      environment({ targets: [managedTarget()] }),
    )
    await controller.activateArtifact('download-original', () => 'authority')
    const action = controller.getSnapshot().chosenAction
    if (action?.artifact === null || action?.artifact === undefined) throw new Error('missing artifact')
    const intent = {
      operationId: identity(82),
      digest: identity(83, 32),
      artifact: action.artifact,
      plan: { kind: action.plan.kind },
    } as ReceiveIntent
    expect(controller.adoptReceiveIntent(projected.epoch, intent)).toBe(true)

    const failures = new TransferFailureAccumulator()
    failures.record({
      kind: 'file',
      fileId: fileId(identity(84)),
      classification: normalizedV2FileTransferFault(
        sourceFault(FaultScope.FileLocal, SourceFaultCode.Unavailable),
      ).diagnostic.classification,
    }, 1n, 'revision-conflict')
    const result = {
      worker: transferWorkerSettlement('CompletedWithErrors', failures.snapshot()),
      intent,
      transferJobId: 'job-1',
    }
    expect(controller.adoptTransferResult(nextProjectionEpoch(2n), result)).toBe(false)
    expect(controller.getSnapshot().transferResultPresentation).toBeNull()
    expect(controller.adoptTransferResult(projected.epoch, result)).toBe(true)
    expect(controller.getSnapshot().transferResultPresentation).toMatchObject({
      title: 'Resume revision conflict',
      lines: [expect.stringMatching(/local resume data/u)],
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

function state(
  value: SelectionProjectionState['projection'],
  discovery: SelectionProjectionState['discovery'],
): SelectionProjectionState {
  return Object.freeze({ projection: value, discovery })
}

function requireActions(value: ReturnType<typeof presentArtifactOffers>) {
  if (value.kind !== 'actions') throw new Error(`expected actions, received ${value.kind}`)
  return value
}

function collectText(value: unknown, seen = new Set<object>()): string {
  if (typeof value === 'string') return value
  if (typeof value !== 'object' || value === null || seen.has(value)) return ''
  seen.add(value)
  return Object.values(value).map((nested) => collectText(nested, seen)).join(' ')
}

function deferred<T>(): {
  readonly promise: Promise<T>
  readonly resolve: (value: T) => void
  readonly reject: (reason: unknown) => void
} {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  return {
    promise: new Promise<T>((accept, decline) => {
      resolve = accept
      reject = decline
    }),
    resolve,
    reject,
  }
}
