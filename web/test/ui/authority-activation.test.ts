import { describe, expect, it, vi } from 'vitest'

import {
  bindReceiveIntent,
  materializationRouteIdentity,
  offerArtifacts,
  reconcileArtifactChoice,
  type ArtifactChoiceReconcileOutcome,
  type ArtifactOffers,
  type OfferedArtifactChoice,
} from '../../src/output/planning'
import { initialReceiveLifecycleState } from '../../src/output/workspace'
import { createSelectionSpec } from '../../src/transfer/intent'
import type { SelectionProjectionState } from '../../src/transfer/projection'
import {
  V2AuthorityActivationCoordinator,
  type AuthorityActivationOptions,
} from '../../src/ui/controller/authority-activation'
import type { V2AuthorityProjectionPublication } from '../../src/ui/controller/authority-planning'
import type { V2ActiveProjection } from '../../src/ui/controller/projection-observation'
import type { ActiveReceiveAdoption } from '../../src/ui/controller/active-receive'
import {
  StaleReceiveBoundaryError,
  type V2ControllerWorkflowTraceEvent,
  type V2ReceiverTraceEvent,
} from '../../src/ui/controller/contracts'
import { V2ControllerObservability } from '../../src/ui/controller/controller-observability'
import type { V2JoinedBrowserShare } from '../../src/ui/v2-gateway'
import type {
  V2ArtifactPresentationAuthority,
  V2BoundReceiveOperation,
  V2OwnedActivationAuthority,
} from '../../src/ui/v2-receive-runtime'
import { V2PresentationSourceError } from '../../src/ui/v2-receive-runtime'
import {
  COMPLETE_DISCOVERY,
  projection,
  singleFileProof,
} from '../output/planning/fixture'
import {
  FakeJoinedShare,
  FakeBoundRuntime,
  FakeReceiveComposition,
  MANAGED_ENVIRONMENT,
  candidateBindingForTest,
  deferred,
  identityText,
  turns,
  waitFor,
  type Deferred,
} from './v2-receiver-orchestration-fixture'

describe('authority activation coordinator', () => {
  it.each(['authority-first', 'resolution-first'] as const)(
    'joins %s prerequisite ordering and starts exactly one presentation authority',
    async (order) => {
      const authorityReady = deferred<void>()
      const harness = await createHarness({ authorityReady: authorityReady.promise })
      harness.observe(0)
      harness.planning[0]?.resolve(harness.observations[0]!.offers)
      await waitFor(() => harness.publications.length === 1)

      expect(harness.coordinator.choose('download-original')).toBe(true)
      expect(harness.coordinator.choose('download-original')).toBe(false)
      expect(harness.receive.startedAuthorities).toHaveLength(1)
      await waitFor(() => harness.reconciliation.length === 1)

      if (order === 'authority-first') {
        authorityReady.resolve()
        await waitFor(() => harness.coordinator.getSnapshot().kind === 'waiting-resolution')
        harness.reconciliation[0]?.resolve(harness.observations[0]!.outcome)
      } else {
        harness.reconciliation[0]?.resolve(harness.observations[0]!.outcome)
        await waitFor(() => {
          const snapshot = harness.coordinator.getSnapshot()
          return snapshot.kind === 'waiting-authority' && snapshot.resolution.kind === 'resolved'
        })
        authorityReady.resolve()
      }

      await waitFor(() => harness.activeAdoptions.length === 1)
      expect(harness.receive.startedAuthorities[0]?.commitActions).toHaveLength(1)
      expect(harness.outputAdoptions).toHaveLength(1)
      expect(harness.coordinator.getSnapshot()).toMatchObject({
        kind: 'terminal',
        outcome: { kind: 'bound-operation' },
      })
      expect(harness.transitions()).toEqual(expect.arrayContaining([
        'activation_started',
        'artifact_resolved',
        'commit_started',
        'commit_bound_operation',
        'cleanup_completed',
      ]))
    },
  )

  it('rechecks the current observation after binding and rejects a late stale fence', async () => {
    const binderStarted = deferred<void>()
    const binderContinue = deferred<void>()
    const harness = await createHarness({
      binder: async input => {
        binderStarted.resolve()
        await binderContinue.promise
        return bindReceiveIntent(input)
      },
    })
    harness.observe(0)
    harness.planning[0]?.resolve(harness.observations[0]!.offers)
    await waitFor(() => harness.publications.length === 1)
    expect(harness.coordinator.choose('download-original')).toBe(true)
    await waitFor(() => harness.reconciliation.length === 1)
    harness.reconciliation[0]?.resolve(harness.observations[0]!.outcome)
    await binderStarted.promise

    harness.replaceCurrentWithoutObservation(1)
    binderContinue.resolve()

    await waitFor(() => harness.receive.startedAuthorities[0]?.releaseReasons.length === 1)
    expect(harness.activeAdoptions).toHaveLength(0)
    expect(harness.outputAdoptions).toHaveLength(0)
    expect(harness.coordinator.getSnapshot()).toMatchObject({
      kind: 'terminal',
      outcome: { kind: 'failed' },
    })
  })

  it('settles and detaches a bound operation when later presentation adoption refuses it', async () => {
    const harness = await createHarness({ adoptOutput: false })
    harness.observe(0)
    harness.planning[0]?.resolve(harness.observations[0]!.offers)
    await waitFor(() => harness.publications.length === 1)
    expect(harness.coordinator.choose('download-original')).toBe(true)
    await waitFor(() => harness.reconciliation.length === 1)
    harness.reconciliation[0]?.resolve(harness.observations[0]!.outcome)

    await waitFor(() =>
      (harness.receive.startedAuthorities[0]?.runtime?.detachments.length ?? 0) === 1,
    )
    const runtime = harness.receive.startedAuthorities[0]?.runtime
    expect(runtime?.admissionFailures).toHaveLength(1)
    expect(runtime?.detachments).toEqual(['detached'])
    expect(harness.activeAdoptions).toHaveLength(0)
    expect(harness.coordinator.getSnapshot()).toMatchObject({
      kind: 'terminal',
      outcome: { kind: 'failed' },
    })
  })

  it('settles and detaches owned effects before closing the activation', async () => {
    const cause = new Error('route retained durable effects')
    const settlements: unknown[] = []
    const detachments: string[] = []
    let owned: V2OwnedActivationAuthority | undefined
    const authority: V2ArtifactPresentationAuthority = {
      ready: Promise.resolve(),
      async commit(input) {
        const candidate = await candidateBindingForTest(input.action, 'report.txt')
        const frozen = await input.freezeAtFence(candidate)
        const lifecycle = initialReceiveLifecycleState({
          operationId: frozen.intent.operationId,
          receiveIntentDigest: frozen.intent.digest,
        })
        owned = {
          intent: frozen.intent,
          lifecycle,
          settleActivationFailure: async reason => {
            settlements.push(reason)
            return { lifecycle }
          },
          detach: () => { detachments.push('detached') },
        }
        return { kind: 'owned-effects', cause, authority: owned }
      },
      release: () => undefined,
    }
    const harness = await createHarness({ startAuthority: () => authority })
    harness.observe(0)
    harness.planning[0]?.resolve(harness.observations[0]!.offers)
    await waitFor(() => harness.publications.length === 1)
    expect(harness.coordinator.choose('download-original')).toBe(true)
    await waitFor(() => harness.reconciliation.length === 1)
    harness.reconciliation[0]?.resolve(harness.observations[0]!.outcome)

    await waitFor(() => detachments.length === 1)
    expect(owned).toBeDefined()
    expect(settlements).toEqual([cause])
    expect(harness.activeAdoptions).toHaveLength(0)
    expect(harness.coordinator.getSnapshot()).toMatchObject({
      kind: 'terminal',
      outcome: { kind: 'owned-effects-settled' },
    })
    expect(harness.transitions()).toEqual(expect.arrayContaining([
      'commit_owned_effects',
      'cleanup_completed',
    ]))
  })

  it('retains the installed authority while retry restarts only discovery', async () => {
    const harness = await createHarness()
    harness.observe(0)
    harness.planning[0]?.resolve(harness.observations[0]!.offers)
    await waitFor(() => harness.publications.length === 1)
    expect(harness.coordinator.choose('download-original')).toBe(true)
    await waitFor(() => harness.reconciliation.length === 1)
    harness.reconciliation[0]?.resolve({
      kind: 'retry-required',
      reason: 'receiver-reconnecting',
      observation: harness.observations[0]!.outcome.observation,
    })

    await waitFor(() => harness.coordinator.getSnapshot().kind === 'retry-required')
    expect(harness.coordinator.retry()).toBe(true)
    expect(harness.retries).toHaveLength(1)
    expect(harness.receive.startedAuthorities).toHaveLength(1)
    expect(harness.receive.startedAuthorities[0]?.commitActions).toHaveLength(0)
  })
})

describe('authority activation transactionality', () => {
  it('reuses one presentation authority after a replacement aborts a pre-fence commit', async () => {
    let starts = 0
    let commits = 0
    const authority: V2ArtifactPresentationAuthority = {
      ready: Promise.resolve(),
      commit: async input => {
        commits += 1
        if (commits === 1) {
          await new Promise<void>(resolve => {
            input.signal.addEventListener('abort', () => resolve(), { once: true })
          })
          return { kind: 'retryable-precut', receiverOperationId: identityText(61) }
        }
        const candidate = await candidateBindingForTest(input.action, 'report.txt')
        const frozen = await input.freezeAtFence(candidate)
        return { kind: 'bound-operation', operation: new FakeBoundRuntime(frozen.intent) }
      },
      release: () => undefined,
    }
    const harness = await createHarness({
      startAuthority: () => {
        starts += 1
        return authority
      },
    })
    harness.observe(0)
    harness.planning[0]?.resolve(harness.observations[0]!.offers)
    await waitFor(() => harness.publications.length === 1)
    expect(harness.coordinator.choose('download-original')).toBe(true)
    await waitFor(() => harness.reconciliation.length === 1)
    harness.reconciliation[0]?.resolve(harness.observations[0]!.outcome)
    await waitFor(() => commits === 1)

    harness.observe(1)
    harness.planning[1]?.resolve(harness.observations[1]!.offers)
    harness.reconciliation[1]?.resolve(harness.observations[1]!.outcome)

    await waitFor(() => harness.activeAdoptions.length === 1)
    expect(starts).toBe(1)
    expect(commits).toBe(2)
    expect(harness.transitions()).toContain('commit_pre_cut_retry')
  })

  it('holds a fenced bound result across the no-projection gap and adopts it once', async () => {
    const fenceReached = deferred<void>()
    const returnBound = deferred<void>()
    let runtime: FakeBoundRuntime | undefined
    let starts = 0
    const authority: V2ArtifactPresentationAuthority = {
      ready: Promise.resolve(),
      commit: async input => {
        const candidate = await candidateBindingForTest(input.action, 'report.txt')
        const frozen = await input.freezeAtFence(candidate)
        runtime = new FakeBoundRuntime(frozen.intent)
        fenceReached.resolve()
        await returnBound.promise
        return { kind: 'bound-operation', operation: runtime }
      },
      release: () => undefined,
    }
    const harness = await createHarness({
      startAuthority: () => {
        starts += 1
        return authority
      },
    })
    harness.observe(0)
    harness.planning[0]?.resolve(harness.observations[0]!.offers)
    await waitFor(() => harness.publications.length === 1)
    expect(harness.coordinator.choose('download-original')).toBe(true)
    await waitFor(() => harness.reconciliation.length === 1)
    harness.reconciliation[0]?.resolve(harness.observations[0]!.outcome)
    await fenceReached.promise

    harness.coordinator.startObservationReplacement(new DOMException('replace', 'AbortError'))
    returnBound.resolve()
    await turns()
    expect(harness.outputAdoptions).toHaveLength(0)
    expect(runtime?.detachments).toHaveLength(0)

    harness.observe(1)
    harness.reconciliation[1]?.resolve(harness.observations[1]!.outcome)
    await turns()
    expect(harness.outputAdoptions).toHaveLength(0)
    harness.planning[1]?.resolve(harness.observations[1]!.offers)
    await waitFor(() => harness.activeAdoptions.length === 1)
    expect(starts).toBe(1)
    expect(harness.outputAdoptions).toHaveLength(1)
    expect(runtime?.detachments).toHaveLength(0)
  })

  it.each([
    ['settlement', true, false],
    ['detach', false, true],
    ['settlement-and-detach', true, true],
  ] as const)(
    'retains owned effects when %s cleanup fails and retries only incomplete stages',
    async (_name, failSettlement, failDetach) => {
      const cause = new Error('owned effects')
      let settlementCalls = 0
      let detachCalls = 0
      const authority: V2ArtifactPresentationAuthority = {
        ready: Promise.resolve(),
        commit: async input => {
          const candidate = await candidateBindingForTest(input.action, 'report.txt')
          const frozen = await input.freezeAtFence(candidate)
          const lifecycle = initialReceiveLifecycleState({
            operationId: frozen.intent.operationId,
            receiveIntentDigest: frozen.intent.digest,
          })
          return {
            kind: 'owned-effects',
            cause,
            authority: {
              intent: frozen.intent,
              lifecycle,
              settleActivationFailure: async () => {
                settlementCalls += 1
                if (failSettlement && settlementCalls === 1) throw new Error('settlement failed')
                return { lifecycle }
              },
              detach: () => {
                detachCalls += 1
                if (failDetach && detachCalls === 1) throw new Error('detach failed')
              },
            },
          }
        },
        release: () => undefined,
      }
      const harness = await createHarness({ startAuthority: () => authority })
      harness.observe(0)
      harness.planning[0]?.resolve(harness.observations[0]!.offers)
      await waitFor(() => harness.publications.length === 1)
      expect(harness.coordinator.choose('download-original')).toBe(true)
      await waitFor(() => harness.reconciliation.length === 1)
      harness.reconciliation[0]?.resolve(harness.observations[0]!.outcome)

      await waitFor(() => harness.coordinator.getSnapshot().kind === 'cleanup-required')
      expect(harness.coordinator.pending).toBe(true)
      expect(harness.coordinator.choose('download-original')).toBe(false)
      expect(harness.transitions()).not.toContain('cleanup_completed')
      expect(harness.coordinator.retry()).toBe(true)
      await waitFor(() => harness.coordinator.getSnapshot().kind === 'terminal')
      expect(settlementCalls).toBe(failSettlement ? 2 : 1)
      expect(detachCalls).toBe(failDetach ? 2 : 1)
      expect(harness.retainedRefreshes).toHaveLength(1)
      expect(harness.transitions().filter(value => value === 'cleanup_completed')).toHaveLength(1)
    },
  )

  it.each([
    ['settlement', true, false],
    ['detach', false, true],
    ['settlement-and-detach', true, true],
  ] as const)(
    'retains a rejected bound operation when %s cleanup fails',
    async (_name, failSettlement, failDetach) => {
      let settlementCalls = 0
      let detachCalls = 0
      const authority: V2ArtifactPresentationAuthority = {
        ready: Promise.resolve(),
        commit: async input => {
          const candidate = await candidateBindingForTest(input.action, 'report.txt')
          const frozen = await input.freezeAtFence(candidate)
          const runtime = new FakeBoundRuntime(frozen.intent)
          runtime.settleTransferAdmissionFailure = () => {
            settlementCalls += 1
            if (failSettlement && settlementCalls === 1) throw new Error('settlement failed')
            return { lifecycle: runtime.lifecycle }
          }
          runtime.detach = () => {
            detachCalls += 1
            if (failDetach && detachCalls === 1) throw new Error('detach failed')
          }
          return { kind: 'bound-operation', operation: runtime }
        },
        release: () => undefined,
      }
      const harness = await createHarness({ startAuthority: () => authority, adoptOutput: false })
      harness.observe(0)
      harness.planning[0]?.resolve(harness.observations[0]!.offers)
      await waitFor(() => harness.publications.length === 1)
      expect(harness.coordinator.choose('download-original')).toBe(true)
      await waitFor(() => harness.reconciliation.length === 1)
      harness.reconciliation[0]?.resolve(harness.observations[0]!.outcome)

      await waitFor(() => harness.coordinator.getSnapshot().kind === 'cleanup-required')
      expect(harness.coordinator.retry()).toBe(true)
      await waitFor(() => harness.coordinator.getSnapshot().kind === 'terminal')
      expect(settlementCalls).toBe(failSettlement ? 2 : 1)
      expect(detachCalls).toBe(failDetach ? 2 : 1)
      expect(harness.activeAdoptions).toHaveLength(0)
      expect(harness.retainedRefreshes).toHaveLength(1)
    },
  )

  it('classifies synchronous picker refusal without an incident-facing action error', async () => {
    const refusal = new V2PresentationSourceError('picker_refused', 'picker refused')
    const harness = await createHarness({ startAuthority: () => { throw refusal } })
    harness.observe(0)
    harness.planning[0]?.resolve(harness.observations[0]!.offers)
    await waitFor(() => harness.publications.length === 1)
    expect(harness.coordinator.choose('download-original')).toBe(true)
    expect(harness.coordinator.getSnapshot()).toMatchObject({
      kind: 'terminal',
      outcome: { kind: 'picker-refused' },
    })
    expect(harness.actionErrors).toHaveLength(0)
    expect(harness.coordinator.choose('download-original')).toBe(true)
  })

  it('reports a synchronous native presentation-source failure', async () => {
    const failure = new Error('native picker failure')
    const harness = await createHarness({ startAuthority: () => { throw failure } })
    harness.observe(0)
    harness.planning[0]?.resolve(harness.observations[0]!.offers)
    await waitFor(() => harness.publications.length === 1)
    expect(harness.coordinator.choose('download-original')).toBe(true)
    expect(harness.coordinator.getSnapshot()).toMatchObject({
      kind: 'terminal',
      outcome: { kind: 'failed' },
    })
    expect(harness.actionErrors).toEqual([failure])
  })
})

describe('post-fence activation ownership', () => {
  it.each(['selection', 'route', 'artifact'] as const)(
    'settles and detaches a bound result rejected by a changed %s',
    async incompatibility => {
      const fenceReached = deferred<void>()
      const returnBound = deferred<void>()
      let runtime: FakeBoundRuntime | undefined
      const authority: V2ArtifactPresentationAuthority = {
        ready: Promise.resolve(),
        commit: async input => {
          const candidate = await candidateBindingForTest(input.action, 'report.txt')
          const frozen = await input.freezeAtFence(candidate)
          runtime = new FakeBoundRuntime(frozen.intent)
          fenceReached.resolve()
          await returnBound.promise
          return { kind: 'bound-operation', operation: runtime }
        },
        release: () => undefined,
      }
      const harness = await createHarness({ startAuthority: () => authority })
      harness.observe(0)
      harness.planning[0]?.resolve(harness.observations[0]!.offers)
      await waitFor(() => harness.publications.length === 1)
      expect(harness.coordinator.choose('download-original')).toBe(true)
      await waitFor(() => harness.reconciliation.length === 1)
      harness.reconciliation[0]?.resolve(harness.observations[0]!.outcome)
      await fenceReached.promise

      if (incompatibility === 'selection') {
        harness.coordinator.invalidate(new StaleReceiveBoundaryError(), 'selection-changed')
        returnBound.resolve()
      } else {
        harness.coordinator.startObservationReplacement(new StaleReceiveBoundaryError())
        returnBound.resolve()
        harness.observe(1)
        harness.planning[1]?.resolve(harness.observations[1]!.offers)
        const observed = harness.observations[1]!.outcome
        if (observed.kind !== 'resolved') throw new Error('replacement did not resolve')
        if (observed.action.route.kind !== 'direct-atomic') {
          throw new Error('replacement did not retain the managed route')
        }
        const action = incompatibility === 'route'
          ? Object.freeze({
              ...observed.action,
              route: Object.freeze({
                ...observed.action.route,
                target: Object.freeze({
                  ...observed.action.route.target,
                  routeId: 'replacement-route',
                }),
              }),
            })
          : Object.freeze({
              ...observed.action,
              resolvedArtifactDigest: identityText(86, 32),
              artifact: Object.freeze({
                ...observed.action.artifact,
                digest: identityText(86, 32),
              }),
            })
        harness.reconciliation[1]?.resolve(Object.freeze({
          kind: 'resolved',
          action,
          observation: Object.freeze({
            ...observed.observation,
            resolvedArtifactDigest: action.artifact.digest,
          }),
        }))
      }

      await waitFor(() => (runtime?.detachments.length ?? 0) === 1)
      expect(runtime?.admissionFailures).toHaveLength(1)
      expect(harness.activeAdoptions).toHaveLength(0)
      expect(harness.outputAdoptions).toHaveLength(0)
      expect(harness.coordinator.pending).toBe(false)
    },
  )
})

interface PlannedObservation {
  readonly active: V2ActiveProjection
  readonly offers: ArtifactOffers
  readonly outcome: ArtifactChoiceReconcileOutcome
}

interface CoordinatorHarness {
  readonly coordinator: V2AuthorityActivationCoordinator
  readonly receive: FakeReceiveComposition
  readonly observations: readonly PlannedObservation[]
  readonly planning: Deferred<ArtifactOffers>[]
  readonly reconciliation: Deferred<ArtifactChoiceReconcileOutcome>[]
  readonly publications: V2AuthorityProjectionPublication[]
  readonly activeAdoptions: ActiveReceiveAdoption[]
  readonly retries: V2ActiveProjection[]
  readonly outputAdoptions: Array<Readonly<{
    choice: OfferedArtifactChoice['choice']
    runtime: V2BoundReceiveOperation
  }>>
  readonly actionErrors: unknown[]
  readonly retainedRefreshes: string[]
  observe(index: number): void
  replaceCurrentWithoutObservation(index: number): void
  transitions(): string[]
}

async function createHarness(options: Readonly<{
  authorityReady?: Promise<void>
  adoptOutput?: boolean
  binder?: NonNullable<AuthorityActivationOptions['binder']>
  startAuthority?: (offered: OfferedArtifactChoice) => V2ArtifactPresentationAuthority
}> = {}): Promise<CoordinatorHarness> {
  const joined = new FakeJoinedShare(true)
  const selection = await createSelectionSpec({
    shareInstance: joined.descriptor.shareInstanceId,
    syntheticRoot: joined.descriptor.syntheticRootId,
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const observations: PlannedObservation[] = []
  for (let revision = 1; revision <= 3; revision += 1) {
    const projected = projection(
      selection,
      singleFileProof(),
      128n,
      BigInt(revision),
    )
    const state: SelectionProjectionState = Object.freeze({
      projection: projected,
      discovery: COMPLETE_DISCOVERY,
    })
    const offers = await offerArtifacts(projected, COMPLETE_DISCOVERY, MANAGED_ENVIRONMENT)
    if (offers.kind !== 'artifact-actions') throw new Error('test projection did not offer an artifact')
    const outcome = await reconcileArtifactChoice({
      choice: offers.primary.choice,
      preferredRoute: materializationRouteIdentity(offers.primary.route),
      expectedSelectionDigest: selection.digest,
      projection: projected,
      discovery: COMPLETE_DISCOVERY,
      environment: MANAGED_ENVIRONMENT,
      previousObservation: null,
    })
    if (outcome.kind !== 'resolved') throw new Error('test artifact did not resolve')
    observations.push({
      offers,
      outcome,
      active: {
        revision,
        joined: joined as unknown as V2JoinedBrowserShare,
        selection,
        frozenSelection: joined.selection.snapshot(),
        epoch: projected.epoch,
        controller: new AbortController(),
        protocolSessionId: identityText(40 + revision),
        state,
        environment: MANAGED_ENVIRONMENT,
      },
    })
  }

  const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
  receive.authorityReady = options.authorityReady
  if (options.startAuthority !== undefined) {
    vi.spyOn(receive, 'startArtifactAuthority').mockImplementation(options.startAuthority)
  }
  const planning: Deferred<ArtifactOffers>[] = []
  const reconciliation: Deferred<ArtifactChoiceReconcileOutcome>[] = []
  const publications: V2AuthorityProjectionPublication[] = []
  const activeAdoptions: ActiveReceiveAdoption[] = []
  const retries: V2ActiveProjection[] = []
  const outputAdoptions: CoordinatorHarness['outputAdoptions'][number][] = []
  const actionErrors: unknown[] = []
  const retainedRefreshes: string[] = []
  const traces: V2ControllerWorkflowTraceEvent[] = []
  let current: V2ActiveProjection | undefined
  const observability = new V2ControllerObservability({
    trace: Object.freeze({
      get current() {
        return (event: V2ReceiverTraceEvent) => {
          if (event.name === 'join_transition' ||
              (event.name === 'authority_transition' && 'activationId' in event)) {
            traces.push(event)
          }
        }
      },
    }),
  })
  const coordinator = new V2AuthorityActivationCoordinator({
    receive,
    activeReceive: {
      prepareAdoption: adoption => Object.freeze({
        commit: () => { activeAdoptions.push(adoption) },
        start: () => undefined,
      }),
    },
    observability,
    currentProjection: () => current,
    currentJoinedShare: () => current?.joined,
    choiceBlocked: () => false,
    retryProjection: active => { retries.push(active) },
    publishProjection: publication => { publications.push(publication) },
    adoptReceiveIntent: (choice, _intent, runtime, commitOwnership) => {
      if (!(options.adoptOutput ?? true)) return false
      commitOwnership()
      outputAdoptions.push({ choice, runtime })
      return true
    },
    refreshRetainedInventory: () => { retainedRefreshes.push('refreshed') },
    publishActionError: error => { actionErrors.push(error) },
    planner: () => {
      const gate = deferred<ArtifactOffers>()
      planning.push(gate)
      return gate.promise
    },
    reconciler: () => {
      const gate = deferred<ArtifactChoiceReconcileOutcome>()
      reconciliation.push(gate)
      return gate.promise
    },
    ...(options.binder === undefined ? {} : { binder: options.binder }),
    createActivationId: () => identityText(30),
  })

  return {
    coordinator,
    receive,
    observations,
    planning,
    reconciliation,
    publications,
    activeAdoptions,
    retries,
    outputAdoptions,
    actionErrors,
    retainedRefreshes,
    observe(index) {
      current = observations[index]?.active
      if (current === undefined) throw new Error('missing test observation')
      coordinator.observeProjection(current)
    },
    replaceCurrentWithoutObservation(index) {
      current = observations[index]?.active
      if (current === undefined) throw new Error('missing replacement observation')
    },
    transitions: () => traces
      .filter((event): event is Extract<V2ControllerWorkflowTraceEvent, { name: 'authority_transition' }> =>
        event.name === 'authority_transition' && 'activationId' in event)
      .map(event => event.transition),
  }
}
