import { describe, expect, it } from 'vitest'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import {
  createDirectResumableZipPlan, createFSAOwnedFileBinding, createReceiveIntent,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import {
  TransferPauseRequestedError, type DirectResumableZipExecution,
} from '../../src/transfer/output-session'
import {
  createV2PlanExecutionAuthority,
  type V2PlanExecutionRouteRegistry,
} from '../../src/transfer/settlement/v2-plan-authority'
import { deferred } from './settlement-deadline'
import {
  catalogFixture, digestIdentity, fileEntry, identity, identityText, planAuthorityFixture, readerFixture,
  receiveIntentFixture, selectOnlyFile, transferJobFixture,
} from './v2-job-fixture'

const ROUTES = [
  { planKind: 'direct-tree', artifactKind: 'directory-tree', paused: 'partial-directory' },
  { planKind: 'direct-atomic', artifactKind: 'original-file', paused: 'restart-required' },
  { planKind: 'direct-resumable-zip', artifactKind: 'zip-archive', paused: 'resumable-receive' },
  { planKind: 'workspace-then-publish', artifactKind: 'original-file', paused: 'resumable-receive' },
  { planKind: 'workspace-then-publish', artifactKind: 'zip-archive', paused: 'resumable-receive' },
  { planKind: 'portable-handoff', artifactKind: 'original-file', paused: 'restart-required' },
  { planKind: 'portable-handoff', artifactKind: 'zip-archive', paused: 'restart-required' },
] as const

describe('production execution handoff cancellation', () => {
  it.each(ROUTES)(
    'hands accepted $planKind / $artifactKind to the job before observing Pause',
    async route => {
      const f = await fixture(route)
      const running = f.job.run(f.controller.signal)
      await f.entered.promise
      const loadsBeforePause = [...f.catalog.loads]
      const pause = new TransferPauseRequestedError()
      f.controller.abort(pause)
      f.release.resolve()
      const result = await running

      expect(result.worker.status).toBe('Paused')
      expect(result.lifecycle.kind).toBe(route.paused)
      expect(result.abortReason).toBe(pause)
      expect(f.backend.pauses).toEqual([route.planKind])
      expect(f.backend.pauseSignals).toHaveLength(1)
      expect(f.backend.pauseSignals[0]?.reason).not.toBe(pause)
      expect(f.backend.admissionFailures).toEqual([])
      expect(f.backend.unknownSettlements).toEqual([])
      expect(f.backend.settlements).toEqual([])
      expect(f.backend.output.requests).toEqual([])
      expect(f.catalog.loads).toEqual(loadsBeforePause)
      expect(f.readers.revisionRequests).toEqual([])
      expect(f.readers.blockRequests).toEqual([])
    },
  )

  it.each(ROUTES.filter(route =>
    route.planKind === 'workspace-then-publish' || route.planKind === 'portable-handoff'))(
    'preserves committed $planKind / $artifactKind rejection when Pause races its return',
    async route => {
      const f = await fixture(route, true)
      const running = f.job.run(f.controller.signal)
      await f.entered.promise
      f.controller.abort(new TransferPauseRequestedError())
      f.release.resolve()
      const result = await running

      expect(result.lifecycle.kind).toBe('discarded')
      expect(f.backend.pauses).toEqual([])
      expect(f.backend.admissionFailures).toEqual([])
      expect(f.backend.unknownSettlements).toEqual([])
      expect(f.backend.output.requests).toEqual([])
      expect(f.readers.revisionRequests).toEqual([])
      expect(f.readers.blockRequests).toEqual([])
    },
  )

  it('does not acquire output when Pause already precedes admission', async () => {
    const f = await fixture(ROUTES[4])
    f.controller.abort(new TransferPauseRequestedError())
    const result = await f.job.run(f.controller.signal)

    expect(result.worker.status).toBe('Paused')
    expect(f.backend.routes).toEqual([])
    expect(f.backend.admissionFailures).toHaveLength(1)
    expect(f.backend.pauses).toEqual([])
    expect(f.backend.output.requests).toEqual([])
    expect(f.catalog.loads).toEqual([])
    expect(f.readers.revisionRequests).toEqual([])
    expect(f.readers.blockRequests).toEqual([])
  })
})

async function fixture(route: typeof ROUTES[number], rejectPreparation = false) {
  const file = fileEntry(identity(11), 'payload.bin', 4n)
  const selection = route.artifactKind === 'original-file'
    ? selectOnlyFile(file) : new V2SelectionPolicy(true)
  const seed = await receiveIntentFixture({
    ...route, planKind: route.planKind === 'direct-resumable-zip' ? 'workspace-then-publish' : route.planKind,
    selection, file,
  })
  const intent = route.planKind === 'direct-resumable-zip' ? await directZipIntent(seed) : seed
  const catalog = catalogFixture([{ id: identity(2), entries: [file] }])
  const readers = readerFixture([file])
  const backend = planAuthorityFixture({ rejectPreparation })
  const entered = deferred()
  const release = deferred()
  const handoff = async <T>(result: Promise<T>): Promise<T> => {
    const accepted = await result
    entered.resolve()
    await release.promise
    return accepted
  }
  const routes: V2PlanExecutionRouteRegistry = {
    directTree: { open: (...args) => handoff(backend.openDirectTree(...args)) },
    directAtomic: { open: (...args) => handoff(backend.openDirectAtomic(...args)) },
    directResumableZip: { open: async intent => {
      backend.routes.push('direct-resumable-zip')
      const execution: DirectResumableZipExecution = {
        planKind: 'direct-resumable-zip',
        output: {
          identity: backend.output.identity,
          capabilities: backend.output.capabilities,
          beginFile: unexpectedContent,
        },
        ordered: {
          beginTraversal: unexpectedContent, visit: unexpectedContent, finishTraversal: unexpectedContent,
          materializationSummary: () => ({ entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n }),
        },
        pause: async (_request, signal) => {
          backend.pauses.push('direct-resumable-zip')
          backend.pauseSignals.push(signal)
          return {
            kind: 'resumable-receive', payloadKind: 'direct-zip',
            operationId: intent.operationId, receiveIntentDigest: intent.digest, generation: 2n,
            directZipCheckpointDigest: digestIdentity(50), safeSelectedPayloadBytes: 0n,
            committedArchiveLength: 0n, checkpointPhase: 'between-members',
          }
        },
        settle: unexpectedContent,
      }
      return handoff(Promise.resolve(execution))
    } },
    workspaceOriginal: { admit: (...args) => handoff(backend.openWorkspaceOriginal(...args)) },
    workspaceZip: { admit: (...args) => handoff(backend.openWorkspaceZip(...args)) },
    portableOriginal: { prepare: (...args) => handoff(backend.preparePortable(...args)) },
    portableZip: { prepare: (...args) => handoff(backend.preparePortable(...args)) },
    lifecycle: backend,
  }
  const plans = await createV2PlanExecutionAuthority({ intent, routes })
  const controller = new AbortController()
  const job = transferJobFixture({
    catalog: catalog.catalog, selection, intent, plans,
    revisions: readers.revisions, broker: readers.broker,
  })
  return { backend, catalog, readers, controller, job, entered, release }
}

async function unexpectedContent(): Promise<never> {
  throw new Error('Canceled acquisition must not start content or publication')
}

async function directZipIntent(seed: ReceiveIntent): Promise<ReceiveIntent> {
  const artifact = seed.artifact
  if (artifact.kind !== 'zip-archive') throw new Error('Direct ZIP requires a ZIP artifact')
  const binding = await createFSAOwnedFileBinding({
    operationId: seed.operationId, artifact,
    stableName: `windshare.windshare-${identityText(12)}.zip`, targetRef: digestIdentity(41),
    policies: {
      zipEncoding: digestIdentity(42), layout: digestIdentity(43), checkpoint: digestIdentity(44),
      journalBudget: digestIdentity(45), epoch: digestIdentity(46),
    },
  })
  return createReceiveIntent({
    selection: seed.selection, artifact, plan: await createDirectResumableZipPlan(artifact, binding),
  })
}
