import {
  INDEXEDDB_BY_KIND_CANDIDATE_INDEX,
  INDEXEDDB_BY_OPERATION_INDEX,
  INDEXEDDB_BY_OPERATION_CHAIN_PAGE_INDEX,
  INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE,
  INDEXEDDB_DIRECT_ZIP_STATE_STORE,
  INDEXEDDB_RECEIVE_LEASE_STORE,
  requestResult,
  transactionCompletion,
} from '../../../browser/indexeddb-database'
import { snapshotIdentity } from '../../../workspace/canonical'
import { receiveOperationLeaseId, validateReceiveOperationLeaseRecord, type ReceiveOperationLeaseRecord } from '../../../workspace/records'
import {
  DIRECT_ZIP_CANDIDATE_BOOTSTRAP,
  type DirectZipBootstrapCandidateBatchV1,
  type DirectZipBootstrapCandidateScanV1,
  type DirectZipBootstrapCandidateV1,
  type DirectZipCandidateV1,
  type DirectZipCheckpointProposalV1,
  type DirectZipImmutablePageV1,
  type DirectZipJournalFenceV1,
  type DirectZipJournalTrace,
  type DirectZipPageBatchV1,
  type DirectZipPageKind,
  type DirectZipPageScanV1,
  type DirectZipStateRowV1,
} from '../model'
import {
  directZipCandidateId,
  directZipPageId,
  directZipStateId,
  validateDirectZipBootstrapCandidateV1,
  validateDirectZipImmutablePageV1,
  validateDirectZipStateRowV1,
} from '../records'
import type { DirectZipOrphanCollectionV1 } from '../repository'
import {
  DirectZipJournalConcurrencyError,
  MAXIMUM_CURSOR_BATCH,
  PAGE_ID_SUFFIX,
  abortQuietly,
  addCheckpointReachability,
  collectCursorRows,
  collectCursorValues,
  cursorLimit,
  directZipPageStore,
  pageKindFromId,
  pageReachabilityKey,
  previousPageBudgetUsage,
  requireBootstrapCandidateCursor,
  requirePageKind,
  sameCandidateRow,
  samePageRow,
  sameStateRow,
  snapshotFence,
  validateCandidate,
} from './authority'

export class IndexedDbDirectZipJournalStorage {
  readonly database: IDBDatabase
  readonly #trace: DirectZipJournalTrace | undefined
  #closed = false

  constructor(database: IDBDatabase, trace?: DirectZipJournalTrace) {
    this.database = database
    this.#trace = trace
    database.addEventListener('versionchange', () => this.close())
  }

  async readState(operationIdInput: string): Promise<DirectZipStateRowV1 | undefined> {
    this.assertOpen()
    const operationId = snapshotIdentity(operationIdInput, 16, 'operation ID')
    const value = await this.read<unknown>(
      INDEXEDDB_DIRECT_ZIP_STATE_STORE,
      directZipStateId(operationId),
    )
    return value === undefined ? undefined : validateDirectZipStateRowV1(value as DirectZipStateRowV1)
  }

  async readCandidate(
    operationIdInput: string,
    candidateIdInput: string,
  ): Promise<DirectZipCandidateV1 | undefined> {
    this.assertOpen()
    const operationId = snapshotIdentity(operationIdInput, 16, 'operation ID')
    const candidateId = snapshotIdentity(candidateIdInput, 16, 'candidate ID')
    const value = await this.read<unknown>(
      INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE,
      directZipCandidateId(operationId, candidateId),
    )
    return value === undefined ? undefined : validateCandidate(value)
  }

  async readOperationCandidate(operationIdInput: string): Promise<DirectZipCandidateV1 | undefined> {
    this.assertOpen()
    const operationId = snapshotIdentity(operationIdInput, 16, 'operation ID')
    const transaction = this.database.transaction(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE, 'readonly')
    const request = transaction.objectStore(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE)
      .index(INDEXEDDB_BY_OPERATION_INDEX).openCursor(IDBKeyRange.only(operationId), 'next')
    const values = await collectCursorValues(request, 2)
    await transactionCompletion(transaction)
    if (values.length > 1) {
      throw new TypeError('Direct ZIP operation has ambiguous live candidates')
    }
    return values.length === 0 ? undefined : validateCandidate(values[0])
  }

  async readPageBatch(scanInput: DirectZipPageScanV1): Promise<DirectZipPageBatchV1> {
    this.assertOpen()
    const operationId = snapshotIdentity(scanInput.operationId, 16, 'operation ID')
    const chainId = snapshotIdentity(scanInput.chainId, 16, 'page chain ID')
    const pageKind = requirePageKind(scanInput.pageKind)
    const afterPageOrdinal = scanInput.afterPageOrdinal
    if (afterPageOrdinal !== undefined &&
        (!Number.isSafeInteger(afterPageOrdinal) || afterPageOrdinal < 0)) {
      throw new TypeError('Direct ZIP page cursor is invalid')
    }
    const limit = cursorLimit(scanInput.limit)
    const transaction = this.database.transaction(directZipPageStore(pageKind), 'readonly')
    const index = transaction.objectStore(directZipPageStore(pageKind))
      .index(INDEXEDDB_BY_OPERATION_CHAIN_PAGE_INDEX)
    const lowerOrdinal = afterPageOrdinal === undefined ? 0 : afterPageOrdinal + 1
    const range = IDBKeyRange.bound(
      [operationId, chainId, lowerOrdinal],
      [operationId, chainId, Number.MAX_SAFE_INTEGER],
    )
    const values = await collectCursorValues(index.openCursor(range, 'next'), limit)
    await transactionCompletion(transaction)
    const pages = Object.freeze(await Promise.all(values.map(async value => {
      const page = await validateDirectZipImmutablePageV1(value as DirectZipImmutablePageV1)
      if (page.operationId !== operationId || page.chainId !== chainId || page.pageKind !== pageKind) {
        throw new TypeError('Direct ZIP page cursor escaped its indexed chain')
      }
      return page
    })))
    const nextPageOrdinal = pages.length === limit ? pages.at(-1)?.pageOrdinal : undefined
    return Object.freeze({
      pages,
      ...(nextPageOrdinal === undefined ? {} : { nextPageOrdinal }),
    })
  }

  async *streamPages(
    scan: Omit<DirectZipPageScanV1, 'afterPageOrdinal'>,
  ): AsyncIterable<DirectZipImmutablePageV1> {
    let afterPageOrdinal: number | undefined
    do {
      const batch = await this.readPageBatch({
        ...scan,
        ...(afterPageOrdinal === undefined ? {} : { afterPageOrdinal }),
      })
      for (const page of batch.pages) yield page
      if (batch.pages.length === 0) return
      afterPageOrdinal = batch.nextPageOrdinal
    } while (afterPageOrdinal !== undefined)
  }

  async readBootstrapCandidateBatch(
    scan: DirectZipBootstrapCandidateScanV1 = {},
  ): Promise<DirectZipBootstrapCandidateBatchV1> {
    this.assertOpen()
    const limit = cursorLimit(scan.limit)
    const afterCandidateKey = scan.afterCandidateKey === undefined
      ? ''
      : requireBootstrapCandidateCursor(scan.afterCandidateKey)
    const transaction = this.database.transaction(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE, 'readonly')
    const index = transaction.objectStore(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE)
      .index(INDEXEDDB_BY_KIND_CANDIDATE_INDEX)
    const range = IDBKeyRange.bound(
      [DIRECT_ZIP_CANDIDATE_BOOTSTRAP, afterCandidateKey],
      [DIRECT_ZIP_CANDIDATE_BOOTSTRAP, PAGE_ID_SUFFIX],
      scan.afterCandidateKey !== undefined,
      false,
    )
    const values = await collectCursorValues(index.openCursor(range, 'next'), limit)
    await transactionCompletion(transaction)
    const candidates = Object.freeze(await Promise.all(values.map(value =>
      validateDirectZipBootstrapCandidateV1(value as DirectZipBootstrapCandidateV1))))
    const nextCandidateKey = candidates.length === limit ? candidates.at(-1)?.id : undefined
    return Object.freeze({
      candidates,
      ...(nextCandidateKey === undefined ? {} : { nextCandidateKey }),
    })
  }

  async *streamBootstrapCandidates(): AsyncIterable<DirectZipBootstrapCandidateV1> {
    let afterCandidateKey: string | undefined
    do {
      const batch = await this.readBootstrapCandidateBatch(
        afterCandidateKey === undefined ? {} : { afterCandidateKey },
      )
      for (const candidate of batch.candidates) yield candidate
      if (batch.candidates.length === 0) return
      afterCandidateKey = batch.nextCandidateKey
    } while (afterCandidateKey !== undefined)
  }

  async collectOrphanPages(
    fenceInput: DirectZipJournalFenceV1,
  ): Promise<DirectZipOrphanCollectionV1> {
    this.assertOpen()
    const fence = snapshotFence(fenceInput)
    const state = await this.readState(fence.operationId)
    if (state === undefined || state.leaseId !== fence.leaseId ||
        state.checkpoint.generation !== fence.checkpointGeneration) {
      throw new DirectZipJournalConcurrencyError()
    }
    const reachablePages = new Map<string, bigint>()
    addCheckpointReachability(reachablePages, state.checkpoint)
    const candidate = await this.readOperationCandidate(fence.operationId)
    if (candidate !== undefined && candidate.kind !== 'bootstrap') {
      addCheckpointReachability(reachablePages, candidate.proposedCheckpoint)
    }
    let scannedPageCount = 0n
    let deletedPageCount = 0n
    for (const pageKind of ['layout', 'central', 'epoch'] as const) {
      let afterId: string | undefined
      do {
        const result = await this.collectOrphanPageBatch(
          fence,
          pageKind,
          reachablePages,
          state,
          candidate,
          afterId,
        )
        scannedPageCount += BigInt(result.scanned)
        deletedPageCount += BigInt(result.deleted)
        afterId = result.nextId
      } while (afterId !== undefined)
    }
    this.emit({
      name: 'direct_zip.journal.orphans_collected',
      operation_id: fence.operationId,
      lease_id: fence.leaseId,
      checkpoint_generation: fence.checkpointGeneration,
      decision: `deleted:${deletedPageCount.toString(10)}`,
    })
    return Object.freeze({ scannedPageCount, deletedPageCount })
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.database.close()
  }

  async assertPagePredecessors(
    page: DirectZipImmutablePageV1,
    fence: DirectZipJournalFenceV1,
  ): Promise<readonly DirectZipImmutablePageV1[]> {
    const predecessors: DirectZipImmutablePageV1[] = []
    const previousUsage = previousPageBudgetUsage(page)
    if (page.accountingPredecessor.kind === 'checkpoint') {
      if (page.accountingPredecessor.checkpointGeneration !== fence.checkpointGeneration) {
        throw new DirectZipJournalConcurrencyError('Direct ZIP page accounting generation changed')
      }
    } else {
      const predecessor = await this.readPage(
        page.accountingPredecessor.pageKind,
        page.accountingPredecessor.pageId,
      )
      if (predecessor === undefined || predecessor.operationId !== fence.operationId ||
          predecessor.digest !== page.accountingPredecessor.pageDigest ||
          predecessor.budgetUsage.memberCount !== previousUsage.memberCount ||
          predecessor.budgetUsage.canonicalMetadataBytes !== previousUsage.canonicalMetadataBytes) {
        throw new DirectZipJournalConcurrencyError('Direct ZIP page accounting lineage is unavailable')
      }
      predecessors.push(predecessor)
    }
    if (page.pageOrdinal === 0) return Object.freeze(predecessors)
    const chainPredecessor = await this.readPage(
      page.pageKind,
      directZipPageId(page.operationId, page.pageKind, page.chainId, page.pageOrdinal - 1),
    )
    if (chainPredecessor === undefined ||
        chainPredecessor.chainRootDigest !== page.predecessorRootDigest ||
        chainPredecessor.chainRecordCount + BigInt(page.entryCount) !== page.chainRecordCount ||
        chainPredecessor.chainCanonicalMetadataBytes + BigInt(page.canonicalBytes.byteLength) !==
          page.chainCanonicalMetadataBytes) {
      throw new DirectZipJournalConcurrencyError('Direct ZIP immutable page chain predecessor changed')
    }
    if (!predecessors.some(predecessor => predecessor.id === chainPredecessor.id)) {
      predecessors.push(chainPredecessor)
    }
    return Object.freeze(predecessors)
  }

  async checkpointPageTails(
    checkpoint: DirectZipStateRowV1['checkpoint'] | DirectZipCheckpointProposalV1,
  ): Promise<readonly DirectZipImmutablePageV1[]> {
    const tails = new Map<string, DirectZipImmutablePageV1>()
    for (const [pageKind, chain] of [
      ['layout', checkpoint.layoutPages],
      ['central', checkpoint.centralPages],
      ['epoch', checkpoint.epochPages],
    ] as const) {
      if (chain.pageCount === 0n) continue
      if (chain.pageCount > BigInt(Number.MAX_SAFE_INTEGER)) {
        throw new TypeError('Direct ZIP page count exceeds the cursor bound')
      }
      const tail = await this.readPage(pageKind, directZipPageId(
        checkpoint.operationId,
        pageKind,
        chain.chainId,
        Number(chain.pageCount - 1n),
      ))
      if (tail === undefined || tail.chainRootDigest !== chain.rootDigest ||
          tail.chainRecordCount !== chain.recordCount ||
          tail.chainCanonicalMetadataBytes !== chain.canonicalMetadataBytes) {
        throw new DirectZipJournalConcurrencyError(`Direct ZIP ${pageKind} page root is unavailable`)
      }
      tails.set(tail.id, tail)
    }
    const tailId = checkpoint.accountingTailPageId
    if (tailId !== undefined) {
      const tail = await this.readPageByCanonicalId(tailId)
      if (tail === undefined || tail.budgetUsage.memberCount !== checkpoint.journalUsage.memberCount ||
          tail.budgetUsage.canonicalMetadataBytes !== checkpoint.journalUsage.canonicalMetadataBytes) {
        throw new DirectZipJournalConcurrencyError('Direct ZIP journal accounting tail is unavailable')
      }
      tails.set(tail.id, tail)
    }
    return Object.freeze([...tails.values()])
  }

  async assertFence(
    transaction: IDBTransaction,
    fence: DirectZipJournalFenceV1,
    expectedState: DirectZipStateRowV1,
  ): Promise<DirectZipStateRowV1> {
    const [stateValue, lease] = await Promise.all([
      requestResult<unknown>(transaction.objectStore(INDEXEDDB_DIRECT_ZIP_STATE_STORE)
        .get(directZipStateId(fence.operationId))),
      this.requireLease(transaction, fence.operationId),
    ])
    if (!sameStateRow(stateValue, expectedState) ||
        expectedState.leaseId !== fence.leaseId || lease.leaseId !== fence.leaseId ||
        expectedState.checkpoint.generation !== fence.checkpointGeneration) {
      throw new DirectZipJournalConcurrencyError()
    }
    return expectedState
  }

  async readFenceState(fence: DirectZipJournalFenceV1): Promise<DirectZipStateRowV1> {
    const state = await this.readState(fence.operationId)
    if (state === undefined || state.leaseId !== fence.leaseId ||
        state.checkpoint.generation !== fence.checkpointGeneration) {
      throw new DirectZipJournalConcurrencyError()
    }
    return state
  }

  async requireLease(
    transaction: IDBTransaction,
    operationId: string,
  ): Promise<ReceiveOperationLeaseRecord> {
    const value = await requestResult<unknown>(transaction.objectStore(INDEXEDDB_RECEIVE_LEASE_STORE)
      .get(receiveOperationLeaseId(operationId)))
    if (value === undefined) throw new DirectZipJournalConcurrencyError()
    return validateReceiveOperationLeaseRecord(value as ReceiveOperationLeaseRecord)
  }

  async readPage(
    pageKind: DirectZipPageKind,
    id: string,
  ): Promise<DirectZipImmutablePageV1 | undefined> {
    const value = await this.read<unknown>(directZipPageStore(pageKind), id)
    return value === undefined
      ? undefined
      : validateDirectZipImmutablePageV1(value as DirectZipImmutablePageV1)
  }

  readPageByCanonicalId(id: string): Promise<DirectZipImmutablePageV1 | undefined> {
    const pageKind = pageKindFromId(id)
    return this.readPage(pageKind, id)
  }

  async read<T>(storeName: string, id: string): Promise<T | undefined> {
    const transaction = this.database.transaction(storeName, 'readonly')
    const value = await requestResult<T | undefined>(transaction.objectStore(storeName).get(id))
    await transactionCompletion(transaction)
    return value
  }

  async collectOrphanPageBatch(
    fence: DirectZipJournalFenceV1,
    pageKind: DirectZipPageKind,
    reachablePages: ReadonlyMap<string, bigint>,
    expectedState: DirectZipStateRowV1,
    expectedCandidate: DirectZipCandidateV1 | undefined,
    afterId?: string,
  ): Promise<Readonly<{ scanned: number; deleted: number; nextId?: string }>> {
    const storeName = directZipPageStore(pageKind)
    const prefix = `windshare/direct-zip-page/v1/${fence.operationId}/`
    const range = IDBKeyRange.bound(
      afterId ?? prefix,
      `${prefix}${PAGE_ID_SUFFIX}`,
      afterId !== undefined,
      false,
    )
    const scanTransaction = this.database.transaction(storeName, 'readonly')
    const rows = await collectCursorRows(
      scanTransaction.objectStore(storeName).openCursor(range, 'next'),
      MAXIMUM_CURSOR_BATCH,
    )
    await transactionCompletion(scanTransaction)
    const pages = await Promise.all(rows.map(async row => {
      const page = await validateDirectZipImmutablePageV1(row.value as DirectZipImmutablePageV1)
      if (page.operationId !== fence.operationId || page.pageKind !== pageKind ||
          row.primaryKey !== page.id) {
        throw new TypeError('Direct ZIP orphan cursor escaped its operation')
      }
      return page
    }))

    const transaction = this.database.transaction([
      storeName,
      INDEXEDDB_DIRECT_ZIP_STATE_STORE,
      INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE,
      INDEXEDDB_RECEIVE_LEASE_STORE,
    ], 'readwrite')
    try {
      await this.assertFence(transaction, fence, expectedState)
      const candidateStore = transaction.objectStore(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE)
      const candidateCount = await requestResult(candidateStore.index(INDEXEDDB_BY_OPERATION_INDEX)
        .count(IDBKeyRange.only(fence.operationId)))
      const candidateValue = expectedCandidate === undefined
        ? undefined
        : await requestResult<unknown>(candidateStore.get(expectedCandidate.id))
      if ((expectedCandidate === undefined && candidateCount !== 0) ||
          (expectedCandidate !== undefined &&
           (candidateCount !== 1 || !sameCandidateRow(candidateValue, expectedCandidate)))) {
        throw new DirectZipJournalConcurrencyError('Direct ZIP candidate reachability changed')
      }
      const store = transaction.objectStore(storeName)
      let deleted = 0
      for (const page of pages) {
        const current = await requestResult<unknown>(store.get(page.id))
        if (!samePageRow(current, page)) {
          throw new DirectZipJournalConcurrencyError('Direct ZIP orphan page changed during collection')
        }
        const reachablePageCount = reachablePages.get(pageReachabilityKey(page.pageKind, page.chainId))
        if (reachablePageCount === undefined || BigInt(page.pageOrdinal) >= reachablePageCount) {
          store.delete(page.id)
          deleted += 1
        }
      }
      await transactionCompletion(transaction)
      const nextId = rows.length === MAXIMUM_CURSOR_BATCH
        ? String(rows.at(-1)?.primaryKey)
        : undefined
      return Object.freeze({
        scanned: rows.length,
        deleted,
        ...(nextId === undefined ? {} : { nextId }),
      })
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  assertOpen(): void {
    if (this.#closed) {
      throw new DOMException('Direct ZIP journal repository is closed', 'InvalidStateError')
    }
  }

  emit(event: Parameters<DirectZipJournalTrace>[0]): void {
    try {
      this.#trace?.(Object.freeze(event))
    } catch {
      // Diagnostics are passive: losing a trace must never change durable authority.
    }
  }

  failed(
    operationId: string,
    leaseId: string | undefined,
    candidateId: string | undefined,
    error: unknown,
  ): void {
    this.emit({
      name: 'direct_zip.journal.transaction_failed',
      operation_id: operationId,
      ...(leaseId === undefined ? {} : { lease_id: leaseId }),
      ...(candidateId === undefined ? {} : { candidate_id: candidateId }),
      decision: error instanceof DOMException ? error.name : 'Error',
    })
  }
}
