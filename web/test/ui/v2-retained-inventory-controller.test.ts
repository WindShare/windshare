import { describe, expect, it } from 'vitest'

import {
  createIncidentScopeIssuer,
  lifecycleFailureFact,
  type FailureFact,
  type FailureFactRelation,
  type IncidentScopeHandle,
  type IncidentScopeIdentity,
  type IncidentScopeKind,
  type IncidentScopeOwner,
  type PresentationDecision,
} from '../../src/diagnostics/incident'
import { recordOutputException } from '../../src/output/diagnostics'
import { RetainedInventoryCoordinator } from '../../src/ui/controller/retained-inventory'
import type { V2JoinedBrowserShare } from '../../src/ui/v2-gateway'
import type {
  V2BoundReceiveOperation,
  V2ReceiveCompositionPort,
  V2RetainedReceiveAction,
  V2RetainedReceiveActionResult,
  V2RetainedReceiveInventory,
  V2RetainedReceiveOperation,
} from '../../src/ui/v2-receive-runtime'

describe('retained inventory incident ownership', () => {
  it('gives successful and failed loads distinct closed presentation scopes', async () => {
    const ready = testInventory([])
    let loadCount = 0
    const harness = retainedHarness(() => {
      loadCount += 1
      return loadCount === 1
        ? Promise.resolve(ready)
        : Promise.reject(new Error('inventory failed'))
    })

    await harness.coordinator.load()
    await harness.coordinator.load()

    expect(harness.incidents.decisions).toMatchObject([
      {
        scope: { scopeKind: 'retained_inventory', scopeSequence: 1n },
        decision: {
          kind: 'excluded',
          boundary: 'retained_inventory',
          reason: 'success',
        },
      },
      {
        scope: { scopeKind: 'retained_inventory', scopeSequence: 2n },
        decision: {
          kind: 'incident',
          boundary: 'retained_inventory',
          outcome: 'failed',
        },
      },
    ])
    expect(harness.incidents.facts).toMatchObject([
      {
        scope: { scopeKind: 'retained_inventory', scopeSequence: 2n },
        fact: {
          kind: 'unclassified',
          stage: 'retained_inventory',
          recoveryDisposition: 'none',
        },
        relation: 'contributor',
      },
    ])
    expect(harness.publications.at(-1)).toMatchObject({ kind: 'failed' })
    expect(harness.incidents.owners.every((owner) => owner.isClosed())).toBe(true)
  })

  it('keeps the inventory result unchanged when diagnostic submission throws', async () => {
    const harness = retainedHarness(
      () => Promise.reject(new Error('inventory failed')),
    )
    harness.incidents.onDecision = () => {
      throw new Error('diagnostic submit failed')
    }

    await harness.coordinator.load()

    expect(harness.publications.at(-1)).toMatchObject({
      kind: 'failed',
      error: 'Stored receive tasks could not be loaded.',
    })
    expect(harness.incidents.decisions[0]?.decision).toMatchObject({
      kind: 'incident',
      boundary: 'retained_inventory',
      outcome: 'failed',
    })
    expect(harness.incidents.owners[0]?.isClosed()).toBe(true)
  })

  it('closes a replaced load as stale without sharing its scope or quota', async () => {
    const first = deferred<V2RetainedReceiveInventory>()
    const second = deferred<V2RetainedReceiveInventory>()
    let loadCount = 0
    const harness = retainedHarness(() => {
      loadCount += 1
      return loadCount === 1 ? first.promise : second.promise
    })

    const stale = harness.coordinator.load()
    const current = harness.coordinator.load()
    second.resolve(testInventory([]))
    await current
    first.resolve(testInventory([]))
    await stale

    expect(harness.incidents.decisions).toMatchObject([
      {
        scope: { scopeKind: 'retained_inventory', scopeSequence: 1n },
        decision: {
          kind: 'excluded',
          reason: 'stale_replacement',
        },
      },
      {
        scope: { scopeKind: 'retained_inventory', scopeSequence: 2n },
        decision: {
          kind: 'excluded',
          reason: 'success',
        },
      },
    ])
    expect(harness.incidents.owners.every((owner) => owner.isClosed())).toBe(true)
  })

  it('submits source-reviewed needs-attention facts before publishing ready inventory', async () => {
    const retained = operation([], 'needs-attention')
    const failures = Object.freeze([
      lifecycleFailureFact({
        stage: 'retained_inventory',
        recoveryDisposition: 'needs_attention',
        kind: 'needs-attention',
        reason: 'publication-unknown',
      }),
      lifecycleFailureFact({
        stage: 'retained_inventory',
        recoveryDisposition: 'needs_attention',
        kind: 'needs-attention',
        reason: 'cleanup-unknown',
      }),
    ])
    const order: string[] = []
    const harness = retainedHarness(
      () => Promise.resolve(testInventory([retained], undefined, failures)),
      undefined,
      {
        onPublish: published => {
          if (published.kind === 'ready') order.push('ready')
        },
      },
    )
    harness.incidents.onDecision = () => order.push('decision')

    await harness.coordinator.load()

    expect(order).toEqual(['decision', 'ready'])
    expect(harness.publications.at(-1)).toMatchObject({
      kind: 'ready',
      operations: [retained],
    })
    expect(harness.incidents.decisions).toHaveLength(1)
    expect(harness.incidents.decisions[0]).toMatchObject({
      decision: {
        kind: 'incident',
        boundary: 'retained_inventory',
        outcome: 'failed',
      },
    })
    expect(harness.incidents.facts).toMatchObject([
      {
        fact: {
          kind: 'lifecycle_failure',
          stage: 'retained_inventory',
          payload: {
            lifecycleFailure: {
              kind: 'needs-attention',
              reason: 'publication-unknown',
            },
          },
        },
        relation: 'contributor',
      },
      {
        fact: {
          kind: 'lifecycle_failure',
          stage: 'retained_inventory',
          payload: {
            lifecycleFailure: {
              kind: 'needs-attention',
              reason: 'cleanup-unknown',
            },
          },
        },
        relation: 'contributor',
      },
    ])
  })

  it('keeps retained trace lazy and admits an unowned AbortError as a native fault', async () => {
    const retained = operation(['save'], 'save-artifact')
    const nativeAbort = new DOMException('private publication detail', 'AbortError')
    const inventory = testInventory(
      [retained],
      (_operation, _action, _signal, failures) => {
        recordOutputException(failures?.publication, nativeAbort)
        return Promise.reject(nativeAbort)
      },
    )
    const enabled = retainedHarness(
      () => Promise.resolve(inventory),
      undefined,
      { traceEnabled: true },
    )

    await enabled.coordinator.load()
    enabled.coordinator.perform(retained, 'save')
    await turns()

    expect(enabled.traceEvents).toEqual([
      { name: 'receive.inventory.load.started' },
      { name: 'receive.inventory.load.completed', operation_count: 1 },
      {
        name: 'receive.inventory.action.started',
        retained_action: 'save',
        continuation: 'save-artifact',
      },
      {
        name: 'receive.inventory.action.failed',
        retained_action: 'save',
        continuation: 'save-artifact',
      },
      { name: 'receive.inventory.load.started' },
      { name: 'receive.inventory.load.completed', operation_count: 1 },
    ])
    expect(JSON.stringify(enabled.traceEvents)).not.toContain('error_name')
    expect(enabled.publications.at(-1)?.pending).toBeNull()
    expect(enabled.incidents.decisions.find(
      ({ scope }) => scope.scopeKind === 'retained_action',
    )).toMatchObject({
      decision: {
        kind: 'incident',
        outcome: 'failed',
      },
    })
    expect(enabled.incidents.facts.find(
      ({ scope }) => scope.scopeKind === 'retained_action',
    )).toMatchObject({
      fact: {
        kind: 'native_output_failure',
        stage: 'publication',
        payload: { nativeOutputFailure: { nativeClass: 'abort' } },
      },
      relation: 'contributor',
    })

    const disabled = retainedHarness(() => Promise.resolve(testInventory([])))
    await disabled.coordinator.load()
    expect(disabled.traceEvents).toEqual([])
  })

})

describe('retained action incident ownership', () => {
  it.each([
    ['continue', 'resume-receive'],
    ['save', 'save-artifact'],
    ['redownload', 'retry-download'],
    ['discard', 'needs-attention'],
    ['delete', 'cleanup-expired'],
  ] as const)(
    'publishes pending %s authority before invoking the retained inventory',
    async (action, continuation) => {
      const retained = operation([action], continuation)
      const actionGate = deferred<V2RetainedReceiveActionResult>()
      let latestPublished: RetainedHarness['publications'][number] | undefined
      let snapshotDuringAct: RetainedHarness['publications'][number] | undefined
      const inventory = testInventory([retained], () => {
        snapshotDuringAct = latestPublished
        return actionGate.promise
      })
      const joined = action === 'continue'
        ? ({ protocolSessionId: 'session' } as unknown as V2JoinedBrowserShare)
        : undefined
      const harness = retainedHarness(
        () => Promise.resolve(inventory),
        joined,
        { onPublish: published => { latestPublished = published } },
      )

      await harness.coordinator.load()
      harness.coordinator.perform(retained, action)

      expect(snapshotDuringAct).toMatchObject({
        kind: 'ready',
        operations: [retained],
        pending: { operationId: retained.operationId, action },
      })
      expect(Object.isFrozen(snapshotDuringAct?.pending)).toBe(true)
      expect(harness.coordinator.pending).toBe(true)

      actionGate.resolve(Object.freeze({ kind: 'completed' }))
      await turns()
      expect(harness.publications.at(-1)).toMatchObject({
        kind: 'ready',
        pending: null,
      })
      expect(harness.coordinator.pending).toBe(false)
    },
  )

  it('aborts and clears a pending action before replacing its inventory', async () => {
    const retained = operation(['save'], 'save-artifact')
    const actionGate = deferred<V2RetainedReceiveActionResult>()
    let actionSignal: AbortSignal | undefined
    const inventory = testInventory([retained], (_operation, _action, signal) => {
      actionSignal = signal
      return actionGate.promise
    })
    const harness = retainedHarness(() => Promise.resolve(inventory))

    await harness.coordinator.load()
    harness.coordinator.perform(retained, 'save')
    await harness.coordinator.load()

    expect(actionSignal?.aborted).toBe(true)
    expect(harness.coordinator.pending).toBe(false)
    expect(harness.publications.at(-1)).toMatchObject({
      kind: 'ready',
      pending: null,
    })

    actionGate.resolve(Object.freeze({ kind: 'completed' }))
    await turns()
    expect(harness.actionErrors).toHaveLength(0)
  })

  it.each([
    ['continue', 'resume-receive'],
    ['save', 'save-artifact'],
    ['redownload', 'retry-download'],
    ['discard', 'save-artifact'],
    ['delete', 'cleanup-expired'],
  ] as const)(
    'constructs no retained trace payloads for disabled %s transitions',
    async (action, continuation) => {
      const retained = operation([action], continuation)
      const inventory = testInventory(
        [retained],
        () => Promise.reject(new DOMException('private native detail', 'AbortError')),
      )
      const harness = retainedHarness(() => Promise.resolve(inventory))

      await harness.coordinator.load()
      harness.coordinator.perform(retained, action)
      await turns()

      expect(harness.traceEvents).toEqual([])
    },
  )

  it('treats a blocked continuation as a visible retained-action failure', async () => {
    const retained = operation(['continue'], 'resume-receive')
    const inventory = testInventory([retained])
    const harness = retainedHarness(() => Promise.resolve(inventory))

    await harness.coordinator.load()
    harness.coordinator.perform(retained, 'continue')

    expect(harness.incidents.decisions.at(-1)).toMatchObject({
      scope: { scopeKind: 'retained_action' },
      decision: {
        kind: 'incident',
        boundary: 'retained_action',
        outcome: 'failed',
      },
    })
    expect(harness.incidents.facts.at(-1)).toMatchObject({
      fact: { kind: 'unclassified', stage: 'retained_action' },
      relation: 'contributor',
    })
    expect(harness.actionErrors).toHaveLength(1)
    expect(harness.incidents.owners.at(-1)?.isClosed()).toBe(true)
  })

  it('uses a deliberate exclusion for a successful discard action', async () => {
    const retained = operation(['discard'], 'needs-attention')
    const inventory = testInventory(
      [retained],
      () => Promise.resolve(Object.freeze({ kind: 'completed' })),
    )
    const harness = retainedHarness(() => Promise.resolve(inventory))

    await harness.coordinator.load()
    harness.coordinator.perform(retained, 'discard')
    await turns()

    const actionDecision = harness.incidents.decisions.find(
      ({ scope }) => scope.scopeKind === 'retained_action',
    )
    expect(actionDecision).toMatchObject({
      decision: {
        kind: 'excluded',
        boundary: 'retained_action',
        reason: 'user_discarded',
      },
    })
    expect(harness.actionErrors).toHaveLength(0)
    expect(harness.incidents.owners.every((owner) => owner.isClosed())).toBe(true)
  })

  it('names cancellation on the pending retained action owner', async () => {
    const retained = operation(['save'], 'save-artifact')
    const action = deferred<V2RetainedReceiveActionResult>()
    const inventory = testInventory(
      [retained],
      () => action.promise,
    )
    const harness = retainedHarness(() => Promise.resolve(inventory))

    await harness.coordinator.load()
    harness.coordinator.perform(retained, 'save')
    expect(harness.publications.at(-1)?.pending).toEqual({
      operationId: retained.operationId,
      action: 'save',
    })
    harness.coordinator.cancelPending(
      new DOMException('cancelled', 'AbortError'),
    )
    expect(harness.publications.at(-1)?.pending).toBeNull()

    const actionDecision = harness.incidents.decisions.find(
      ({ scope }) => scope.scopeKind === 'retained_action',
    )
    expect(actionDecision).toMatchObject({
      decision: {
        kind: 'excluded',
        boundary: 'retained_action',
        reason: 'cancelled',
      },
    })
    expect(harness.incidents.owners.find(
      (owner) => owner.identity.scopeKind === 'retained_action',
    )?.isClosed()).toBe(true)

    action.resolve(Object.freeze({ kind: 'completed' }))
    await turns()
    expect(harness.actionErrors).toHaveLength(0)
  })

  it('does not present a continuation failure cancelled during detach cleanup', async () => {
    const retained = operation(['continue'], 'resume-receive')
    const detach = deferred<void>()
    const runtime = {
      intent: Object.freeze({}),
      detach: () => detach.promise,
    } as unknown as V2BoundReceiveOperation
    const inventory = testInventory(
      [retained],
      () => Promise.resolve(Object.freeze({
        kind: 'receive-continuation',
        runtime,
      })),
    )
    const joined = {
      descriptor: {
        shareInstanceId: 'share',
        syntheticRootId: 'root',
      },
      protocolSessionId: 'session',
    } as unknown as V2JoinedBrowserShare
    const harness = retainedHarness(
      () => Promise.resolve(inventory),
      joined,
    )

    await harness.coordinator.load()
    harness.coordinator.perform(retained, 'continue')
    await turns()
    harness.coordinator.cancelPending(
      new DOMException('cancelled during detach', 'AbortError'),
    )
    detach.resolve(undefined)
    await turns()

    const actionDecisions = harness.incidents.decisions.filter(
      ({ scope }) => scope.scopeKind === 'retained_action',
    )
    expect(actionDecisions).toMatchObject([
      {
        decision: {
          kind: 'excluded',
          boundary: 'retained_action',
          reason: 'cancelled',
        },
      },
    ])
    expect(harness.actionErrors).toHaveLength(0)
    expect(harness.incidents.owners.every((owner) => owner.isClosed())).toBe(true)
  })

  it('keeps continuation validation primary and detach failure consequential', async () => {
    const retained = operation(['continue'], 'resume-receive')
    const runtime = {
      intent: Object.freeze({}),
      detach: () => Promise.reject(new Error('detach failed')),
    } as unknown as V2BoundReceiveOperation
    const inventory = testInventory(
      [retained],
      () => Promise.resolve(Object.freeze({
        kind: 'receive-continuation',
        runtime,
      })),
    )
    const joined = {
      descriptor: {
        shareInstanceId: 'share',
        syntheticRootId: 'root',
      },
      protocolSessionId: 'session',
    } as unknown as V2JoinedBrowserShare
    const harness = retainedHarness(
      () => Promise.resolve(inventory),
      joined,
    )

    await harness.coordinator.load()
    harness.coordinator.perform(retained, 'continue')
    await turns()

    const actionFacts = harness.incidents.facts.filter(
      ({ scope }) => scope.scopeKind === 'retained_action',
    )
    expect(actionFacts).toMatchObject([
      {
        fact: { kind: 'unclassified', stage: 'retained_action' },
        relation: 'contributor',
      },
      {
        fact: { kind: 'unclassified', stage: 'detach' },
        relation: 'consequence',
      },
    ])
    expect(harness.incidents.decisions.find(
      ({ scope }) => scope.scopeKind === 'retained_action',
    )).toMatchObject({
      decision: {
        kind: 'incident',
        boundary: 'retained_action',
        outcome: 'failed',
      },
    })
    expect(harness.actionErrors).toHaveLength(1)
    expect(harness.incidents.owners.every((owner) => owner.isClosed())).toBe(true)
  })
})

interface RetainedHarness {
  readonly coordinator: RetainedInventoryCoordinator
  readonly incidents: RecordingRetainedIncidents
  readonly publications: Array<
    Parameters<RetainedInventoryCoordinatorOptions['publish']>[0]
  >
  readonly actionErrors: unknown[]
  readonly traceEvents: unknown[]
}

type RetainedInventoryCoordinatorOptions =
  ConstructorParameters<typeof RetainedInventoryCoordinator>[0]

function retainedHarness(
  list: (signal: AbortSignal) => PromiseLike<V2RetainedReceiveInventory>,
  joined?: V2JoinedBrowserShare,
  options: Readonly<{
    traceEnabled?: boolean
    onPublish?: (retained: Parameters<RetainedInventoryCoordinatorOptions['publish']>[0]) => void
  }> = {},
): RetainedHarness {
  const incidents = new RecordingRetainedIncidents()
  const publications: Array<
    Parameters<RetainedInventoryCoordinatorOptions['publish']>[0]
  > = []
  const actionErrors: unknown[] = []
  const traceEvents: unknown[] = []
  const receive = {
    retained: { list },
  } as V2ReceiveCompositionPort
  const coordinator = new RetainedInventoryCoordinator({
    receive,
    isDisposed: () => false,
    currentJoinedShare: () => joined,
    continuationBlocked: () => false,
    adoptContinuation: async () => undefined,
    ownsRuntime: () => false,
    publish: (retained) => {
      publications.push(retained)
      options.onPublish?.(retained)
    },
    trace: Object.freeze({
      current: options.traceEnabled === true
        ? (event: unknown) => traceEvents.push(event)
        : undefined,
    }),
    onActionError: (error) => actionErrors.push(error),
    incidents,
  })
  return { coordinator, incidents, publications, actionErrors, traceEvents }
}

function operation(
  actions: readonly V2RetainedReceiveAction[],
  continuation: V2RetainedReceiveOperation['continuation'],
): V2RetainedReceiveOperation {
  return Object.freeze({
    operationId: 'operation',
    receiveIntentDigest: 'intent',
    lifecycleGeneration: 1n,
    lifecycle: Object.freeze({ kind: 'needs-attention' }),
    continuation,
    actions: Object.freeze([...actions]),
  }) as V2RetainedReceiveOperation
}

function testInventory(
  operations: readonly V2RetainedReceiveOperation[],
  act: (
    operation: V2RetainedReceiveOperation,
    action: V2RetainedReceiveAction,
    signal: AbortSignal,
    failures?: Parameters<V2RetainedReceiveInventory['act']>[3],
  ) => PromiseLike<V2RetainedReceiveActionResult> = () =>
    Promise.reject(new Error('action was not expected')),
  presentationFailures: readonly FailureFact[] = Object.freeze([]),
): V2RetainedReceiveInventory {
  return Object.freeze({
    operations: Object.freeze([...operations]),
    presentationFailures: Object.freeze([...presentationFailures]),
    act,
    close: () => undefined,
  })
}

class RecordingRetainedIncidents {
  readonly #issuer = createIncidentScopeIssuer()
  readonly owners: IncidentScopeOwner[] = []
  readonly decisions: Array<{
    readonly scope: IncidentScopeIdentity
    readonly decision: PresentationDecision
  }> = []
  readonly facts: Array<{
    readonly scope: IncidentScopeIdentity
    readonly fact: FailureFact
    readonly relation: FailureFactRelation
  }> = []
  onDecision: (() => void) | undefined

  openScope(kind: IncidentScopeKind): IncidentScopeOwner {
    const owner = this.#issuer.open(kind, {
      factRecorded: (observation) => {
        this.facts.push({
          scope: observation.ref.scope,
          fact: observation.fact,
          relation: observation.relation,
        })
      },
    })
    this.owners.push(owner)
    return owner
  }

  submitDecision(
    scope: IncidentScopeHandle,
    decision: PresentationDecision,
  ): void {
    this.decisions.push({ scope: scope.identity, decision })
    this.onDecision?.()
  }
}

interface Deferred<T> {
  readonly promise: Promise<T>
  resolve(value: T): void
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((complete) => {
    resolve = complete
  })
  return { promise, resolve }
}

async function turns(): Promise<void> {
  for (let index = 0; index < 16; index += 1) await Promise.resolve()
}
