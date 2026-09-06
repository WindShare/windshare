import { afterEach, describe, expect, it } from 'vitest'
import type { V2CatalogEntry } from '../../src/catalog/v2-records'
import type { V2ConnectivityActivation } from '../../src/connectivity/v2-receiver-policy'
import { encodeBase64Url } from '../../src/crypto/bytes'
import type { ReceiveIntent } from '../../src/transfer/intent'
import type { V2PlanExecutionAuthority } from '../../src/transfer/output-session'
import type { TransferProgress } from '../../src/transfer/v2-job'
import type { V2BrowseDirectory, V2BrowsePage } from '../../src/ui/v2-gateway'
import { EMPTY_V2_PROGRESS } from '../../src/ui/v2-model'
import {
  FakeJoinedShare, FakeReceiveComposition, FILE_ID, MANAGED_ENVIRONMENT,
  controllerFor, deferred, identityText, next, resetOrchestrationTestEnvironment,
  startTransfer, turns, waitFor,
} from './v2-receiver-orchestration-fixture'

const folder = Object.freeze({
  kind: 'directory' as const, id: new Uint8Array(16).fill(88),
  idText: encodeBase64Url(new Uint8Array(16).fill(88)), name: 'Other folder',
})

class BrowsableTransferShare extends FakeJoinedShare {
  onProgress: ((progress: TransferProgress) => void) | undefined
  browseFailure = false
  soleFolder = false
  pageCalls: string[] = []

  override async page(directory: V2BrowseDirectory): Promise<V2BrowsePage> {
    this.pageCalls.push(directory.idText)
    const original = await super.page(directory)
    if (directory.path.length === 0) {
      if (this.soleFolder) return { ...original, entries: [folder], entryCount: 1 }
      return { ...original, entries: [...original.entries, folder], entryCount: 2 }
    }
    if (this.browseFailure) throw new Error('Folder temporarily unavailable')
    return original
  }

  childDirectory(parent: V2BrowseDirectory, entry: V2CatalogEntry): V2BrowseDirectory {
    return { id: entry.id, idText: entry.idText, name: entry.name,
      path: [...parent.path, entry.name], ancestry: [...parent.ancestry, entry.idText] }
  }

  override transferJob(
    plans: V2PlanExecutionAuthority, intent: ReceiveIntent,
    _connectivity?: V2ConnectivityActivation,
    options?: { onProgress?: (progress: TransferProgress) => void },
  ) {
    this.onProgress = options?.onProgress
    return super.transferJob(plans, intent)
  }
}

afterEach(resetOrchestrationTestEnvironment)

describe('draft, browsing and committed output ownership', () => {
  it('opens an authenticated sole folder directly without losing its share identity or hierarchy', async () => {
    const joined = new BrowsableTransferShare(true)
    joined.soleFolder = true
    const controller = controllerFor(joined, new FakeReceiveComposition(MANAGED_ENVIRONMENT))
    await waitFor(() => controller.getSnapshot().breadcrumbs.at(-1)?.name === folder.name)
    expect(joined.pageCalls).toHaveLength(2)
    expect(controller.getSnapshot().share).toMatchObject({
      kind: 'browser', name: folder.name, singleFolder: true, homeDirectoryId: folder.idText,
    })
    expect(controller.getSnapshot().draft).toMatchObject({ scope: 'current-folder', label: folder.name })
    expect(joined.selection.snapshot().canonicalRules).toMatchObject([{ kind: 'directory', selected: true }])
    await controller.dispose()
  })

  it('preserves committed intent, label and bytes through selection, navigation, failure and reconnect', async () => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const joined = new BrowsableTransferShare(true)
    const controller = controllerFor(joined, receive)
    await waitFor(() => controller.getSnapshot().phase === 'browsing')
    controller.toggleSelection(FILE_ID)
    await startTransfer(controller, joined)
    const committed = controller.getSnapshot().output
    const label = controller.getSnapshot().taskDisplay
    expect(label?.objectLabel).toBe('1 file')
    joined.onProgress?.({ ...EMPTY_V2_PROGRESS, writtenBytes: 64n, discoveredBytes: 128n,
      discoveredFiles: 1, transferJobId: identityText(76) } as TransferProgress)
    const progress = controller.getSnapshot().progress

    controller.clearSelection()
    expect(controller.getSnapshot().draft.empty).toBe(true)
    expect(controller.getSnapshot().startAdmission.allowed).toBe(false)
    expect(controller.getSnapshot().output.receiveIntent).toBe(committed.receiveIntent)
    controller.openDirectory(folder.idText)
    await waitFor(() => controller.getSnapshot().breadcrumbs.at(-1)?.name === folder.name)
    controller.exitSelectionMode()
    expect(controller.getSnapshot().draft.label).toBe(folder.name)
    controller.openBreadcrumb(0)
    await waitFor(() => controller.getSnapshot().breadcrumbs.length === 1)
    joined.browseFailure = true
    controller.openDirectory(folder.idText)
    await waitFor(() => controller.getSnapshot().browse.kind === 'failed')
    expect(controller.getSnapshot().error).toBeNull()
    expect(controller.getSnapshot().phase).toBe('browsing')
    joined.replaceProtocolSession(identityText(91))
    await turns()

    expect(controller.getSnapshot().output.receiveIntent).toBe(committed.receiveIntent)
    expect(controller.getSnapshot().output.plan).toBe(committed.plan)
    expect(controller.getSnapshot().output.chosenChoice).toBe(committed.chosenChoice)
    expect(controller.getSnapshot().taskDisplay).toBe(label)
    expect(controller.getSnapshot().progress).toBe(progress)
    expect(joined.transferRuns).toHaveLength(1)
    expect(joined.transferRuns[0]?.signal?.aborted).toBe(false)
    await controller.dispose()
  })

  it('keeps picker cancellation as a draft and releases a finished task only on explicit next-task intent', async () => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const readiness = deferred<void>()
    receive.authorityReady = readiness.promise
    const joined = new FakeJoinedShare(true)
    const controller = controllerFor(joined, receive)
    await waitFor(() => controller.getSnapshot().output.offers?.kind === 'artifact-actions')
    const offers = controller.getSnapshot().output.offers
    if (offers?.kind !== 'artifact-actions') throw new Error('offers missing')
    controller.chooseArtifact(offers.primary.choice.choiceId)
    controller.cancelPreparing()
    readiness.resolve()
    await turns()
    expect(controller.getSnapshot().output.receiveIntent).toBeNull()
    expect(controller.getSnapshot().taskDisplay).toBeNull()
    expect(controller.getSnapshot().draft.label).toBe('report.txt')

    receive.authorityReady = undefined
    await startTransfer(controller, joined)
    const runtime = receive.startedAuthorities.at(-1)?.runtime
    if (runtime === undefined) throw new Error('runtime missing')
    joined.transferRuns[0]?.resolve(next(runtime.lifecycle, {
      kind: 'download-started', attemptKind: 'portable', attemptId: identityText(92),
    }))
    await waitFor(() => controller.getSnapshot().startAdmission.canReleaseCurrent)
    expect(controller.getSnapshot().output.receiveIntent).toEqual(runtime.intent)
    controller.startNewReceiveOperation()
    await waitFor(() => controller.getSnapshot().startAdmission.allowed)
    expect(controller.getSnapshot().output.receiveIntent).toBeNull()
    expect(runtime.detachments).toEqual(['detached'])
    expect(controller.getSnapshot().draft.label).toBe('report.txt')
    await controller.dispose()
  })
})
