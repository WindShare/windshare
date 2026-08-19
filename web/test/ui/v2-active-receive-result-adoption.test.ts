import { describe, expect, it } from 'vitest'

import { fileId } from '../../src/catalog/model'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import type { V2ConnectivityActivation } from '../../src/connectivity/v2-receiver-policy'
import {
  initialReceiveLifecycleState,
  nextReceiveLifecycleState,
  type ReceiveLifecycleState,
} from '../../src/output/workspace'
import {
  createDirectAtomicPlan,
  createManagedAtomicReservation,
  createReceiveIntent,
  createSelectionSpec,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import type { SelectionMeasure } from '../../src/transfer/measure'
import type { V2PlanExecutionAuthority } from '../../src/transfer/output-session'
import {
  TransferFailureAccumulator,
  transferWorkerSettlement,
  type TransferFileOutcome,
  type TransferWorkerSettlement,
} from '../../src/transfer/outcome'
import type { TransferJobResult } from '../../src/transfer/v2-job'
import {
  ActiveReceiveCoordinator,
  type ActiveReceiveJoinedShare,
} from '../../src/ui/controller/active-receive'
import type {
  LifecycleUserAction,
  V2ActiveReceiveControl,
} from '../../src/ui/v2-lifecycle-presentation'
import { V2OutputPresentationController } from '../../src/ui/v2-output'
import type {
  V2BoundReceiveOperation,
  V2LifecycleMutation,
  V2StartedArtifactAuthority,
} from '../../src/ui/v2-receive-runtime'
import {
  COMPLETE_DISCOVERY,
  environment,
  identity,
  managedTarget,
  projection,
  singleFileProof,
} from '../output/planning/fixture'

const FILE_ID = identity(2)
const TRANSFER_MEASURE: SelectionMeasure = Object.freeze({
  discoveredFiles: 1,
  discoveredBytes: 128n,
  discovery: 'complete',
  sizeClass: 'small',
})
const UNAVAILABLE_PLANS: V2PlanExecutionAuthority = Object.freeze({
  openDirectTree: unavailableExecution,
  openDirectAtomic: unavailableExecution,
  openWorkspaceOriginal: unavailableExecution,
  prepareWorkspaceZip: unavailableExecution,
  preparePortable: unavailableExecution,
  settleExecutionAdmissionFailure: unavailableExecution,
  recordSettlementUnknown: unavailableExecution,
})

describe('active receive result adoption', () => {
  it.each([
    ['revision-conflict', 'Resume revision conflict', /another source revision/u],
    ['checkpoint-invalid', 'Invalid resume checkpoint', /invalid local resume checkpoint/u],
    ['destination-collision', 'Existing destinations prevented completion', /existing destination/u],
  ] as const)('publishes the exact %s result behind the live projection fence', async (
    outcome,
    title,
    line,
  ) => {
    const fixture = await resultAdoptionFixture('live')

    fixture.settle(outcome)
    await waitFor(() => fixture.outputs.getSnapshot().transferResultPresentation?.title === title)

    expect(fixture.outputs.getSnapshot().projection).not.toBeNull()
    expect(fixture.outputs.getSnapshot().transferResultPresentation).toMatchObject({
      title,
      tone: 'warning',
      lines: [expect.stringMatching(line)],
    })
    expect(fixture.failures).toEqual([])
    await fixture.close()
  })

  it('publishes the exact result behind the retained null-projection fence', async () => {
    const fixture = await resultAdoptionFixture('retained')

    fixture.settle('destination-collision')
    await waitFor(() => fixture.outputs.getSnapshot().transferResultPresentation !== null)

    expect(fixture.outputs.getSnapshot().projection).toBeNull()
    expect(fixture.outputs.getSnapshot().transferResultPresentation).toMatchObject({
      title: 'Existing destinations prevented completion',
      tone: 'warning',
      lines: [expect.stringMatching(/existing destination prevented/u)],
    })
    expect(fixture.failures).toEqual([])
    await fixture.close()
  })
})

async function resultAdoptionFixture(mode: 'live' | 'retained'): Promise<Readonly<{
  outputs: V2OutputPresentationController<V2StartedArtifactAuthority>
  failures: unknown[]
  settle: (outcome: TransferFileOutcome) => void
  close: () => Promise<void>
}>> {
  const { outputs, intent, projectionEpoch } = await preparedOutput()
  const lifecycle = initialReceiveLifecycleState({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
  })
  if (mode === 'live') {
    expect(outputs.adoptReceiveIntent(projectionEpoch, intent, lifecycle)).toBe(true)
  } else {
    outputs.adoptRetainedReceiveIntent(intent, lifecycle)
  }

  const runtime = new ResultRuntime(intent, lifecycle)
  const share = new ResultShare(intent)
  const failures: unknown[] = []
  const coordinator = new ActiveReceiveCoordinator({
    outputs,
    ownsJoinedShare: candidate => candidate === share,
    onProgress: () => undefined,
    onTrace: () => undefined,
    onActionError: error => failures.push(error),
    onFailure: error => failures.push(error),
  })
  coordinator.adopt({
    joined: share,
    selection: new V2SelectionPolicy(true).snapshot(),
    runtime,
  })

  return Object.freeze({
    outputs,
    failures,
    settle: outcome => {
      share.resolve(Object.freeze({
        worker: workerWithFileOutcome(outcome),
        lifecycle: resumableLifecycle(lifecycle),
        measure: TRANSFER_MEASURE,
        transferJobId: runtime.transferJobId,
        intent,
      }))
    },
    close: () => coordinator.reset(new DOMException('test completed', 'AbortError')),
  })
}

async function preparedOutput(): Promise<Readonly<{
  outputs: V2OutputPresentationController<V2StartedArtifactAuthority>
  intent: ReceiveIntent
  projectionEpoch: ReturnType<typeof projection>['epoch']
}>> {
  const selection = await createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(3),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const projected = projection(selection, singleFileProof(), 128n, 4n)
  const outputs = new V2OutputPresentationController<V2StartedArtifactAuthority>()
  await outputs.updateProjection(
    Object.freeze({ projection: projected, discovery: COMPLETE_DISCOVERY }),
    environment({ targets: [managedTarget()] }),
  )
  await outputs.activateArtifact('download-original', () => INERT_ARTIFACT_AUTHORITY)
  const action = outputs.getSnapshot().chosenAction
  if (action?.artifact === null || action?.artifact === undefined ||
      action.plan.kind !== 'direct-atomic') {
    throw new Error('result-adoption fixture did not select a direct atomic artifact')
  }
  const nameAuthority = action.plan.target.guarantees.nameAuthority
  if (nameAuthority === 'browser-chosen') {
    throw new Error('managed result-adoption fixture requires an application-owned name')
  }
  const reservation = await createManagedAtomicReservation({
    operationId: identity(10),
    reservationId: identity(11),
    artifact: action.artifact,
    authorityRef: identity(12, 32),
    nameAuthority,
    requestedName: action.suggestedName ?? 'report.txt',
    reservedName: action.suggestedName ?? 'report.txt',
    collisionIndex: 0,
  })
  const plan = await createDirectAtomicPlan(action.artifact, reservation)
  const intent = await createReceiveIntent({
    selection,
    artifact: action.artifact,
    plan,
  })
  return Object.freeze({
    outputs,
    intent,
    projectionEpoch: projected.epoch,
  })
}

const INERT_ARTIFACT_AUTHORITY: V2StartedArtifactAuthority = Object.freeze({
  finalize: unavailableExecution,
  release: () => undefined,
})

class ResultShare implements ActiveReceiveJoinedShare {
  readonly #result = deferred<TransferJobResult>()
  readonly #expectedIntent: ReceiveIntent

  constructor(expectedIntent: ReceiveIntent) {
    this.#expectedIntent = expectedIntent
  }

  beginDownloadConnectivity(): V2ConnectivityActivation {
    return Object.freeze({
      routes: Object.freeze({
        active: true,
        allows: () => true,
        assertActive: () => undefined,
        subscribe: () => () => undefined,
      }),
      close: () => undefined,
    })
  }

  transferJob(
    ...args: Parameters<ActiveReceiveJoinedShare['transferJob']>
  ): ReturnType<ActiveReceiveJoinedShare['transferJob']> {
    const [plans, intent] = args
    if (plans !== UNAVAILABLE_PLANS || intent !== this.#expectedIntent) {
      return unexpectedFixtureCall('coordinator changed the adopted transfer authority', {
        plans,
        intent,
      })
    }
    return Object.freeze({
      run: () => this.#result.promise,
    })
  }

  resolve(result: TransferJobResult): void {
    this.#result.resolve(result)
  }
}

class ResultRuntime implements V2BoundReceiveOperation {
  readonly plans = UNAVAILABLE_PLANS
  readonly transferJobId = identity(20)
  readonly activeControls = Object.freeze([] as const)
  readonly initialWorkspaceUsage = null
  readonly intent: ReceiveIntent
  readonly lifecycle: ReceiveLifecycleState

  constructor(intent: ReceiveIntent, lifecycle: ReceiveLifecycleState) {
    this.intent = intent
    this.lifecycle = lifecycle
  }

  interrupt(
    control: V2ActiveReceiveControl,
    transfer: AbortController,
  ): never {
    return unexpectedFixtureCall('result-adoption fixture cannot be interrupted', {
      control,
      transfer,
    })
  }

  startLifecycleAction(
    action: Exclude<LifecycleUserAction, V2ActiveReceiveControl>,
    lifecycle: ReceiveLifecycleState,
  ): V2LifecycleMutation {
    return unexpectedFixtureCall('result-adoption fixture has no lifecycle action', {
      action,
      lifecycle,
    })
  }

  observeExpiry(lifecycle: ReceiveLifecycleState): Promise<V2LifecycleMutation> {
    return unexpectedFixtureCall('result-adoption fixture cannot expire', lifecycle)
  }

  resolveWorkspaceUsage(lifecycle: ReceiveLifecycleState): null {
    if (lifecycle.operationId !== this.lifecycle.operationId) {
      return unexpectedFixtureCall('coordinator changed the adopted operation', lifecycle)
    }
    return null
  }

  settleTransferAdmissionFailure(reason: unknown): V2LifecycleMutation {
    return unexpectedFixtureCall('settled result cannot enter admission failure', reason)
  }

  detach(): void {}
}

function unexpectedFixtureCall(message: string, cause: unknown): never {
  throw new Error(message, { cause })
}

function workerWithFileOutcome(outcome: TransferFileOutcome): TransferWorkerSettlement {
  const failures = new TransferFailureAccumulator()
  failures.record(Object.freeze({
    kind: 'file',
    fileId: fileId(FILE_ID),
    reason: new Error('bounded diagnostic is not presentation authority'),
  }), outcome)
  return transferWorkerSettlement('CompletedWithErrors', failures.snapshot())
}

function resumableLifecycle(initial: ReceiveLifecycleState): ReceiveLifecycleState {
  return nextReceiveLifecycleState(initial, {
    kind: 'resumable-receive',
    checkpointSetDigest: identity(30, 32),
    completedFileCount: 0n,
    completedBytes: 0n,
    expiresAt: Date.now() + 60_000,
  })
}

function unavailableExecution(): Promise<never> {
  return Promise.reject(new Error('fixture execution authority is unavailable'))
}

interface Deferred<Value> {
  readonly promise: Promise<Value>
  readonly resolve: (value: Value) => void
}

function deferred<Value>(): Deferred<Value> {
  let resolve!: (value: Value) => void
  return {
    promise: new Promise<Value>((accept) => {
      resolve = accept
    }),
    resolve,
  }
}

async function waitFor(predicate: () => boolean): Promise<void> {
  for (let attempt = 0; attempt < 50; attempt += 1) {
    if (predicate()) return
    await Promise.resolve()
  }
  throw new Error('condition did not become true')
}
