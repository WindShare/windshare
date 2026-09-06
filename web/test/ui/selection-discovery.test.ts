import { describe, expect, it, vi } from 'vitest'
import { encodeBase64Url } from '../../src/crypto/bytes'
import type { V2CatalogPage, V2CatalogEntry, V2ShareDescriptor } from '../../src/catalog/v2-records'
import type { V2CommittedDirectory } from '../../src/catalog/v2-page-store'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import { createSelectionSpec, selectionRulesSpecFromPolicy } from '../../src/transfer/intent'
import { discoverAuthenticatedSelection, SelectionProjectionController } from '../../src/transfer/projection'
import { V2JoinedProjectionSource, DRAFT_PREFETCH_DIRECTORY_LIMIT } from '../../src/ui/selection-discovery/source'
import { offerArtifacts, reconcileArtifactChoice, materializationRouteIdentity } from '../../src/output/planning'
import { environment, fsaTarget } from '../output/planning/fixture'

function identity(seed: number): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(16)
  bytes[0] = seed
  return bytes
}
const rootId = identity(1)
const shareInstance = identity(2)
const rootText = encodeBase64Url(rootId)
function folder(seed: number, name = `folder-${seed}`): V2CatalogEntry {
  const id = identity(seed)
  return { kind: 'directory', id, idText: encodeBase64Url(id), name }
}
function file(seed: number): V2CatalogEntry {
  const id = identity(seed)
  return { kind: 'file', id, idText: encodeBase64Url(id), name: `file-${seed}`, expectedSize: 64n }
}

function fixture(directories: readonly { id: Uint8Array<ArrayBuffer>; entries: readonly V2CatalogEntry[] }[]) {
  const generations = new Map(directories.map(({ id, entries }) => {
    const directoryIdText = encodeBase64Url(id)
    const generation = identity(90)
    const generationText = encodeBase64Url(generation)
    const terminalCommitment = new Uint8Array(32).fill(7)
    const committed: V2CommittedDirectory = {
      directoryId: id, directoryIdText, generation, generationText, pageCount: 1,
      entryCount: entries.length, omittedCount: 0n, terminalCommitment,
    }
    const page: V2CatalogPage = {
      shareInstance, directoryId: id, directoryIdText, generation, generationText,
      pageIndex: 0, terminal: true, previousCommitment: new Uint8Array(32), entries,
      omittedCount: 0n, objectCommitment: terminalCommitment, senderObjectBytes: 128,
    }
    return [directoryIdText, { committed, page }] as const
  }))
  const loadDirectory = vi.fn(async (id: Uint8Array<ArrayBuffer>) => {
    const value = generations.get(encodeBase64Url(id))
    if (value === undefined) throw new Error('Unexpected directory traversal')
    return value.committed
  })
  const catalog = {
    loadDirectory,
    async *pages(committed: V2CommittedDirectory) { yield generations.get(committed.directoryIdText)!.page },
  }
  const descriptor = {
    shareInstance, shareInstanceId: encodeBase64Url(shareInstance), syntheticRoot: rootId,
    syntheticRootId: rootText,
  } as V2ShareDescriptor
  return { catalog, descriptor, loadDirectory }
}

async function project(input: ReturnType<typeof fixture>, policy: V2SelectionPolicy, signal = new AbortController().signal) {
  const selection = policy.snapshot()
  const controller = new SelectionProjectionController()
  controller.beginSelection(await createSelectionSpec({
    shareInstance: input.descriptor.shareInstanceId, syntheticRoot: rootText,
    rules: selectionRulesSpecFromPolicy(selection),
  }))
  const source = new V2JoinedProjectionSource({
    descriptor: input.descriptor, catalog: input.catalog as never, selection,
    protocolSessionId: () => 'session', explicitRetry: false,
  })
  const states = []
  for await (const state of discoverAuthenticatedSelection(controller, source, signal)) states.push(state)
  return states
}

describe('bounded authenticated selection discovery', () => {
  it('keeps a progressive folder action and stable hierarchy without traversing descendants for totals', async () => {
    const shared = folder(3, 'Photos')
    const branches = [folder(4), folder(5), folder(6), folder(7)]
    const input = fixture([
      { id: rootId, entries: [shared] },
      { id: shared.id, entries: branches },
      ...branches.map(branch => ({ id: branch.id, entries: [file(20)] })),
    ])
    const states = await project(input, new V2SelectionPolicy(true))
    const final = states.at(-1)!
    expect(input.loadDirectory).toHaveBeenCalledTimes(1 + DRAFT_PREFETCH_DIRECTORY_LIMIT)
    expect(final.discovery.kind).toBe('bounded')
    expect(final.projection.workspaceCostObservation).toBeUndefined()
    expect(states.slice(1).every(state => state.projection.proof.kind === 'tree' &&
      state.projection.proof.layoutBasis.kind === 'complete-directory' &&
      state.projection.proof.layoutBasis.anchor.sourcePath === 'Photos')).toBe(true)
    const available = environment({ targets: [fsaTarget()] })
    const offers = await offerArtifacts(final.projection, final.discovery, available)
    expect(offers).toMatchObject({ kind: 'artifact-actions', primary: { suggestedName: 'Photos' } })
    if (offers.kind !== 'artifact-actions') throw new Error('Expected progressive folder offer')
    const resolved = await reconcileArtifactChoice({
      choice: offers.primary.choice,
      preferredRoute: materializationRouteIdentity(offers.primary.route),
      expectedSelectionDigest: final.projection.selectionDigest,
      projection: final.projection,
      discovery: final.discovery,
      environment: available,
      previousObservation: null,
    })
    expect(resolved).toMatchObject({ kind: 'resolved', action: { artifact: { kind: 'directory-tree' } } })
  })

  it('authenticates selected path hints without scanning unrelated directories', async () => {
    const selected = folder(3)
    const unrelated = folder(4)
    const target = file(5)
    const input = fixture([
      { id: rootId, entries: [unrelated, selected] },
      { id: selected.id, entries: [target] },
    ])
    const policy = new V2SelectionPolicy(false)
    policy.set(target, [rootText, selected.idText], true)
    const final = (await project(input, policy)).at(-1)!
    expect(input.loadDirectory).toHaveBeenCalledTimes(2)
    expect(final).toMatchObject({ discovery: { kind: 'complete' }, projection: { proof: { kind: 'single-file' } } })
  })

  it('rejects an unproven selection path instead of pruning on caller ancestry', async () => {
    const actual = folder(3)
    const forged = folder(4)
    const target = file(5)
    const input = fixture([{ id: rootId, entries: [actual] }])
    const policy = new V2SelectionPolicy(false)
    policy.set(target, [rootText, forged.idText], true)
    await expect(project(input, policy)).rejects.toThrow('Selection target path was not found')
  })

  it('settles a partial folder layout before speculative discovery, including selected descendants inside exclusions', async () => {
    const selected = folder(3, 'Project')
    const excluded = folder(4)
    const kept = file(5)
    const input = fixture([
      { id: rootId, entries: [selected] },
      { id: selected.id, entries: [excluded, folder(6), folder(7), folder(8)] },
      { id: excluded.id, entries: [kept] },
      { id: identity(6), entries: [] },
      { id: identity(7), entries: [] },
      { id: identity(8), entries: [] },
    ])
    const policy = new V2SelectionPolicy(false)
    policy.set(selected, [rootText], true)
    policy.set(excluded, [rootText, selected.idText], false)
    policy.set(kept, [rootText, selected.idText, excluded.idText], true)
    const final = (await project(input, policy)).at(-1)!
    expect(final).toMatchObject({
      discovery: { kind: 'bounded' },
      projection: { proof: { kind: 'tree', layoutBasis: { kind: 'directory-selection', anchor: { sourcePath: 'Project' } } } },
    })
  })

  it('keeps explicit empty selection empty without catalog I/O and honors cancellation', async () => {
    const input = fixture([])
    const policy = new V2SelectionPolicy(false)
    policy.set(file(3), [rootText], false)
    expect((await project(input, policy)).at(-1)).toMatchObject({
      discovery: { kind: 'complete' }, projection: { proof: { kind: 'none' } },
    })
    expect(input.loadDirectory).not.toHaveBeenCalled()
    const abort = new AbortController()
    abort.abort(new DOMException('Draft replaced', 'AbortError'))
    await expect(project(input, policy, abort.signal)).rejects.toMatchObject({ name: 'AbortError' })
  })
})
