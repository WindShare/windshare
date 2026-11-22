import { ReceiveRuntimeState } from '../../src/application/receive/state'
import { ReceiveLifecycleNotifications, type ReceiveLifecycleListener } from '../../src/output/workspace/lifecycle/observation'
import { describe, expect, it } from 'vitest'

import { fileId } from '../../src/catalog/model'
import { SelectionPolicy } from '../../src/catalog/selection'
import type { ConnectivityActivation } from '../../src/connectivity/receiver-policy'
import {
  initialReceiveLifecycleState,
  nextReceiveLifecycleState,
  type ReceiveLifecycleState,
} from '../../src/output/workspace'
import { safeRenamedSummary } from '../../src/transfer/lifecycle/safe-names'
import { materializationRouteIdentity } from '../../src/output/planning/guarantees'
import { offerArtifacts } from '../../src/output/planning/offers'
import { type ArtifactChoice } from '../../src/output/model/artifacts/choices'
import {
  createOriginalFileArtifact,
  createReceiveIntent,
  createSelectionSpec,
  createWorkspaceBinding,
  createWorkspaceThenPublishPlan,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import { FaultScope, SourceFaultCode, sourceFault } from '../../src/transfer/failure/fault'
import { normalizedFileTransferFault } from '../../src/transfer/job/failures'
import type { SelectionMeasure } from '../../src/transfer/measure'
import type { PlanExecutionAuthority } from '../../src/transfer/output/session'
import {
  TransferFailureAccumulator,
  transferWorkerSettlement,
  type TransferFileOutcome,
  type TransferWorkerSettlement,
} from '../../src/transfer/outcome'
import type { TransferJobResult } from '../../src/transfer/job'
import { ActiveReceiveCoordinator } from '../../src/application/receive/active/coordinator'
import { type ActiveReceiveJoinedShare } from '../../src/application/receive/joined-share'
import type { LifecycleUserAction, ActiveReceiveControl } from '../../src/application/receive/actions'
import { OutputPresentationController } from '../../src/ui/output'
import type {
  BoundReceiveOperation,
  LifecycleMutation,
} from '../../src/application/receive/contracts'
import {
  COMPLETE_DISCOVERY,
  environment,
  identity,
  managedTarget,
  projection,
  singleFileProof,
  workspaceOffer,
} from '../output/planning/fixture'

const FILE_ID = identity(2)
const TRANSFER_MEASURE: SelectionMeasure = Object.freeze({
  discoveredFiles: 1,
  discoveredBytes: 128n,
  discovery: 'complete',
  sourceOmittedEntries: 0n, sizeClass: 'small',
})
const UNAVAILABLE_PLANS: PlanExecutionAuthority = Object.freeze({
  openDirectTree: unavailableExecution,
  openDirectResumableZip: unavailableExecution,
  openWorkspaceOriginal: unavailableExecution,
  openWorkspaceZip: unavailableExecution,
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

  it('publishes the exact retained result while preserving the independent draft projection', async () => {
    const fixture = await resultAdoptionFixture('retained')
    const draftProjection = fixture.outputs.getSnapshot().projection
    expect(draftProjection).not.toBeNull()

    fixture.settle('destination-collision')
    await waitFor(() => fixture.outputs.getSnapshot().transferResultPresentation !== null)

    expect(fixture.outputs.getSnapshot().projection).toBe(draftProjection)
    expect(fixture.outputs.getSnapshot().transferResultPresentation).toMatchObject({
      title: 'Existing destinations prevented completion',
      tone: 'warning',
      lines: [expect.stringMatching(/existing destination prevented/u)],
    })
    expect(fixture.failures).toEqual([])
    await fixture.close()
  })

  it('adopts a safe-renamed summary alongside a normal published completion', async () => {
    const fixture = await resultAdoptionFixture('live')
    const safeRenamed = safeRenamedSummary({
      count: 2,
      sample: [
        { logicalPath: ['folder', 'pyvenv.cfg'], physicalName: 'pyvenv-cfg.windshare-a1b2c3d4' },
        { logicalPath: ['folder', 'CON'], physicalName: 'entry-con.windshare-deadbeef' },
      ],
    })

    fixture.settleResult(Object.freeze({
      worker: transferWorkerSettlement('Succeeded', new TransferFailureAccumulator().snapshot()),
      lifecycle: publishedLifecycle(fixture.initialLifecycle),
      measure: TRANSFER_MEASURE,
      transferJobId: fixture.runtime.transferJobId,
      intent: fixture.intent,
      safeRenamed,
    }))
    await waitFor(() => fixture.outputs.getSnapshot().transferResultPresentation !== null)

    expect(fixture.outputs.getSnapshot()).toMatchObject({
      safeRenamed,
      lifecyclePresentation: { title: 'Saved', tone: 'positive' },
      transferResultPresentation: { title: 'Transfer completed', tone: 'success' },
    })
    await fixture.close()
  })
})

async function resultAdoptionFixture(
  mode: 'live' | 'retained',
): Promise<Readonly<{
  outputs: OutputPresentationController
  receiveState: ReceiveRuntimeState
  failures: unknown[]
  settle: (outcome: TransferFileOutcome) => void
  settleResult: (result: TransferJobResult) => void
  intent: ReceiveIntent
  initialLifecycle: ReceiveLifecycleState
  runtime: ResultRuntime
  close: () => Promise<void>
}>> {
  const { outputs, receiveState, intent, choice } = await preparedOutput()
  const lifecycle = initialReceiveLifecycleState({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
  })
  if (mode === 'live') {
    expect(receiveState.adoptReceiveIntent(choice, intent, lifecycle)).toBe(true)
  } else {
    receiveState.adoptRetainedReceiveIntent(intent, lifecycle)
  }

  const runtime = new ResultRuntime(intent, lifecycle)
  const share = new ResultShare(intent)
  const failures: unknown[] = []
  const coordinator = new ActiveReceiveCoordinator({
    state: receiveState,
    ownsJoinedShare: candidate => candidate === share,
    observeProgress: () => undefined,
    onActionError: error => failures.push(error),
    onFailure: error => failures.push(error),
  })
  coordinator.adopt({
    joined: share,
    selection: new SelectionPolicy(true).snapshot(),
    runtime,
  })

  return Object.freeze({
    outputs,
    receiveState,
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
  outputs: OutputPresentationController
  receiveState: ReceiveRuntimeState
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
    environment({ targets: [managedTarget()], workspace: workspaceOffer() }),
  )
  if (offers.kind !== 'artifact-actions' || offers.primary.route.kind !== 'workspace-then-publish') {
    throw new Error('result-adoption fixture did not select a workspace artifact')
  }
  const offered = offers.primary
  if (offered.route.kind !== 'workspace-then-publish') {
    throw new Error('result-adoption fixture lost its workspace route')
  }
  const artifact = await createOriginalFileArtifact({
    fileId: FILE_ID,
    sourcePath: 'report.txt',
    suggestedName: offered.suggestedName ?? 'report.txt',
  })
  const workspace = await createWorkspaceBinding({
    operationId: identity(10),
    workspaceId: identity(11),
    artifact,
    repositoryRef: identity(12, 32),
  })
  const plan = await createWorkspaceThenPublishPlan(artifact, workspace, 'managed-atomic')
  const intent = await createReceiveIntent({
    selection,
    artifact,
    plan,
  })
  const receiveState = new ReceiveRuntimeState()
  const outputs = new OutputPresentationController(receiveState)
  receiveState.updateProjection(1, state, offers)
  receiveState.updateActivation(Object.freeze({
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
    receiveState,
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

  beginDownloadConnectivity(): ConnectivityActivation {
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

class ResultRuntime implements BoundReceiveOperation {
  readonly lifecycleNotifications = new ReceiveLifecycleNotifications()
  subscribeLifecycle(listener: ReceiveLifecycleListener): () => void {
    return this.lifecycleNotifications.subscribe(listener)
  }
  readonly plans = UNAVAILABLE_PLANS
  readonly transferJobId = identity(20)
  readonly activeControls = Object.freeze([] as const)
  readonly initialWorkspaceUsage = null
  readonly intent: ReceiveIntent
  readonly lifecycle: ReceiveLifecycleState

  constructor(
    intent: ReceiveIntent,
    lifecycle: ReceiveLifecycleState,
  ) {
    this.intent = intent
    this.lifecycle = lifecycle
  }

  interrupt(
    control: ActiveReceiveControl,
    transfer: AbortController,
  ): never {
    return unexpectedFixtureCall('result-adoption fixture cannot be interrupted', {
      control,
      transfer,
    })
  }

  startLifecycleAction(
    action: Exclude<LifecycleUserAction, ActiveReceiveControl>,
    lifecycle: ReceiveLifecycleState,
  ): LifecycleMutation {
    return unexpectedFixtureCall('result-adoption fixture has no lifecycle action', {
      action,
      lifecycle,
    })
  }

  resolveWorkspaceUsage(lifecycle: ReceiveLifecycleState): null {
    if (lifecycle.operationId !== this.lifecycle.operationId) {
      return unexpectedFixtureCall('coordinator changed the adopted operation', lifecycle)
    }
    return null
  }

  settleTransferAdmissionFailure(reason: unknown): LifecycleMutation {
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
    classification: normalizedFileTransferFault(
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
    retainedBytes: 0n,
    completedBytes: 0n,
    selectionFacts: Object.freeze({
      discoveredFileCount: 0n,
      discoveredBytes: 0n,
      discovery: 'complete',
    }),
  })
}

function publishedLifecycle(initial: ReceiveLifecycleState): ReceiveLifecycleState {
  return nextReceiveLifecycleState(initial, {
    kind: 'published',
    receiptDigest: identity(31, 32),
    cleanupState: 'clean',
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
