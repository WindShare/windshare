import { afterEach, describe, expect, it, vi } from 'vitest'
import { encodeBase64Url } from '../../src/crypto/bytes'
import type { V2FrozenSelectionPolicy } from '../../src/catalog/v2-selection'
import type { V2CatalogEntry } from '../../src/catalog/v2-records'
import {
  createAuthenticatedProjectionEvidence,
  type AuthenticatedDiscoveryRequest,
  type AuthenticatedDiscoverySource,
} from '../../src/transfer/projection'
import type { V2ReceiverController, V2ReceiverTraceEvent } from '../../src/ui/v2-controller'
import type { V2BrowseDirectory, V2BrowsePage } from '../../src/ui/v2-gateway'
import {
  controllerFor, deferred, DIRECT_ZIP_ENVIRONMENT, FakeJoinedShare, FakeReceiveComposition,
  identityText, resetOrchestrationTestEnvironment, turns, waitFor,
} from './v2-receiver-orchestration-fixture'

const FOLDER = Object.freeze({
  kind: 'directory' as const, id: new Uint8Array(16).fill(88),
  idText: encodeBase64Url(new Uint8Array(16).fill(88)), name: 'Photos',
})

class InitialFolderShare extends FakeJoinedShare {
  readonly listing = deferred<void>()
  readonly pageCalls: string[] = []

  override async page(directory: V2BrowseDirectory): Promise<V2BrowsePage> {
    this.pageCalls.push(directory.idText)
    const page = await super.page(directory)
    if (directory.path.length === 0) return { ...page, entries: [FOLDER], entryCount: 1 }
    await this.listing.promise
    return page
  }

  childDirectory(parent: V2BrowseDirectory, entry: V2CatalogEntry): V2BrowseDirectory {
    return { id: entry.id, idText: entry.idText, name: entry.name,
      path: [...parent.path, entry.name], ancestry: [...parent.ancestry, entry.idText] }
  }

  override projectionSource(selection: V2FrozenSelectionPolicy): AuthenticatedDiscoverySource {
    const rootId = this.descriptor.syntheticRootId
    const selected = selection.selected(FOLDER, [rootId])
    const requests = this.projectionRequests
    return {
      async *discover(request: AuthenticatedDiscoveryRequest) {
        requests.push({ signal: request.signal })
        request.signal.throwIfAborted()
        // The authenticated root identifies the selected folder before its contents are listed.
        yield createAuthenticatedProjectionEvidence({
          generations: [{ directoryId: rootId, generation: identityText(20) }],
          metrics: { fileCountLowerBound: 0, directoryCountLowerBound: selected ? 1 : 0,
            byteCountLowerBound: 0n },
          ...(selected ? {
            selectedRoots: [{ kind: 'directory' as const, directoryId: FOLDER.idText,
              sourcePath: FOLDER.name, portableName: FOLDER.name }],
            selectedRootCount: 1,
            earlyLayoutBasis: { kind: 'complete-directory' as const,
              anchor: { directoryId: FOLDER.idText, sourcePath: FOLDER.name } },
          } : {}),
          settledTargets: request.unsettledTargets,
        })
        return { kind: selected ? 'bounded' as const : 'complete' as const }
      },
    }
  }
}

const controllers: V2ReceiverController[] = []
afterEach(async () => {
  await Promise.all(controllers.splice(0).map(controller => controller.dispose()))
  vi.restoreAllMocks()
  resetOrchestrationTestEnvironment()
})

function setup() {
  const joined = new InitialFolderShare(true)
  const receive = new FakeReceiveComposition(DIRECT_ZIP_ENVIRONMENT)
  const traces: V2ReceiverTraceEvent[] = []
  const controller = controllerFor(joined, receive, undefined, event => traces.push(event))
  controllers.push(controller)
  return { joined, receive, traces, controller }
}

async function clickFirstDownload(controller: V2ReceiverController): Promise<void> {
  await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'choices')
  const offers = controller.getSnapshot().output.offers
  if (offers?.kind !== 'artifact-actions') throw new Error('No download offered')
  expect(controller.getSnapshot().startAdmission.allowed).toBe(true)
  controller.chooseArtifact(offers.primary.choice.choiceId)
}

function invalidations(traces: readonly V2ReceiverTraceEvent[]) {
  return traces.filter(event => event.name === 'authority_transition' && event.transition === 'semantic_invalidated')
}

function expectFolderIntent(joined: InitialFolderShare): void {
  expect(joined.transferRuns).toHaveLength(1)
  expect(joined.transferRuns[0]?.intent.selection.rules).toEqual({
    mode: 'node-id', defaultSelected: false,
    rules: [{ kind: 'directory', id: FOLDER.idText, selected: true }],
  })
  expect(joined.transferRuns[0]?.signal?.aborted).toBe(false)
}

describe('initial folder download scope', () => {
  it.each(['preparing', 'committing', 'receiving'] as const)(
    'preserves the first click when the folder listing arrives during %s', async stage => {
      const { joined, receive, traces, controller } = setup()
      const authorityReady = deferred<void>()
      const commitReady = deferred<void>()
      receive.authorityReady = authorityReady.promise
      const startAuthority = receive.startArtifactAuthority.bind(receive)
      vi.spyOn(receive, 'startArtifactAuthority').mockImplementation((...args) => {
        const authority = startAuthority(...args)
        const commit = authority.commit.bind(authority)
        vi.spyOn(authority, 'commit').mockImplementation(async input => {
          await commitReady.promise
          return commit(input)
        })
        return authority
      })
      await clickFirstDownload(controller)
      expect(joined.pageCalls).toContain(FOLDER.idText)
      expect(controller.getSnapshot().browse.kind).toBe('loading')
      if (stage !== 'preparing') authorityReady.resolve()
      if (stage === 'committing') {
        await waitFor(() => traces.some(event =>
          event.name === 'authority_transition' && event.transition === 'commit_started'))
      }
      if (stage === 'receiving') {
        commitReady.resolve()
        await waitFor(() => joined.transferRuns.length === 1)
      }

      joined.listing.resolve()
      await waitFor(() => controller.getSnapshot().breadcrumbs.at(-1)?.id === FOLDER.idText)
      expect(invalidations(traces)).toEqual([])
      authorityReady.resolve()
      commitReady.resolve()
      await waitFor(() => joined.transferRuns.length === 1)
      expectFolderIntent(joined)
      expect(receive.startedAuthorities).toHaveLength(1)
      expect(controller.getSnapshot().taskDisplay?.objectLabel).toBe(FOLDER.name)
      expect(controller.getSnapshot().error).toBeNull()
    },
  )

  it('can start the folder download even when its display listing fails', async () => {
    const { joined, receive, traces, controller } = setup()
    const authorityReady = deferred<void>()
    receive.authorityReady = authorityReady.promise
    await clickFirstDownload(controller)
    joined.listing.reject(new Error('Directory listing unavailable'))
    await waitFor(() => controller.getSnapshot().browse.kind === 'failed')
    authorityReady.resolve()
    await waitFor(() => joined.transferRuns.length === 1)
    expectFolderIntent(joined)
    expect(invalidations(traces)).toEqual([])
  })

  it('still cancels preparation for an explicit selection change during initial navigation', async () => {
    const { joined, receive, traces, controller } = setup()
    const authorityReady = deferred<void>()
    receive.authorityReady = authorityReady.promise
    await clickFirstDownload(controller)
    controller.clearSelection()
    joined.listing.resolve()
    await waitFor(() => controller.getSnapshot().breadcrumbs.at(-1)?.id === FOLDER.idText)
    authorityReady.resolve()
    await turns()
    expect(invalidations(traces)).toMatchObject([{ invalidationReason: 'selection-changed' }])
    expect(controller.getSnapshot().draft.empty).toBe(true)
    expect(joined.transferRuns).toHaveLength(0)
  })

  it('restores the initial folder scope when leaving selection mode before the listing arrives', async () => {
    const { joined, controller } = setup()
    await waitFor(() => joined.pageCalls.includes(FOLDER.idText))
    controller.enterSelectionMode()
    controller.exitSelectionMode()
    await clickFirstDownload(controller)
    await waitFor(() => joined.transferRuns.length === 1)
    expectFolderIntent(joined)
    expect(controller.getSnapshot().breadcrumbs.at(-1)?.id).toBe(joined.descriptor.syntheticRootId)
    joined.listing.resolve()
    await waitFor(() => controller.getSnapshot().breadcrumbs.at(-1)?.id === FOLDER.idText)
    expectFolderIntent(joined)
  })

  it('treats an explicit return to the share root as a new download scope', async () => {
    const { joined, receive, traces, controller } = setup()
    joined.listing.resolve()
    await waitFor(() => controller.getSnapshot().breadcrumbs.at(-1)?.id === FOLDER.idText)
    const authorityReady = deferred<void>()
    receive.authorityReady = authorityReady.promise
    await clickFirstDownload(controller)
    controller.openBreadcrumb(0)
    await waitFor(() => controller.getSnapshot().breadcrumbs.length === 1)
    authorityReady.resolve()
    await turns()
    expect(controller.getSnapshot().draft.scope).toBe('whole-share')
    expect(invalidations(traces)).toMatchObject([{ invalidationReason: 'selection-changed' }])
    expect(joined.transferRuns).toHaveLength(0)
  })
})
