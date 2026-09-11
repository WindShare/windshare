import { IndexedDbStagingBudgetStore } from '../../../src/output/staging-budget/indexeddb-store'
import { StagingBudgetCoordinator, type StagingFileReservation } from '../../../src/output/staging-budget/coordinator'
import { withStagingExportAuthority } from '../../../src/output/staging-budget/export-authority'

interface Session {
  readonly store: IndexedDbStagingBudgetStore
  readonly coordinator: StagingBudgetCoordinator
  reservation?: StagingFileReservation
  exportWork?: Promise<void>
  finishExport?: () => void
}
const sessions = new Map<string, Session>()

export async function openStagingCapacity(databaseName: string, actor: string, quota = 1000n) {
  const store = await IndexedDbStagingBudgetStore.open(databaseName)
  sessions.set(actor, { store, coordinator: new StagingBudgetCoordinator({ store,
    storage: async () => ({ opfs: 'usable', persistence: 'not-persisted',
      quota: { kind: 'estimated', usageBytes: 0n, quotaBytes: quota }, pressure: 'normal' }),
    policy: { maximumTaskFiles: 2, maximumSiteFiles: 4, maximumTaskPhysicalBytes: 10_000n,
      maximumSitePhysicalBytes: 20_000n, metadataHeadroomBytes: 10n, finalizationHeadroomBytes: 20n,
      minimumQuotaReserveBytes: 100n },
  }) })
}

export async function stagingCapacityAction(actor: string,
  kind: 'reserve' | 'cancel' | 'queue' | 'export' | 'failed' | 'saved' | 'deleted' | 'restore' | 'receive',
  exactSize = 500n, operationId = actor) {
  const session = sessions.get(actor)!
  try {
    if (kind === 'reserve') {
      const decision = await session.coordinator.tryReserve({ operationId, fileId: 'file', exactSize })
      if (decision.kind === 'admitted') session.reservation = decision.reservation
      return decision.kind === 'admitted' ? 'admitted' : decision.reason
    }
    if (kind === 'restore') {
      session.reservation = await session.coordinator.restore({ operationId, fileId: 'file', exactSize,
        verifiedStagedBytes: exactSize, phase: 'queued', operationLease: { operationId, leaseId: actor } })
      return 'restored'
    }
    const reservation = session.reservation!
    switch (kind) {
      case 'cancel': await reservation.cancelUnused(); break
      case 'receive': await reservation.received(exactSize); break
      case 'queue': await reservation.received(exactSize); await reservation.queueExport(); break
      case 'export': return await startExport(session, reservation)
      case 'failed': await reservation.exportFailed(); await finishExport(session); break
      case 'saved': await reservation.targetSaved(); await finishExport(session); break
      case 'deleted': await reservation.releaseDeleted(); break
    }
    return 'ok'
  } catch (error) { return error instanceof Error ? error.name : String(error) }
}

export async function stagingCapacitySnapshot(actor: string) {
  return sessions.get(actor)!.coordinator.snapshot()
}

function startExport(session: Session, reservation: StagingFileReservation): Promise<string> {
  return new Promise(resolve => {
    session.exportWork = withStagingExportAuthority(async authority => {
      const started = await reservation.beginExport(authority)
      if (!started) { resolve('waiting'); return }
      const completion = new Promise<void>(finish => { session.finishExport = finish })
      resolve('started')
      await completion
    }).catch(error => { resolve(error instanceof Error ? error.name : String(error)) })
  })
}

async function finishExport(session: Session): Promise<void> {
  session.finishExport?.()
  await session.exportWork
  delete session.finishExport
  delete session.exportWork
}

export async function closeStagingCapacity() {
  for (const session of sessions.values()) {
    await finishExport(session)
    session.store.close()
  }
  sessions.clear()
}
