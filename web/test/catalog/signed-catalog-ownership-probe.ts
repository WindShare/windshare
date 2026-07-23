import { V2CatalogClient, V2CatalogClientError } from '../../src/catalog/v2-client'
import type { V2CatalogPageStore } from '../../src/catalog/v2-page-store'
import type { V2CatalogPageRequest } from '../../src/catalog/v2-records'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { createSignedCatalogFixture, signedCatalogIdentity } from './signed-catalog-fixture'

export interface SignedCatalogOwnershipProbeResult {
  readonly crossPageNameRejected: boolean
  readonly crossPageNodeRejected: boolean
  readonly crossDirectoryNodeRejected: boolean
  readonly everyCollisionReportedAsProtocol: boolean
  readonly firstDirectoryCommitSurvived: boolean
}

export async function runSignedCatalogOwnershipProbe(
  store: V2CatalogPageStore,
): Promise<SignedCatalogOwnershipProbeResult> {
  const fixture = await createSignedCatalogFixture()
  const objects = new Map<string, Uint8Array<ArrayBuffer>>()
  const nameDirectory = signedCatalogIdentity(0x61)
  const nodeDirectory = signedCatalogIdentity(0x62)
  const firstOwnerDirectory = signedCatalogIdentity(0x63)
  const secondOwnerDirectory = signedCatalogIdentity(0x64)
  const firstNode = signedCatalogIdentity(0x51)
  const secondNode = signedCatalogIdentity(0x52)
  const replayedNode = signedCatalogIdentity(0x53)
  const crossDirectoryNode = signedCatalogIdentity(0x54)

  await addTwoPageCatalog(objects, fixture, {
    directoryId: nameDirectory,
    generation: signedCatalogIdentity(0x71),
    first: { id: firstNode, name: 'STRASSE' },
    second: { id: secondNode, name: 'Straße' },
  })
  await addTwoPageCatalog(objects, fixture, {
    directoryId: nodeDirectory,
    generation: signedCatalogIdentity(0x72),
    first: { id: replayedNode, name: 'alpha' },
    second: { id: replayedNode, name: 'beta' },
  })
  await addSinglePageCatalog(
    objects,
    fixture,
    firstOwnerDirectory,
    signedCatalogIdentity(0x73),
    crossDirectoryNode,
    'first-owner',
  )
  await addSinglePageCatalog(
    objects,
    fixture,
    secondOwnerDirectory,
    signedCatalogIdentity(0x74),
    crossDirectoryNode,
    'second-owner',
  )

  const protocolFailures: unknown[] = []
  const client = new V2CatalogClient({
    descriptor: fixture.descriptor,
    readSecret: fixture.readSecret,
    operations: {
      fetchPage: async (request) => {
        const object = objects.get(requestKey(request))
        if (object === undefined) throw new Error('Signed collision fixture received an unknown page request')
        return object
      },
      failProtocol: async (reason) => { protocolFailures.push(reason) },
    },
    store,
  })
  try {
    const crossPageNameRejected = await rejectsProtocol(client.loadDirectory(nameDirectory))
    const crossPageNodeRejected = await rejectsProtocol(client.loadDirectory(nodeDirectory))
    await client.loadDirectory(firstOwnerDirectory)
    const crossDirectoryNodeRejected = await rejectsProtocol(client.loadDirectory(secondOwnerDirectory))
    return Object.freeze({
      crossPageNameRejected,
      crossPageNodeRejected,
      crossDirectoryNodeRejected,
      everyCollisionReportedAsProtocol: protocolFailures.length === 3,
      firstDirectoryCommitSurvived: await store.loadDirectory(
        encodeBase64Url(firstOwnerDirectory),
      ) !== undefined,
    })
  } finally {
    client.close()
    fixture.close()
  }
}

async function addTwoPageCatalog(
  objects: Map<string, Uint8Array<ArrayBuffer>>,
  fixture: Awaited<ReturnType<typeof createSignedCatalogFixture>>,
  input: {
    readonly directoryId: Uint8Array<ArrayBuffer>
    readonly generation: Uint8Array<ArrayBuffer>
    readonly first: { readonly id: Uint8Array<ArrayBuffer>; readonly name: string }
    readonly second: { readonly id: Uint8Array<ArrayBuffer>; readonly name: string }
  },
): Promise<void> {
  const first = await fixture.sealPage({
    directoryId: input.directoryId,
    generation: input.generation,
    pageIndex: 0,
    terminal: false,
    previousCommitment: new Uint8Array(32),
    entries: [{ kind: 'file', ...input.first }],
  })
  const second = await fixture.sealPage({
    directoryId: input.directoryId,
    generation: input.generation,
    pageIndex: 1,
    terminal: true,
    previousCommitment: first.commitment,
    entries: [{ kind: 'file', ...input.second }],
  })
  objects.set(requestKey({ directoryId: input.directoryId, pageIndex: 0 }), first.object)
  objects.set(requestKey({ directoryId: input.directoryId, pageIndex: 1 }), second.object)
}

async function addSinglePageCatalog(
  objects: Map<string, Uint8Array<ArrayBuffer>>,
  fixture: Awaited<ReturnType<typeof createSignedCatalogFixture>>,
  directoryId: Uint8Array<ArrayBuffer>,
  generation: Uint8Array<ArrayBuffer>,
  nodeId: Uint8Array<ArrayBuffer>,
  name: string,
): Promise<void> {
  const sealed = await fixture.sealPage({
    directoryId,
    generation,
    pageIndex: 0,
    terminal: true,
    previousCommitment: new Uint8Array(32),
    entries: [{ kind: 'file', id: nodeId, name }],
  })
  objects.set(requestKey({ directoryId, pageIndex: 0 }), sealed.object)
}

function requestKey(request: Pick<V2CatalogPageRequest, 'directoryId' | 'pageIndex'>): string {
  return `${encodeBase64Url(request.directoryId)}:${request.pageIndex}`
}

async function rejectsProtocol(operation: Promise<unknown>): Promise<boolean> {
  try {
    await operation
    return false
  } catch (error) {
    return error instanceof V2CatalogClientError &&
      error.message === 'Authenticated catalog traffic violated its protocol'
  }
}
