import {
  bindReceiveIntent, materializationRouteIdentity, offerArtifacts, reconcileArtifactChoice,
} from '../../../src/output/planning'
import type { OutputTraceEvent } from '../../../src/output/diagnostics/trace'
import { createSelectionSpec } from '../../../src/transfer/intent'
import { WorkspaceArtifactPresentationAuthority } from '../../../src/ui/browser-receive/workspace-route'
import type { V2BoundReceiveOperation } from '../../../src/ui/v2-receive-runtime'
import {
  COMPLETE_DISCOVERY, environment, handoffTarget, identity, projection,
  singleFileProof, treeProof, workspaceOffer,
} from '../../output/planning/fixture'

type ArtifactKind = 'original-file' | 'zip-archive'

async function activate(kind: ArtifactKind, events: OutputTraceEvent[]): Promise<V2BoundReceiveOperation> {
  const selection = await createSelectionSpec({
    shareInstance: identity(1), syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const currentEnvironment = environment({ targets: [handoffTarget()], workspace: workspaceOffer() })
  const currentProjection = projection(selection, kind === 'original-file' ? singleFileProof() : treeProof(), 1n)
  const offers = await offerArtifacts(currentProjection, COMPLETE_DISCOVERY, currentEnvironment)
  if (offers.kind !== 'artifact-actions') throw new Error('Workspace action unavailable')
  const offered = [offers.primary, ...offers.alternatives].find(candidate =>
    candidate.route.kind === 'workspace-then-publish' && candidate.choice.artifactKind === kind)
  if (offered === undefined) throw new Error('Workspace artifact unavailable')
  const resolved = await reconcileArtifactChoice({
    choice: offered.choice, preferredRoute: materializationRouteIdentity(offered.route),
    expectedSelectionDigest: selection.digest, projection: currentProjection,
    discovery: COMPLETE_DISCOVERY, environment: currentEnvironment,
    previousObservation: {
      projectionEpoch: currentProjection.epoch, selectionDigest: selection.digest, resolvedArtifactDigest: null,
    },
  })
  if (resolved.kind !== 'resolved') throw new Error('Workspace action unresolved')
  const authority = new WorkspaceArtifactPresentationAuthority({
    windowPort: window, offered, preClickRanking: [offered.choice.choiceId],
    diagnostics: { backend: 'origin_private', trace: { current: event => events.push(event) } },
  })
  const result = await authority.commit({
    action: resolved.action, signal: new AbortController().signal,
    freezeAtFence: candidate => bindReceiveIntent({ selection, action: resolved.action, candidate }),
  })
  if (result.kind !== 'bound-operation') throw new Error('Workspace activation did not bind')
  return result.operation
}

async function exercise(events: OutputTraceEvent[], settlePermission: () => Promise<void>) {
  const operations: V2BoundReceiveOperation[] = []
  try {
    operations.push(await activate('original-file', events))
    operations.push(await activate('zip-archive', events))
    const pauses: string[] = []
    for (const operation of operations) {
      const transfer = new AbortController()
      operation.interrupt('pause', transfer)
      const paused = await operation.settleTransferAdmissionFailure(transfer.signal.reason)
      pauses.push(paused.lifecycle.kind)
      await operation.startLifecycleAction('discard', paused.lifecycle)
    }
    const beforePermission = operations.map(operation => operation.lifecycle.kind)
    await settlePermission()
    const afterPermission = operations.map(operation => operation.lifecycle.kind)
    const locks = await navigator.locks.query()
    return {
      pauses, beforePermission, afterPermission,
      activationLocks: locks.held?.filter(lock => lock.name?.startsWith('windshare/workspace-activation/')),
      transitions: events.filter(event => event.eventName === 'storage_persistence').map(event => event.payload.transition),
    }
  } finally {
    await Promise.all(operations.map(operation => operation.detach()))
  }
}

export async function withPendingPermission(granted: boolean) {
  const original = Object.getOwnPropertyDescriptor(navigator.storage, 'persist')
  let resolve!: (value: boolean) => void
  const pending = new Promise<boolean>(accept => { resolve = accept })
  let requests = 0
  Object.defineProperty(navigator.storage, 'persist', {
    configurable: true, value: () => { requests++; return pending },
  })
  try {
    const result = await exercise([], async () => { resolve(granted); await pending })
    return { ...result, requests }
  } finally {
    resolve(granted)
    if (original === undefined) Reflect.deleteProperty(navigator.storage, 'persist')
    else Object.defineProperty(navigator.storage, 'persist', original)
  }
}

export async function withNativePermission() {
  return exercise([], async () => {})
}
