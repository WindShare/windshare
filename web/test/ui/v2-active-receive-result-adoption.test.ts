import { describe, expect, it } from 'vitest'

import { fileId } from '../../src/catalog/model'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import type { V2ConnectivityActivation } from '../../src/connectivity/v2-receiver-policy'
import {
  initialReceiveLifecycleState,
  nextReceiveLifecycleState,
  type ReceiveLifecycleState,
} from '../../src/output/workspace'
import type { CompatibleNameRepairSummary } from '../../src/output/file-system-access/compatible-name/model'
import {
  materializationRouteIdentity,
  offerArtifacts,
  type ArtifactChoice,
} from '../../src/output/planning'
import {
  createDirectAtomicPlan,
  createManagedAtomicReservation,
  createOriginalFileArtifact,
  createReceiveIntent,
  createSelectionSpec,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import { FaultScope, SourceFaultCode, sourceFault } from '../../src/transfer/fault'
import { normalizedV2FileTransferFault } from '../../src/transfer/job/failures'
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
import type { V2ActiveCompatibleNameRepairProjection } from '../../src/ui/controller/contracts'
import type {
  LifecycleUserAction,
  V2ActiveReceiveControl,
} from '../../src/ui/v2-lifecycle-presentation'
import { V2OutputPresentationController } from '../../src/ui/v2-output'
import type {
  V2BoundReceiveOperation,
  V2LifecycleMutation,
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
  openDirectResumableZip: unavailableExecution,
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

  it('keeps one live repair notice updated and qualifies the first terminal publication', async () => {
    const repair = new TestActiveRepairProjection()
    repair.publish(repairSummary(0, [], 'active'))
    const fixture = await resultAdoptionFixture('live', repair)

    expect(fixture.outputs.getSnapshot().lifecyclePresentation?.compatibleNameRepair).toMatchObject({
      replacementCount: 0,
      actionMode: 'receiving-notice',
      scriptName: 'restore.windshare-abc234.ps1',
      sidecarName: 'restore.windshare-abc234.data',
      runCommand: null,
    })

    const active = repairSummary(2, [
      ['folder', 'pyvenv.cfg'],
      ['folder', 'nested'],
    ], 'active')
    repair.publish(active)
    expect(fixture.outputs.getSnapshot().lifecyclePresentation?.compatibleNameRepair).toMatchObject({
      replacementCount: 2,
      logicalPathSample: ['folder/pyvenv.cfg', 'folder/nested'],
      actionMode: 'receiving-notice',
    })

    const publishedTitles: string[] = []
    const unsubscribe = fixture.outputs.subscribe(() => {
      const snapshot = fixture.outputs.getSnapshot()
      if (snapshot.lifecycle?.kind === 'published') {
        publishedTitles.push(snapshot.lifecyclePresentation?.title ?? '')
      }
    })
    const terminalRepair = repairSummary(2, active.logicalPathSample, 'completed')
    fixture.settleResult(Object.freeze({
      worker: workerWithFileOutcome('destination-collision'),
      lifecycle: publishedLifecycle(fixture.initialLifecycle),
      measure: TRANSFER_MEASURE,
      transferJobId: fixture.runtime.transferJobId,
      intent: fixture.intent,
      repairSummary: terminalRepair,
    }))
    await waitFor(() => fixture.outputs.getSnapshot().lifecycle?.kind === 'published')
    unsubscribe()

    expect(publishedTitles).not.toHaveLength(0)
    expect(publishedTitles.every(title => title === 'Completed with compatible names')).toBe(true)
    expect(fixture.outputs.getSnapshot().lifecyclePresentation?.compatibleNameRepair)
      .toMatchObject({ actionMode: 'routine-restoration' })
    expect(repair.subscriptionCount).toBe(1)
    await fixture.close()
    expect(repair.unsubscribed).toBe(true)
  })

  it('clears a live zero-count notice before publishing an ordinary empty-cleanup result', async () => {
    const repair = new TestActiveRepairProjection()
    repair.publish(repairSummary(0, [], 'active'))
    const fixture = await resultAdoptionFixture('live', repair)
    expect(fixture.outputs.getSnapshot().repairSummary?.committedCount).toBe(0)

    const publishedSnapshots: Array<Readonly<{ title: string; hasRepair: boolean }>> = []
    const unsubscribe = fixture.outputs.subscribe(() => {
      const snapshot = fixture.outputs.getSnapshot()
      if (snapshot.lifecycle?.kind === 'published') {
        publishedSnapshots.push(Object.freeze({
          title: snapshot.lifecyclePresentation?.title ?? '',
          hasRepair: snapshot.repairSummary !== null,
        }))
      }
    })
    fixture.settleResult(Object.freeze({
      worker: transferWorkerSettlement('Succeeded', new TransferFailureAccumulator().snapshot()),
      lifecycle: publishedLifecycle(fixture.initialLifecycle),
      measure: TRANSFER_MEASURE,
      transferJobId: fixture.runtime.transferJobId,
      intent: fixture.intent,
    }))
    await waitFor(() => fixture.outputs.getSnapshot().transferResultPresentation !== null)
    unsubscribe()

    expect(publishedSnapshots).not.toHaveLength(0)
    expect(publishedSnapshots.every(snapshot =>
      snapshot.title === 'Saved' && !snapshot.hasRepair)).toBe(true)
    expect(fixture.outputs.getSnapshot()).toMatchObject({
      repairSummary: null,
      lifecyclePresentation: { compatibleNameRepair: null },
      transferResultPresentation: { title: 'Transfer completed' },
    })
    await fixture.close()
  })

  it('keeps the restoration command labeled for abnormal stops while resumably paused', async () => {
    const repair = new TestActiveRepairProjection()
    repair.publish(repairSummary(1, [['folder', 'pyvenv.cfg']], 'active'))
    const fixture = await resultAdoptionFixture('live', repair)

    fixture.settle('destination-collision')
    await waitFor(() => fixture.outputs.getSnapshot().lifecycle?.kind === 'resumable-receive')

    expect(fixture.outputs.getSnapshot().lifecyclePresentation?.compatibleNameRepair).toMatchObject({
      replacementCount: 1,
      actionMode: 'abnormal-stop-recovery',
      actionTitle: 'Restore names after stopping',
    })
    await fixture.close()
  })

  it('adopts a repair projection activated by the first repaired descendant', async () => {
    const fixture = await resultAdoptionFixture('live', undefined, true)
    const repair = new TestActiveRepairProjection()
    repair.publish(repairSummary(0, [], 'active'))

    expect(fixture.outputs.getSnapshot().lifecyclePresentation?.compatibleNameRepair).toBeNull()
    fixture.runtime.activateRepairProjection(repair)

    expect(fixture.outputs.getSnapshot().lifecyclePresentation?.compatibleNameRepair).toMatchObject({
      replacementCount: 0,
      logicalPathSample: [],
      actionMode: 'receiving-notice',
    })
    expect(fixture.runtime.activationSubscriptionCount).toBe(1)
    expect(repair.subscriptionCount).toBe(1)

    fixture.settle('destination-collision')
    await waitFor(() => fixture.outputs.getSnapshot().lifecycle?.kind === 'resumable-receive')
    await fixture.close()
    expect(fixture.runtime.activationUnsubscribed).toBe(true)
    expect(repair.unsubscribed).toBe(true)
  })
})

async function resultAdoptionFixture(
  mode: 'live' | 'retained',
  repairProjection?: V2ActiveCompatibleNameRepairProjection,
  enableLateRepairActivation = false,
): Promise<Readonly<{
  outputs: V2OutputPresentationController
  failures: unknown[]
  settle: (outcome: TransferFileOutcome) => void
  settleResult: (result: TransferJobResult) => void
  intent: ReceiveIntent
  initialLifecycle: ReceiveLifecycleState
  runtime: ResultRuntime
  close: () => Promise<void>
}>> {
  const { outputs, intent, choice } = await preparedOutput()
  const lifecycle = initialReceiveLifecycleState({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
  })
  if (mode === 'live') {
    expect(outputs.adoptReceiveIntent(choice, intent, lifecycle)).toBe(true)
  } else {
    outputs.adoptRetainedReceiveIntent(intent, lifecycle)
  }

  const runtime = new ResultRuntime(intent, lifecycle, enableLateRepairActivation)
  const share = new ResultShare(intent)
  const failures: unknown[] = []
  const coordinator = new ActiveReceiveCoordinator({
    outputs,
    ownsJoinedShare: candidate => candidate === share,
    onProgress: () => undefined,
    onActionError: error => failures.push(error),
    onFailure: error => failures.push(error),
  })
  coordinator.adopt({
    joined: share,
    selection: new V2SelectionPolicy(true).snapshot(),
    runtime,
    ...(repairProjection === undefined ? {} : { repairProjection }),
  })

  return Object.freeze({
    outputs,
    failures,
    intent,
    initialLifecycle: lifecycle,
    runtime,
    settle: outcome => {
      share.resolve(Object.freeze({
        worker: workerWithFileOutcome(outcome),
        lifecycle: resumableLifecycle(lifecycle),
        measure: TRANSFER_MEASURE,
        transferJobId: runtime.transferJobId,
        intent,
      }))
    },
    settleResult: result => share.resolve(result),
    close: () => coordinator.reset(new DOMException('test completed', 'AbortError')),
  })
}

async function preparedOutput(): Promise<Readonly<{
  outputs: V2OutputPresentationController
  intent: ReceiveIntent
  choice: ArtifactChoice
}>> {
  const selection = await createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(3),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const projected = projection(selection, singleFileProof(), 128n, 4n)
  const state = Object.freeze({ projection: projected, discovery: COMPLETE_DISCOVERY })
  const offers = await offerArtifacts(
    projected,
    COMPLETE_DISCOVERY,
    environment({ targets: [managedTarget()] }),
  )
  if (offers.kind !== 'artifact-actions' || offers.primary.route.kind !== 'direct-atomic') {
    throw new Error('result-adoption fixture did not select a direct atomic artifact')
  }
  const offered = offers.primary
  if (offered.route.kind !== 'direct-atomic') {
    throw new Error('result-adoption fixture lost its direct atomic route')
  }
  const artifact = await createOriginalFileArtifact({
    fileId: FILE_ID,
    sourcePath: 'report.txt',
    suggestedName: offered.suggestedName ?? 'report.txt',
  })
  const nameAuthority = offered.route.target.guarantees.nameAuthority
  if (nameAuthority === 'browser-chosen') {
    throw new Error('managed result-adoption fixture requires an application-owned name')
  }
  const reservation = await createManagedAtomicReservation({
    operationId: identity(10),
    reservationId: identity(11),
    artifact,
    authorityRef: identity(12, 32),
    nameAuthority,
    requestedName: offered.suggestedName ?? 'report.txt',
    reservedName: offered.suggestedName ?? 'report.txt',
    collisionIndex: 0,
  })
  const plan = await createDirectAtomicPlan(artifact, reservation)
  const intent = await createReceiveIntent({
    selection,
    artifact,
    plan,
  })
  const outputs = new V2OutputPresentationController()
  outputs.updateProjection(1, state, offers)
  outputs.updateActivation(Object.freeze({
    kind: 'terminal',
    activationId: identity(13),
    authenticatedShareInstanceId: selection.shareInstance,
    selectionDigest: selection.digest,
    choice: offered.choice,
    installedRoute: materializationRouteIdentity(offered.route),
    preClickRanking: Object.freeze([offered.choice.choiceId]),
    observation: Object.freeze({
      revision: 1,
      protocolSessionId: identity(14),
      projectionEpoch: projected.epoch,
    }),
    outcome: Object.freeze({ kind: 'bound-operation', operationId: intent.operationId }),
  }))
  return Object.freeze({
    outputs,
    intent,
    choice: offered.choice,
  })
}

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
  readonly subscribeRepairProjectionActivation?: NonNullable<
    V2BoundReceiveOperation['subscribeRepairProjectionActivation']
  >
  readonly intent: ReceiveIntent
  readonly lifecycle: ReceiveLifecycleState
  #repairActivationListener:
    ((source: V2ActiveCompatibleNameRepairProjection) => void) | undefined
  activationSubscriptionCount = 0
  activationUnsubscribed = false

  constructor(
    intent: ReceiveIntent,
    lifecycle: ReceiveLifecycleState,
    enableLateRepairActivation: boolean,
  ) {
    this.intent = intent
    this.lifecycle = lifecycle
    if (enableLateRepairActivation) {
      this.subscribeRepairProjectionActivation = listener => {
        this.activationSubscriptionCount += 1
        this.#repairActivationListener = listener
        return () => {
          if (this.#repairActivationListener === listener) {
            this.#repairActivationListener = undefined
          }
          this.activationUnsubscribed = true
        }
      }
    }
  }

  activateRepairProjection(source: V2ActiveCompatibleNameRepairProjection): void {
    this.#repairActivationListener?.(source)
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
    classification: normalizedV2FileTransferFault(
      sourceFault(FaultScope.FileLocal, SourceFaultCode.Unavailable),
    ).diagnostic.classification,
  }), 1n, outcome)
  return transferWorkerSettlement('CompletedWithErrors', failures.snapshot())
}

function resumableLifecycle(initial: ReceiveLifecycleState): ReceiveLifecycleState {
  return nextReceiveLifecycleState(initial, {
    kind: 'resumable-receive',
    payloadKind: 'file-set',
    checkpointSetDigest: identity(30, 32),
    completedFileCount: 0n,
    completedBytes: 0n,
    selectionFacts: Object.freeze({
      discoveredFileCount: 0n,
      discoveredBytes: 0n,
      discovery: 'complete',
    }),
    expiresAt: Date.now() + 60_000,
  })
}

function publishedLifecycle(initial: ReceiveLifecycleState): ReceiveLifecycleState {
  return nextReceiveLifecycleState(initial, {
    kind: 'published',
    receiptDigest: identity(31, 32),
    cleanupState: 'clean',
  })
}

function repairSummary(
  committedCount: number,
  logicalPathSample: readonly (readonly string[])[],
  footerState: NonNullable<CompatibleNameRepairSummary['latestObservedFooter']>['state'],
): CompatibleNameRepairSummary {
  return Object.freeze({
    committedCount,
    logicalPathSample: Object.freeze(logicalPathSample.map(path => Object.freeze([...path]))),
    pairDisplayNames: Object.freeze({
      script: 'restore.windshare-abc234.ps1',
      sidecar: 'restore.windshare-abc234.data',
    }),
    placement: 'inside-logical-root',
    latestObservedFooter: Object.freeze({ committedCount, state: footerState }),
    sidecarSync: 'current',
    terminalSettlement: footerState === 'active' ? 'none' : 'complete',
  })
}

class TestActiveRepairProjection implements V2ActiveCompatibleNameRepairProjection {
  #listener: ((summary: CompatibleNameRepairSummary) => void) | undefined
  #current: CompatibleNameRepairSummary | undefined
  subscriptionCount = 0
  unsubscribed = false

  subscribe(listener: (summary: CompatibleNameRepairSummary) => void): () => void {
    this.subscriptionCount += 1
    this.#listener = listener
    if (this.#current !== undefined) listener(this.#current)
    return () => {
      if (this.#listener === listener) this.#listener = undefined
      this.unsubscribed = true
    }
  }

  publish(summary: CompatibleNameRepairSummary): void {
    this.#current = summary
    this.#listener?.(summary)
  }
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
