import { describe, expect, it, vi } from 'vitest'

import {
  bindReceiveIntent,
  materializationRouteIdentity,
  offerArtifacts,
  reconcileArtifactChoice,
  type CandidateMaterializationBinding,
  type OfferedArtifactChoice,
  type ResolvedArtifactAction,
} from '../../src/output/planning'
import {
  createOperationID,
  createOriginalFileArtifact,
  createPortableBinding,
  createPortablePlanID,
  createSelectionSpec,
  type SelectionSpec,
} from '../../src/transfer/intent'
import { nextProjectionEpoch } from '../../src/transfer/projection'
import type { BrowserReceiveWindow } from '../../src/ui/browser-receive/contracts'
import { createPortableReceiveOperation } from '../../src/ui/browser-receive/portable'
import {
  PortableArtifactPresentationAuthority,
  startPortableArtifactAuthority,
} from '../../src/ui/browser-receive/portable-route'
import {
  COMPLETE_DISCOVERY,
  environment,
  handoffTarget,
  identity,
  portableOffer,
  projection,
  singleFileProof,
} from '../output/planning/fixture'

describe('portable route activation', () => {
  it('commits only canonical in-memory ownership after the coordinator fence', async () => {
    const fixture = await portableFixture()
    const effects = browserEffects()
    const order: string[] = []
    const authority = startPortableArtifactAuthority(effects.windowPort, fixture.offered)
    const freezeAtFence = vi.fn(async (candidate: CandidateMaterializationBinding) => {
      order.push('fence')
      return bindReceiveIntent({
        selection: fixture.selection,
        action: fixture.action,
        candidate,
      })
    })

    await expect(authority.ready).resolves.toBeUndefined()
    const result = await authority.commit({
      action: fixture.action,
      signal: new AbortController().signal,
      freezeAtFence,
    })

    expect(result.kind).toBe('bound-operation')
    if (result.kind !== 'bound-operation') throw new Error('portable route retained durable effects')
    expect(freezeAtFence).toHaveBeenCalledTimes(1)
    expect(freezeAtFence.mock.calls[0]?.[0]).toMatchObject({
      kind: 'portable-binding',
      portableRouteId: fixture.action.route.kind === 'portable-handoff'
        ? fixture.action.route.portable.routeId
        : undefined,
      handoffTargetRouteId: fixture.action.route.kind === 'portable-handoff'
        ? fixture.action.route.handoffTarget.routeId
        : undefined,
      portable: {
        operationId: result.operation.intent.operationId,
        artifactDigest: fixture.action.artifact.digest,
        preparation: 'exact-artifact',
      },
    })
    expect(result.operation.intent.artifact.digest).toBe(fixture.action.artifact.digest)
    expect(result.operation.lifecycle.kind).toBe('intent-frozen')
    expect(order).toEqual(['fence'])
    expect(effects.indexedDbOpen).not.toHaveBeenCalled()
    expect(effects.createObjectUrl).not.toHaveBeenCalled()
    expect(effects.createAnchor).not.toHaveBeenCalled()
    expect(effects.appendAnchor).not.toHaveBeenCalled()
  })

  it('binds the newest compatible resolved artifact instead of click-time projection data', async () => {
    const fixture = await portableFixture()
    const newestAction = await replacementResolvedAction(fixture)
    const authority = startPortableArtifactAuthority(browserEffects().windowPort, fixture.offered)
    let candidate: CandidateMaterializationBinding | undefined

    const result = await authority.commit({
      action: newestAction,
      signal: new AbortController().signal,
      freezeAtFence: async (supplied) => {
        candidate = supplied
        return bindReceiveIntent({
          selection: fixture.selection,
          action: newestAction,
          candidate: supplied,
        })
      },
    })

    expect(result.kind).toBe('bound-operation')
    expect(candidate?.kind).toBe('portable-binding')
    if (candidate?.kind !== 'portable-binding') throw new Error('portable candidate was not frozen')
    expect(candidate.portable.artifactDigest).toBe(newestAction.artifact.digest)
    if (result.kind !== 'bound-operation') throw new Error('portable operation was not bound')
    expect(result.operation.intent.artifact.digest).toBe(newestAction.artifact.digest)
  })

  it('reuses the authority with the newest action after clean pre-cut cancellation', async () => {
    const fixture = await portableFixture()
    const newestAction = await replacementResolvedAction(fixture)
    const effects = browserEffects()
    const authority = startPortableArtifactAuthority(effects.windowPort, fixture.offered)
    const firstAttempt = new AbortController()
    let firstCandidate: CandidateMaterializationBinding | undefined

    const firstResult = await authority.commit({
      action: fixture.action,
      signal: firstAttempt.signal,
      freezeAtFence: async (candidate) => {
        firstCandidate = candidate
        await bindReceiveIntent({
          selection: fixture.selection,
          action: fixture.action,
          candidate,
        })
        firstAttempt.abort(new DOMException('projection replacement', 'AbortError'))
        firstAttempt.signal.throwIfAborted()
        throw new Error('unreachable final fence')
      },
    })

    expect(firstCandidate?.kind).toBe('portable-binding')
    if (firstCandidate?.kind !== 'portable-binding') throw new Error('portable candidate was not frozen')
    expect(firstResult).toEqual({
      kind: 'retryable-precut',
      receiverOperationId: firstCandidate.portable.operationId,
    })
    expect(effects.all()).toEqual([])

    const result = await authority.commit({
      action: newestAction,
      signal: new AbortController().signal,
      freezeAtFence: candidate => bindReceiveIntent({
        selection: fixture.selection,
        action: newestAction,
        candidate,
      }),
    })
    expect(result.kind).toBe('bound-operation')
    if (result.kind !== 'bound-operation') throw new Error('portable operation was not bound')
    expect(result.operation.intent.artifact.digest).toBe(newestAction.artifact.digest)
    expect(result.operation.intent.artifact.digest).not.toBe(fixture.action.artifact.digest)
    expect(effects.all()).toEqual([])
  })

  it('reuses the authority when cancellation arrives before candidate preparation', async () => {
    const fixture = await portableFixture()
    const authority = startPortableArtifactAuthority(browserEffects().windowPort, fixture.offered)
    const firstAttempt = new AbortController()
    firstAttempt.abort(new DOMException('projection replacement', 'AbortError'))
    const firstFence = vi.fn()

    await expect(authority.commit({
      action: fixture.action,
      signal: firstAttempt.signal,
      freezeAtFence: firstFence,
    })).resolves.toEqual({ kind: 'retryable-precut' })
    expect(firstFence).not.toHaveBeenCalled()

    const result = await authority.commit({
      action: fixture.action,
      signal: new AbortController().signal,
      freezeAtFence: candidate => bindReceiveIntent({
        selection: fixture.selection,
        action: fixture.action,
        candidate,
      }),
    })
    expect(result.kind).toBe('bound-operation')
  })

  it('detaches an inert operation and recommits when cancellation wins before return', async () => {
    const fixture = await portableFixture()
    const effects = browserEffects()
    const operationAssemblyEntered = deferred<void>()
    const continueOperationAssembly = deferred<void>()
    let assemblyCount = 0
    let firstDetach: ReturnType<typeof vi.spyOn> | undefined
    const authority = new PortableArtifactPresentationAuthority({
      windowPort: effects.windowPort,
      offered: fixture.offered,
      dependencies: {
        createReceiveOperation: async (windowPort, action, intent, diagnostics) => {
          const operation = await createPortableReceiveOperation(
            windowPort,
            action,
            intent,
            diagnostics,
          )
          assemblyCount += 1
          if (assemblyCount === 1) {
            firstDetach = vi.spyOn(operation, 'detach')
            operationAssemblyEntered.resolve()
            await continueOperationAssembly.promise
          }
          return operation
        },
      },
    })
    const firstAttempt = new AbortController()
    const committing = authority.commit({
      action: fixture.action,
      signal: firstAttempt.signal,
      freezeAtFence: candidate => bindReceiveIntent({
        selection: fixture.selection,
        action: fixture.action,
        candidate,
      }),
    })
    await operationAssemblyEntered.promise
    firstAttempt.abort(new DOMException('projection replacement', 'AbortError'))
    continueOperationAssembly.resolve()

    await expect(committing).resolves.toMatchObject({ kind: 'retryable-precut' })
    expect(firstDetach).toHaveBeenCalledTimes(1)
    expect(effects.all()).toEqual([])

    const result = await authority.commit({
      action: fixture.action,
      signal: new AbortController().signal,
      freezeAtFence: candidate => bindReceiveIntent({
        selection: fixture.selection,
        action: fixture.action,
        candidate,
      }),
    })
    expect(result.kind).toBe('bound-operation')
    expect(assemblyCount).toBe(2)
  })

  it('rejects route identity and browser support drift before the final fence', async () => {
    const fixture = await portableFixture()
    if (fixture.action.route.kind !== 'portable-handoff') throw new Error('portable fixture drifted')
    const changedRouteId: ResolvedArtifactAction = Object.freeze({
      ...fixture.action,
      route: Object.freeze({
        ...fixture.action.route,
        portable: Object.freeze({
          ...fixture.action.route.portable,
          routeId: 'replacement-portable-route',
        }),
      }),
    })
    const lostSupport: ResolvedArtifactAction = Object.freeze({
      ...fixture.action,
      route: Object.freeze({
        ...fixture.action.route,
        handoffTarget: Object.freeze({
          ...fixture.action.route.handoffTarget,
          supportsPortableArtifact: false,
        }),
      }),
    })

    for (const action of [changedRouteId, lostSupport]) {
      const freezeAtFence = vi.fn()
      const authority = startPortableArtifactAuthority(browserEffects().windowPort, fixture.offered)
      await expect(authority.commit({
        action,
        signal: new AbortController().signal,
        freezeAtFence,
      })).rejects.toMatchObject({ name: 'NotSupportedError' })
      expect(freezeAtFence).not.toHaveBeenCalled()
    }
  })

  it('rejects a frozen intent bound to a different resolved action', async () => {
    const fixture = await portableFixture()
    const newestAction = await replacementResolvedAction(fixture)
    const newestRoute = newestAction.route
    if (newestRoute.kind !== 'portable-handoff') throw new Error('portable fixture drifted')
    const effects = browserEffects()
    const authority = startPortableArtifactAuthority(effects.windowPort, fixture.offered)

    await expect(authority.commit({
      action: fixture.action,
      signal: new AbortController().signal,
      freezeAtFence: async () => {
        const candidate = Object.freeze({
          kind: 'portable-binding' as const,
          portableRouteId: newestRoute.portable.routeId,
          handoffTargetRouteId: newestRoute.handoffTarget.routeId,
          portable: await createPortableBinding({
            operationId: createOperationID(),
            portablePlanId: createPortablePlanID(),
            artifact: newestAction.artifact,
          }),
        })
        return bindReceiveIntent({
          selection: fixture.selection,
          action: newestAction,
          candidate,
        })
      },
    })).rejects.toThrow('Frozen ReceiveIntent does not bind the prepared portable candidate')
    expect(effects.all()).toEqual([])
    await expect(authority.commit({
      action: fixture.action,
      signal: new AbortController().signal,
      freezeAtFence: vi.fn(),
    })).rejects.toMatchObject({ name: 'InvalidStateError' })
  })

  it('releases and failed fences without creating browser or spool effects', async () => {
    const fixture = await portableFixture()
    const releasedEffects = browserEffects()
    const released = startPortableArtifactAuthority(releasedEffects.windowPort, fixture.offered)
    const releasedFence = vi.fn()
    released.release(new Error('selection changed'))

    await expect(released.commit({
      action: fixture.action,
      signal: new AbortController().signal,
      freezeAtFence: releasedFence,
    })).rejects.toMatchObject({ name: 'InvalidStateError' })
    expect(releasedFence).not.toHaveBeenCalled()
    expect(releasedEffects.all()).toEqual([])

    const rejectedEffects = browserEffects()
    const rejected = startPortableArtifactAuthority(rejectedEffects.windowPort, fixture.offered)
    const fenceFailure = new Error('final observation changed')
    await expect(rejected.commit({
      action: fixture.action,
      signal: new AbortController().signal,
      freezeAtFence: async () => Promise.reject(fenceFailure),
    })).rejects.toBe(fenceFailure)
    expect(rejectedEffects.all()).toEqual([])
    await expect(rejected.commit({
      action: fixture.action,
      signal: new AbortController().signal,
      freezeAtFence: vi.fn(),
    })).rejects.toMatchObject({ name: 'InvalidStateError' })
  })

  it('drains a fence that resolves after release without assembling an operation', async () => {
    const fixture = await portableFixture()
    const effects = browserEffects()
    const authority = startPortableArtifactAuthority(effects.windowPort, fixture.offered)
    let continueFence: (() => void) | undefined
    const fenceGate = new Promise<void>((resolve) => { continueFence = resolve })
    const freezeAtFence = vi.fn(async (candidate: CandidateMaterializationBinding) => {
      await fenceGate
      return bindReceiveIntent({
        selection: fixture.selection,
        action: fixture.action,
        candidate,
      })
    })
    const committing = authority.commit({
      action: fixture.action,
      signal: new AbortController().signal,
      freezeAtFence,
    })
    await vi.waitFor(() => expect(freezeAtFence).toHaveBeenCalledTimes(1))

    authority.release(new Error('newest observation invalidated the route'))
    continueFence?.()

    await expect(committing).rejects.toMatchObject({
      name: 'V2PresentationSourceError',
      outcome: 'stale_replacement',
    })
    expect(effects.all()).toEqual([])
  })
})

async function portableFixture(): Promise<Readonly<{
  selection: SelectionSpec
  offered: OfferedArtifactChoice
  action: ResolvedArtifactAction
}>> {
  const selection = await createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const installedEnvironment = environment({
    targets: [handoffTarget('browser-download')],
    portable: portableOffer('portable-memory'),
  })
  const currentProjection = projection(selection, singleFileProof())
  const offers = await offerArtifacts(
    currentProjection,
    COMPLETE_DISCOVERY,
    installedEnvironment,
  )
  if (offers.kind !== 'artifact-actions' || offers.primary.route.kind !== 'portable-handoff') {
    throw new Error('portable fixture did not produce an actionable route')
  }
  const offered = offers.primary
  const outcome = await reconcileArtifactChoice({
    choice: offered.choice,
    preferredRoute: materializationRouteIdentity(offered.route),
    expectedSelectionDigest: selection.digest,
    projection: currentProjection,
    discovery: COMPLETE_DISCOVERY,
    environment: installedEnvironment,
    previousObservation: null,
  })
  if (outcome.kind !== 'resolved') throw new Error('portable fixture did not resolve')
  return Object.freeze({ selection, offered, action: outcome.action })
}

async function replacementResolvedAction(
  fixture: Awaited<ReturnType<typeof portableFixture>>,
): Promise<ResolvedArtifactAction> {
  const artifact = await createOriginalFileArtifact({
    fileId: identity(91),
    sourcePath: 'newest/report.txt',
    suggestedName: 'report.txt',
  })
  return Object.freeze({
    ...fixture.action,
    projectionEpoch: nextProjectionEpoch(fixture.action.projectionEpoch),
    resolvedArtifactDigest: artifact.digest,
    artifact,
  })
}

function browserEffects(): Readonly<{
  windowPort: BrowserReceiveWindow
  indexedDbOpen: ReturnType<typeof vi.fn>
  createObjectUrl: ReturnType<typeof vi.fn>
  createAnchor: ReturnType<typeof vi.fn>
  appendAnchor: ReturnType<typeof vi.fn>
  all(): readonly unknown[]
}> {
  const indexedDbOpen = vi.fn()
  const createObjectUrl = vi.fn(() => 'blob:portable')
  const createAnchor = vi.fn(() => ({
    download: '',
    href: '',
    hidden: false,
    click: vi.fn(),
    remove: vi.fn(),
  }))
  const appendAnchor = vi.fn()
  const windowPort = {
    indexedDB: { open: indexedDbOpen },
    URL: { createObjectURL: createObjectUrl, revokeObjectURL: vi.fn() },
    document: { createElement: createAnchor, documentElement: { append: appendAnchor } },
    setTimeout: vi.fn(() => 1),
    clearTimeout: vi.fn(),
    Blob,
    WritableStream,
  } as unknown as BrowserReceiveWindow
  return Object.freeze({
    windowPort,
    indexedDbOpen,
    createObjectUrl,
    createAnchor,
    appendAnchor,
    all: () => [
      ...indexedDbOpen.mock.calls,
      ...createObjectUrl.mock.calls,
      ...createAnchor.mock.calls,
      ...appendAnchor.mock.calls,
    ],
  })
}

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise
    reject = rejectPromise
  })
  return { promise, resolve, reject }
}
