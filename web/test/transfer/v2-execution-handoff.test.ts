import { describe, expect, it } from 'vitest'
import { SelectionPolicy } from '../../src/catalog/selection'
import { TransferPauseRequestedError } from '../../src/transfer/output/session'
import {
  createPlanExecutionAuthority,
  type PlanExecutionRouteRegistry,
} from '../../src/transfer/settlement/plan-authority'
import { deferred } from './settlement-deadline'
import {
  catalogFixture, fileEntry, identity, planAuthorityFixture, readerFixture,
  receiveIntentFixture, selectOnlyFile, transferJobFixture,
} from './v2-job-fixture'

const ROUTES = [
  { planKind: 'direct-tree', artifactKind: 'directory-tree', paused: 'partial-directory' },
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
    const f = await fixture(ROUTES[2])
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
    ? selectOnlyFile(file) : new SelectionPolicy(true)
  const intent = await receiveIntentFixture({ ...route, selection, file })
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
  const routes: PlanExecutionRouteRegistry = {
    directTree: { open: (...args) => handoff(backend.openDirectTree(...args)) },
    workspaceOriginal: { admit: (...args) => handoff(backend.openWorkspaceOriginal(...args)) },
    workspaceZip: { admit: (...args) => handoff(backend.openWorkspaceZip(...args)) },
    portableOriginal: { prepare: (...args) => handoff(backend.preparePortable(...args)) },
    portableZip: { prepare: (...args) => handoff(backend.preparePortable(...args)) },
    lifecycle: backend,
  }
  const plans = await createPlanExecutionAuthority({ intent, routes })
  const controller = new AbortController()
  const job = transferJobFixture({
    catalog: catalog.catalog, selection, intent, plans,
    revisions: readers.revisions, broker: readers.broker,
  })
  return { backend, catalog, readers, controller, job, entered, release }
}
