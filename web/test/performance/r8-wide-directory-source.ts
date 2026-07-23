import { ed25519 } from '@noble/curves/ed25519.js'

import {
  V2CatalogClient,
  type V2CatalogOperationClient,
} from '../../src/catalog/v2-client'
import {
  type V2CatalogPageStore,
  type V2CachedDirectoryFailure,
  type V2CommittedDirectory,
} from '../../src/catalog/v2-page-store'
import {
  V2_CATALOG_DIRECTORY_ENTRIES,
  V2_CATALOG_PAGE_ENTRIES,
  V2_PATH_POLICY,
  type V2CatalogPage,
  type V2CatalogPageRequest,
  type V2ShareDescriptor,
} from '../../src/catalog/v2-records'
import { concatBytes, copyBytes, encodeBase64Url, equalBytes } from '../../src/crypto/bytes'
import { sha256 } from '../../src/crypto/digest'
import {
  createCatalogPageObjectBinding,
  SENDER_OBJECT_HEADER_BYTES,
  SENDER_OBJECT_NONCE_BYTES,
  SENDER_OBJECT_SIGNATURE_BYTES,
  SENDER_OBJECT_TAG_BYTES,
  SENDER_OBJECT_WIRE_VERSION,
  senderObjectAuthenticationData,
  senderObjectSignaturePreimage,
} from '../../src/crypto/sender-object'
import { deriveSuite02CatalogKey } from '../../src/crypto/suite02-key-derivation'
import { defaultCryptoRuntime } from '../../src/crypto/webcrypto'
import { encodeCanonicalCbor } from '../../src/protocol/cbor'

const CATALOG_SCHEMA = 1n
const CATALOG_ENTRY_FILE_KIND = 2n
const CATALOG_IDENTITY_BYTES = 16
const CATALOG_COMMITMENT_BYTES = 32
const CATALOG_READ_SECRET_BYTES = 16
const CATALOG_NONCE_PREFIX = 0x72
const CATALOG_NAME_WIDTH = 7
const R8_WIDE_PROGRESS_PAGE_INTERVAL = 256

export const R8_WIDE_PROGRESS_PREFIX = 'WINDSHARE_R8_WIDE_PROGRESS '

export interface R8WideDirectoryProgress {
  readonly pageIndex: number
  readonly stagedPages: number
  readonly stagedEntries: number
  readonly stagedPageOwnershipRecords: number
  readonly stagedNodeOwnershipKeys: number
  readonly stagedNameOwnershipKeys: number
}

export type R8WideDirectoryProgressObserver = (progress: R8WideDirectoryProgress) => void

export interface R8WideDirectoryProbeSnapshot {
  readonly generatedPages: number
  readonly generatedEntries: number
  readonly stagedPages: number
  readonly stagedEntries: number
  readonly stagedPageOwnershipRecords: number
  readonly stagedNodeOwnershipKeys: number
  readonly stagedNameOwnershipKeys: number
  readonly loadedPages: number
  readonly maximumGeneratedRows: number
  readonly maximumSourceSenderObjects: number
  readonly maximumStoreBoundaryPages: number
  readonly maximumStoreBoundaryRows: number
  readonly maximumLoadedPageRows: number
  readonly maximumControllerRows: number
  readonly maximumControllerEntryRecords: number
  readonly maximumControllerRootCandidates: number
  readonly maximumDomRows: number
  readonly maximumDomNodes: number
  readonly protocolFailures: number
}

/**
 * The probe counts ownership transitions instead of sampling the garbage
 * collector. That makes the bounded-memory claim deterministic: a source
 * object must be consumed by production decode/stage before another page can
 * be generated, and only one persisted page may cross back into the UI.
 */
export class R8WideDirectoryProbe {
  readonly #observeProgress: R8WideDirectoryProgressObserver | undefined
  #generatedPages = 0
  #generatedEntries = 0
  #stagedPages = 0
  #stagedEntries = 0
  #loadedPages = 0
  #generatedRows = 0
  #sourceSenderObjects = 0
  #storeBoundaryPages = 0
  #storeBoundaryRows = 0
  #maximumGeneratedRows = 0
  #maximumSourceSenderObjects = 0
  #maximumStoreBoundaryPages = 0
  #maximumStoreBoundaryRows = 0
  #maximumLoadedPageRows = 0
  #maximumControllerRows = 0
  #maximumControllerEntryRecords = 0
  #maximumControllerRootCandidates = 0
  #maximumDomRows = 0
  #maximumDomNodes = 0
  #pendingPageIndex: number | undefined
  #protocolFailures = 0

  constructor(observeProgress?: R8WideDirectoryProgressObserver) {
    this.#observeProgress = observeProgress
  }

  beginGeneration(pageIndex: number, entryCount: number): void {
    if (this.#pendingPageIndex !== undefined || this.#generatedRows !== 0) {
      throw new Error('Wide-directory source attempted to materialize more than one page')
    }
    this.#pendingPageIndex = pageIndex
    this.#generatedRows = entryCount
    this.#maximumGeneratedRows = Math.max(this.#maximumGeneratedRows, this.#generatedRows)
  }

  finishGeneration(entryCount: number): void {
    if (this.#pendingPageIndex === undefined || this.#generatedRows !== entryCount) {
      throw new Error('Wide-directory source lost page-generation ownership')
    }
    this.#generatedPages += 1
    this.#generatedEntries += entryCount
    this.#generatedRows = 0
    this.#sourceSenderObjects += 1
    this.#maximumSourceSenderObjects = Math.max(
      this.#maximumSourceSenderObjects,
      this.#sourceSenderObjects,
    )
  }

  beginStage(page: V2CatalogPage): void {
    if (this.#pendingPageIndex !== page.pageIndex || this.#sourceSenderObjects !== 1) {
      throw new Error('Production catalog decode did not preserve the lazy page sequence')
    }
    this.#storeBoundaryPages += 1
    this.#storeBoundaryRows += page.entries.length
    this.#maximumStoreBoundaryPages = Math.max(
      this.#maximumStoreBoundaryPages,
      this.#storeBoundaryPages,
    )
    this.#maximumStoreBoundaryRows = Math.max(
      this.#maximumStoreBoundaryRows,
      this.#storeBoundaryRows,
    )
  }

  finishStage(page: V2CatalogPage): void {
    this.#stagedPages += 1
    this.#stagedEntries += page.entries.length
    this.#storeBoundaryPages -= 1
    this.#storeBoundaryRows -= page.entries.length
    this.#sourceSenderObjects -= 1
    this.#pendingPageIndex = undefined
    if (page.terminal || this.#stagedPages % R8_WIDE_PROGRESS_PAGE_INTERVAL === 0) {
      this.#observeProgress?.(Object.freeze({
        pageIndex: page.pageIndex,
        stagedPages: this.#stagedPages,
        stagedEntries: this.#stagedEntries,
        stagedPageOwnershipRecords: this.#stagedPages,
        stagedNodeOwnershipKeys: this.#stagedEntries,
        stagedNameOwnershipKeys: this.#stagedEntries,
      }))
    }
  }

  observeLoadedPage(page: V2CatalogPage): void {
    this.#loadedPages += 1
    this.#maximumLoadedPageRows = Math.max(this.#maximumLoadedPageRows, page.entries.length)
  }

  observeControllerRows(
    rowCount: number,
    ownership: {
      readonly currentPageEntries: number
      readonly retainedRootCandidates: number
    },
  ): void {
    this.#maximumControllerRows = Math.max(this.#maximumControllerRows, rowCount)
    this.#maximumControllerEntryRecords = Math.max(
      this.#maximumControllerEntryRecords,
      ownership.currentPageEntries,
    )
    this.#maximumControllerRootCandidates = Math.max(
      this.#maximumControllerRootCandidates,
      ownership.retainedRootCandidates,
    )
  }

  observeDom(rowCount: number, nodeCount: number): void {
    this.#maximumDomRows = Math.max(this.#maximumDomRows, rowCount)
    this.#maximumDomNodes = Math.max(this.#maximumDomNodes, nodeCount)
  }

  observeProtocolFailure(): void {
    this.#protocolFailures += 1
  }

  snapshot(): R8WideDirectoryProbeSnapshot {
    return Object.freeze({
      generatedPages: this.#generatedPages,
      generatedEntries: this.#generatedEntries,
      stagedPages: this.#stagedPages,
      stagedEntries: this.#stagedEntries,
      stagedPageOwnershipRecords: this.#stagedPages,
      stagedNodeOwnershipKeys: this.#stagedEntries,
      stagedNameOwnershipKeys: this.#stagedEntries,
      loadedPages: this.#loadedPages,
      maximumGeneratedRows: this.#maximumGeneratedRows,
      maximumSourceSenderObjects: this.#maximumSourceSenderObjects,
      maximumStoreBoundaryPages: this.#maximumStoreBoundaryPages,
      maximumStoreBoundaryRows: this.#maximumStoreBoundaryRows,
      maximumLoadedPageRows: this.#maximumLoadedPageRows,
      maximumControllerRows: this.#maximumControllerRows,
      maximumControllerEntryRecords: this.#maximumControllerEntryRecords,
      maximumControllerRootCandidates: this.#maximumControllerRootCandidates,
      maximumDomRows: this.#maximumDomRows,
      maximumDomNodes: this.#maximumDomNodes,
      protocolFailures: this.#protocolFailures,
    })
  }
}

export class ObservedR8CatalogPageStore implements V2CatalogPageStore {
  readonly #inner: V2CatalogPageStore
  readonly #probe: R8WideDirectoryProbe

  constructor(inner: V2CatalogPageStore, probe: R8WideDirectoryProbe) {
    this.#inner = inner
    this.#probe = probe
  }

  loadDirectory(directoryIdText: string): Promise<V2CommittedDirectory | undefined> {
    return this.#inner.loadDirectory(directoryIdText)
  }

  loadFailure(directoryIdText: string): Promise<V2CachedDirectoryFailure | undefined> {
    return this.#inner.loadFailure(directoryIdText)
  }

  async loadPage(
    directory: V2CommittedDirectory,
    pageIndex: number,
  ): Promise<V2CatalogPage | undefined> {
    const page = await this.#inner.loadPage(directory, pageIndex)
    if (page !== undefined) this.#probe.observeLoadedPage(page)
    return page
  }

  begin(directoryIdText: string): Promise<void> {
    return this.#inner.begin(directoryIdText)
  }

  async stage(page: V2CatalogPage): Promise<void> {
    this.#probe.beginStage(page)
    await this.#inner.stage(page)
    this.#probe.finishStage(page)
  }

  commit(directory: V2CommittedDirectory): Promise<void> {
    return this.#inner.commit(directory)
  }

  storeFailure(cached: V2CachedDirectoryFailure): Promise<void> {
    return this.#inner.storeFailure(cached)
  }

  abort(directoryIdText: string): Promise<void> {
    return this.#inner.abort(directoryIdText)
  }

  close(): void {
    this.#inner.close()
  }
}

export interface R8WideDirectoryFixture {
  readonly descriptor: V2ShareDescriptor
  readonly readSecret: Uint8Array<ArrayBuffer>
  readonly operations: V2CatalogOperationClient
  readonly probe: R8WideDirectoryProbe
  readonly pageCount: number
  createClient(store: V2CatalogPageStore, storageIdentity: string): V2CatalogClient
  close(): void
}

export async function createR8WideDirectoryFixture(
  entryCount: number,
  observeProgress?: R8WideDirectoryProgressObserver,
): Promise<R8WideDirectoryFixture> {
  requireEntryCount(entryCount)
  const pageCount = Math.ceil(entryCount / V2_CATALOG_PAGE_ENTRIES)
  const shareInstance = fixedIdentity(0x11)
  const directoryId = fixedIdentity(0x22)
  const generation = fixedIdentity(0x33)
  const readSecret = new Uint8Array(CATALOG_READ_SECRET_BYTES).fill(0x44)
  const signingSeed = new Uint8Array(32).fill(0x55)
  const signing = ed25519.keygen(signingSeed)
  const signingSecret = signing.secretKey.slice()
  const senderPublicKey = signing.publicKey.slice()
  signing.secretKey.fill(0)
  signingSeed.fill(0)
  const catalogKeyBytes = await deriveSuite02CatalogKey(readSecret, shareInstance)
  const runtime = defaultCryptoRuntime()
  const catalogKey = await runtime.subtle.importKey(
    'raw',
    catalogKeyBytes,
    'AES-GCM',
    false,
    ['encrypt'],
  )
  const probe = new R8WideDirectoryProbe(observeProgress)
  const descriptor: V2ShareDescriptor = Object.freeze({
    wireVersion: 2,
    suite: 2,
    shareInstance,
    shareInstanceId: encodeBase64Url(shareInstance),
    syntheticRoot: directoryId,
    syntheticRootId: encodeBase64Url(directoryId),
    chunkSize: 1 << 20,
    capabilities: 0n,
    senderPublicKey,
    createdAtSeconds: 1n,
    pathPolicy: V2_PATH_POLICY,
  })
  let nextPageIndex = 0
  let previousCommitment = new Uint8Array(CATALOG_COMMITMENT_BYTES)
  let closed = false

  const operations: V2CatalogOperationClient = {
    fetchPage: async (request, signal) => {
      signal.throwIfAborted()
      if (closed) throw new Error('Wide-directory source is closed')
      requireSequentialRequest(request, nextPageIndex, directoryId, generation)
      const firstEntry = request.pageIndex * V2_CATALOG_PAGE_ENTRIES
      const entriesOnPage = Math.min(V2_CATALOG_PAGE_ENTRIES, entryCount - firstEntry)
      probe.beginGeneration(request.pageIndex, entriesOnPage)
      const entries = Array.from({ length: entriesOnPage }, (_unused, offset) => {
        const entryIndex = firstEntry + offset
        return Object.freeze([
          CATALOG_ENTRY_FILE_KIND,
          entryIdentity(entryIndex),
          catalogEntryName(entryIndex),
          BigInt(entryIndex + 1),
          null,
          0n,
          0n,
        ])
      })
      const terminal = request.pageIndex + 1 === pageCount
      const plaintext = encodeCanonicalCbor(new Map<number, unknown>([
        [0, CATALOG_SCHEMA],
        [1, shareInstance],
        [2, directoryId],
        [3, generation],
        [4, BigInt(request.pageIndex)],
        [5, terminal],
        [6, previousCommitment],
        [7, entries],
        [8, 0n],
      ]))
      const object = await sealCatalogPage(
        plaintext,
        descriptor,
        directoryId,
        request.pageIndex,
        catalogKey,
        signingSecret,
        runtime.subtle,
      )
      previousCommitment = await sha256(object)
      nextPageIndex += 1
      probe.finishGeneration(entriesOnPage)
      return object
    },
    failProtocol: async () => {
      probe.observeProtocolFailure()
    },
  }

  return Object.freeze({
    descriptor,
    readSecret,
    operations,
    probe,
    pageCount,
    createClient: (store: V2CatalogPageStore, storageIdentity: string) => new V2CatalogClient({
      descriptor,
      readSecret,
      operations,
      store,
      storageIdentity,
    }),
    close: () => {
      if (closed) return
      closed = true
      signingSecret.fill(0)
      catalogKeyBytes.fill(0)
      readSecret.fill(0)
    },
  })
}

export function catalogEntryName(entryIndex: number): string {
  return `entry-${entryIndex.toString().padStart(CATALOG_NAME_WIDTH, '0')}`
}

async function sealCatalogPage(
  plaintext: Uint8Array,
  descriptor: V2ShareDescriptor,
  directoryId: Uint8Array,
  pageIndex: number,
  catalogKey: CryptoKey,
  signingSecret: Uint8Array,
  subtle: SubtleCrypto,
): Promise<Uint8Array<ArrayBuffer>> {
  const binding = createCatalogPageObjectBinding(
    descriptor.shareInstance,
    directoryId,
    pageIndex,
  )
  const header = new Uint8Array(SENDER_OBJECT_HEADER_BYTES)
  header[0] = SENDER_OBJECT_WIRE_VERSION
  new DataView(header.buffer).setUint32(4, plaintext.byteLength + SENDER_OBJECT_TAG_BYTES, false)
  const nonce = new Uint8Array(SENDER_OBJECT_NONCE_BYTES)
  nonce[0] = CATALOG_NONCE_PREFIX
  new DataView(nonce.buffer).setUint32(SENDER_OBJECT_NONCE_BYTES - 4, pageIndex, false)
  const ciphertext = new Uint8Array(await subtle.encrypt({
    name: 'AES-GCM',
    iv: nonce,
    additionalData: await senderObjectAuthenticationData(binding, header),
    tagLength: SENDER_OBJECT_TAG_BYTES * 8,
  }, catalogKey, copyBytes(plaintext)))
  const prefix = concatBytes([header, nonce, ciphertext])
  const signature = ed25519.sign(
    await senderObjectSignaturePreimage(binding, prefix),
    signingSecret,
  )
  if (signature.byteLength !== SENDER_OBJECT_SIGNATURE_BYTES) {
    throw new Error('Wide-directory source produced an invalid Ed25519 signature width')
  }
  return concatBytes([prefix, signature])
}

function requireEntryCount(entryCount: number): void {
  if (
    !Number.isSafeInteger(entryCount) ||
    entryCount <= 0 ||
    entryCount > V2_CATALOG_DIRECTORY_ENTRIES
  ) {
    throw new RangeError('Wide-directory fixture entry count is outside the catalog contract')
  }
}

function requireSequentialRequest(
  request: V2CatalogPageRequest,
  nextPageIndex: number,
  directoryId: Uint8Array,
  generation: Uint8Array,
): void {
  if (!equalBytes(request.directoryId, directoryId) || request.pageIndex !== nextPageIndex) {
    throw new Error('Wide-directory source refuses out-of-order or cross-directory page requests')
  }
  if (
    (request.pageIndex === 0 && request.generation !== undefined) ||
    (request.pageIndex > 0 &&
      (request.generation === undefined || !equalBytes(request.generation, generation)))
  ) {
    throw new Error('Wide-directory source refuses a generation splice')
  }
}

function fixedIdentity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(CATALOG_IDENTITY_BYTES)
  value[0] = first
  return value
}

function entryIdentity(entryIndex: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(CATALOG_IDENTITY_BYTES)
  new DataView(value.buffer).setBigUint64(8, BigInt(entryIndex + 1), false)
  return value
}
