import { snapshotIdentity } from '../workspace/canonical'
import {
  receiveOperationHandleRecord,
} from '../workspace/records'
import type {
  CompatibleNameLedger,
  CompatibleNameMappingCommitInput,
  CompatibleNameMappingOwnershipInput,
  CompatibleNamePairOwnershipInput,
  CompatibleNamePendingTerminalInput,
  CompatibleNameTargetCreatedInput,
  CompatibleNameTerminalRepairInput,
} from '../file-system-access/compatible-name/ledger'
import {
  MAX_COMPATIBLE_NAME_COMMITTED_MAPPINGS,
  compatibleNameMappingId,
  compatibleNameMappingV1,
  compatibleNameOperationBootstrapV1,
  compatibleNameOperationHeaderV1,
  compatibleNamePendingTerminalOutcomeV1,
  compatibleNameRepairSummary,
  type CompatibleNameMappingSpec,
  type CompatibleNameMappingV1,
  type CompatibleNameOperationBootstrapV1,
  type CompatibleNameOperationHeaderV1,
  type CompatibleNameOperationSnapshotV1,
  type CompatibleNamePendingTerminalOutcomeV1,
  type CompatibleNameRepairSummary,
} from '../file-system-access/compatible-name/model'
import {
  FSA_OPERATION_HANDLE_COMPATIBLE_NAME_SCRIPT,
  FSA_OPERATION_HANDLE_COMPATIBLE_NAME_SIDECAR,
} from './indexeddb-root-binding'
import {
  DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME,
  INDEXEDDB_BY_OPERATION_COMMIT_ORDINAL_INDEX,
  INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE,
  INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE,
  INDEXEDDB_RECEIVE_HANDLE_STORE,
  openIndexedDbCheckpointDatabase,
  requestResult,
  transactionCompletion,
} from './indexeddb-database'
import {
  abortCapacity,
  abortIntegrity,
  abortQuietly,
  applyCompatibleNameBootstrapTransaction,
  assertCompatibleNameBootstrapTransaction,
  assertMappingCommitsOpen,
  assertPairOwned,
  assertPairReady,
  assertRepairSummary,
  boundedOrdinal,
  compatiblePairKind,
  headerAfterCommit,
  headerWithPairOwnership,
  headerWithoutPendingOutcome,
  isTerminalRepairSummary,
  mappingPhysicalClaimKey,
  operationMappingValues,
  operationRow,
  operationSnapshot,
  pairPhysicalClaimKeys,
  readMapping,
  readOperationRow,
  readPairHandle,
  replaceHeader,
  requireFileHandle,
  requireOperationRow,
  sameCleanupAuthority,
  sameMappingSelection,
  samePairHandleMetadata,
  sameValue,
  storedOperationRow,
} from './indexeddb-compatible-name-records'



export class IndexedDbCompatibleNameLedger implements CompatibleNameLedger {
  readonly #database: IDBDatabase
  #closed = false

  private constructor(database: IDBDatabase) {
    this.#database = database
    database.addEventListener('versionchange', () => this.close())
  }

  static async open(
    databaseName = DEFAULT_OUTPUT_CHECKPOINT_DATABASE_NAME,
  ): Promise<IndexedDbCompatibleNameLedger> {
    return new IndexedDbCompatibleNameLedger(
      await openIndexedDbCheckpointDatabase(databaseName),
    )
  }

  async readHeader(operationId: string): Promise<CompatibleNameOperationHeaderV1 | undefined> {
    const identity = snapshotIdentity(operationId, 16, 'operation ID')
    const transaction = this.#transaction(INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE, 'readonly')
    try {
      const value = await requestResult<unknown>(
        transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE).get(identity),
      )
      await transactionCompletion(transaction)
      return value === undefined ? undefined : readOperationRow(value).header
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  async loadOperation(operationId: string): Promise<CompatibleNameOperationSnapshotV1 | undefined> {
    const identity = snapshotIdentity(operationId, 16, 'operation ID')
    const transaction = this.#transaction([
      INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE,
      INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE,
    ], 'readonly')
    try {
      const headerValue = await requestResult<unknown>(
        transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE).get(identity),
      )
      // An absent header is the ordinary-operation fast path: no mapping cursor or scan is opened.
      if (headerValue === undefined) {
        await transactionCompletion(transaction)
        return undefined
      }
      const operation = readOperationRow(headerValue)
      const values = await operationMappingValues(transaction, identity)
      const snapshot = operationSnapshot(operation, values)
      await transactionCompletion(transaction)
      return snapshot
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  async bootstrapOperation(
    bootstrapInput: CompatibleNameOperationBootstrapV1,
  ): Promise<CompatibleNameOperationSnapshotV1> {
    const bootstrap = compatibleNameOperationBootstrapV1(bootstrapInput)
    const transaction = this.#transaction([
      INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE,
      INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE,
    ], 'readwrite')
    try {
      const insert = await assertCompatibleNameBootstrapTransaction(transaction, bootstrap)
      if (insert) applyCompatibleNameBootstrapTransaction(transaction, bootstrap)
      await transactionCompletion(transaction)
      return operationSnapshot(operationRow(bootstrap.header, 1), [bootstrap.initialMapping])
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  async claimMapping(selectionInput: CompatibleNameMappingSpec): Promise<CompatibleNameMappingV1> {
    const selection = compatibleNameMappingV1(selectionInput)
    if (selection.ownershipState !== 'selected' || selection.commitState !== 'uncommitted') {
      throw new TypeError('compatible-name claim must be an unowned, uncommitted selection')
    }
    const transaction = this.#transaction([
      INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE,
      INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE,
    ], 'readwrite')
    try {
      const operation = await requireOperationRow(transaction, selection.operationId)
      assertMappingCommitsOpen(transaction, operation.header)
      if (selection.attempt === 0 && selection.token !== operation.header.primaryToken) {
        abortIntegrity(transaction, 'primary mapping selection changed the operation token')
      }
      const mappings = transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE)
      const existingValue = await requestResult<unknown>(mappings.get(selection.id))
      if (existingValue !== undefined) {
        const existing = readMapping(existingValue)
        if (!sameMappingSelection(existing, selection)) {
          abortIntegrity(transaction, 'compatible-name mapping selection is immutable')
        }
        await transactionCompletion(transaction)
        return existing
      }
      const operationValues = await operationMappingValues(transaction, selection.operationId)
      if (operationValues.length >= MAX_COMPATIBLE_NAME_COMMITTED_MAPPINGS) {
        abortCapacity(transaction)
      }
      const operationMappings = operationValues.map(readMapping)
      const selectionClaim = mappingPhysicalClaimKey(operation.header, selection)
      if (operationMappings.some(mapping =>
        mappingPhysicalClaimKey(operation.header, mapping) === selectionClaim) ||
          pairPhysicalClaimKeys(operation.header).has(selectionClaim)) {
        abortIntegrity(transaction, 'compatible-name physical component is already claimed')
      }
      mappings.add(selection)
      await transactionCompletion(transaction)
      return selection
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  async recordPairOwnership(
    input: CompatibleNamePairOwnershipInput,
  ): Promise<CompatibleNameOperationHeaderV1> {
    const operationId = snapshotIdentity(input.operationId, 16, 'operation ID')
    const pairKind = compatiblePairKind(input.pairKind)
    const handle = requireFileHandle(input.handle)
    const transaction = this.#transaction([
      INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE,
      INDEXEDDB_RECEIVE_HANDLE_STORE,
    ], 'readwrite')
    try {
      const operation = await requireOperationRow(transaction, operationId)
      const identity = operation.header.pair[pairKind]
      const handleKind = pairKind === 'script'
        ? FSA_OPERATION_HANDLE_COMPATIBLE_NAME_SCRIPT
        : FSA_OPERATION_HANDLE_COMPATIBLE_NAME_SIDECAR
      const expected = receiveOperationHandleRecord({
        id: identity.handleId,
        operationId,
        kind: handleKind,
        authorityRef: operation.header.authorityRef,
        ownedObjectId: identity.ownedObjectId,
        handle,
      })
      const handles = transaction.objectStore(INDEXEDDB_RECEIVE_HANDLE_STORE)
      const existingValue = await requestResult<unknown>(handles.get(identity.handleId))
      if (identity.ownershipState === 'owned') {
        if (existingValue === undefined || !samePairHandleMetadata(
          readPairHandle(existingValue),
          expected,
        )) {
          abortIntegrity(transaction, 'owned restoration pair handle is missing or changed')
        }
        await transactionCompletion(transaction)
        return operation.header
      }
      if (existingValue !== undefined) {
        abortIntegrity(transaction, 'restoration pair handle ID is already occupied')
      }
      handles.add(expected)
      const header = headerWithPairOwnership(operation.header, pairKind)
      transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE)
        .put(storedOperationRow(header, operation.nextCommitOrdinal))
      await transactionCompletion(transaction)
      return header
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  async recordCompatibleTargetCreated(
    input: CompatibleNameTargetCreatedInput,
  ): Promise<CompatibleNameOperationHeaderV1> {
    const operationId = snapshotIdentity(input.operationId, 16, 'operation ID')
    const mappingId = compatibleNameMappingId(operationId, input.logicalPath, input.entryKind)
    const summary = compatibleNameRepairSummary(input.repairSummary)
    const transaction = this.#transaction([
      INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE,
      INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE,
    ], 'readwrite')
    try {
      const operation = await requireOperationRow(transaction, operationId)
      const mappingValue = await requestResult<unknown>(
        transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE).get(mappingId),
      )
      if (mappingValue === undefined) {
        abortIntegrity(transaction, 'created compatible target lacks its durable mapping claim')
      }
      readMapping(mappingValue)
      if (operation.header.activationState === 'active') {
        if (operation.header.repairSummary === undefined) {
          abortIntegrity(transaction, 'active compatible-name operation lacks its repair summary')
        }
        await transactionCompletion(transaction)
        return operation.header
      }
      assertPairOwned(transaction, operation.header)
      await assertRepairSummary(transaction, operation, summary)
      if (operation.nextCommitOrdinal !== 1 || summary.committedCount !== 0 ||
          operation.header.repairSummary !== undefined) {
        abortIntegrity(transaction, 'first compatible target crossed an existing repair lifecycle')
      }
      const header = replaceHeader(operation.header, {
        activationState: 'active',
        repairSummary: summary,
      })
      transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE)
        .put(storedOperationRow(header, operation.nextCommitOrdinal))
      await transactionCompletion(transaction)
      return header
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  async recordVerifiedDirectoryOwnership(
    input: CompatibleNameMappingOwnershipInput,
  ): Promise<CompatibleNameMappingV1> {
    const operationId = snapshotIdentity(input.operationId, 16, 'operation ID')
    const ownedObjectId = snapshotIdentity(input.ownedObjectId, 32, 'owned object ID')
    const mappingId = compatibleNameMappingId(operationId, input.logicalPath, input.entryKind)
    if (input.entryKind !== 'directory') {
      throw new TypeError('only a directory can record ownership before its final mapping commit')
    }
    const transaction = this.#transaction([
      INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE,
      INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE,
    ], 'readwrite')
    try {
      const operation = await requireOperationRow(transaction, operationId)
      assertPairReady(transaction, operation.header)
      assertMappingCommitsOpen(transaction, operation.header)
      const mappings = transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE)
      const value = await requestResult<unknown>(mappings.get(mappingId))
      if (value === undefined) abortIntegrity(transaction, 'compatible-name mapping claim is missing')
      const mapping = readMapping(value)
      if (mapping.ownershipState === 'owned') {
        if (mapping.ownedObjectId !== ownedObjectId) {
          abortIntegrity(transaction, 'compatible-name owned-object correlation changed')
        }
        await transactionCompletion(transaction)
        return mapping
      }
      const owned = compatibleNameMappingV1({
        ...mapping,
        ownershipState: 'owned',
        ownedObjectId,
      })
      mappings.put(owned)
      await transactionCompletion(transaction)
      return owned
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  async commitMapping(input: CompatibleNameMappingCommitInput): Promise<CompatibleNameMappingV1> {
    const operationId = snapshotIdentity(input.operationId, 16, 'operation ID')
    const ownedObjectId = snapshotIdentity(input.ownedObjectId, 32, 'owned object ID')
    const mappingId = compatibleNameMappingId(operationId, input.logicalPath, input.entryKind)
    const transaction = this.#transaction([
      INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE,
      INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE,
    ], 'readwrite')
    try {
      const operation = await requireOperationRow(transaction, operationId)
      assertPairReady(transaction, operation.header)
      assertMappingCommitsOpen(transaction, operation.header)
      const mappings = transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE)
      const value = await requestResult<unknown>(mappings.get(mappingId))
      if (value === undefined) abortIntegrity(transaction, 'compatible-name mapping claim is missing')
      const mapping = readMapping(value)
      if (mapping.commitState === 'committed') {
        if (mapping.ownedObjectId !== ownedObjectId) {
          abortIntegrity(transaction, 'committed compatible-name ownership is immutable')
        }
        await transactionCompletion(transaction)
        return mapping
      }
      if (mapping.entryKind === 'directory' &&
          (mapping.ownershipState !== 'owned' || mapping.ownedObjectId !== ownedObjectId)) {
        abortIntegrity(transaction, 'directory mapping commit lacks verified ownership')
      }
      if (operation.nextCommitOrdinal > MAX_COMPATIBLE_NAME_COMMITTED_MAPPINGS) {
        abortCapacity(transaction)
      }
      const committed = compatibleNameMappingV1({
        ...mapping,
        ownershipState: 'owned',
        ownedObjectId,
        commitState: 'committed',
        commitOrdinal: operation.nextCommitOrdinal,
      })
      const header = headerAfterCommit(operation.header, committed)
      mappings.put(committed)
      transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE).put(storedOperationRow(
        header,
        operation.nextCommitOrdinal + 1,
      ))
      await transactionCompletion(transaction)
      return committed
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  async scanCommittedMappings(
    operationIdInput: string,
    afterOrdinal = 0,
  ): Promise<readonly CompatibleNameMappingV1[]> {
    const operationId = snapshotIdentity(operationIdInput, 16, 'operation ID')
    const after = boundedOrdinal(afterOrdinal, true, 'compatible-name publication cursor')
    const transaction = this.#transaction([
      INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE,
      INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE,
    ], 'readonly')
    try {
      const operation = await requireOperationRow(transaction, operationId)
      const committedCount = operation.nextCommitOrdinal - 1
      if (after > committedCount) {
        abortIntegrity(transaction, 'compatible-name publication cursor exceeds the ledger')
      }
      if (after === committedCount) {
        await transactionCompletion(transaction)
        return Object.freeze([])
      }
      const values = await requestResult<unknown[]>(
        transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE)
          .index(INDEXEDDB_BY_OPERATION_COMMIT_ORDINAL_INDEX)
          .getAll(IDBKeyRange.bound(
            [operationId, after + 1],
            [operationId, MAX_COMPATIBLE_NAME_COMMITTED_MAPPINGS],
          )),
      )
      const mappings = values.map(readMapping)
      let expected = after + 1
      for (const mapping of mappings) {
        if (mapping.operationId !== operationId || mapping.commitState !== 'committed' ||
            mapping.commitOrdinal !== expected) {
          abortIntegrity(transaction, 'compatible-name committed ordinals are not contiguous')
        }
        expected += 1
      }
      if (expected !== operation.nextCommitOrdinal) {
        abortIntegrity(transaction, 'compatible-name committed ordinal prefix is incomplete')
      }
      await transactionCompletion(transaction)
      return Object.freeze(mappings)
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  persistRepairSummary(
    operationId: string,
    repairSummary: CompatibleNameRepairSummary,
  ): Promise<CompatibleNameOperationHeaderV1> {
    return this.#persistSummary(operationId, repairSummary)
  }

  async persistPendingTerminalOutcome(
    input: CompatibleNamePendingTerminalInput,
  ): Promise<CompatibleNameOperationHeaderV1> {
    const operationId = snapshotIdentity(input.operationId, 16, 'operation ID')
    const outcome = compatibleNamePendingTerminalOutcomeV1(input.outcome)
    const summary = compatibleNameRepairSummary(input.repairSummary)
    if (outcome.ordinaryLifecycle.operationId !== operationId) {
      throw new TypeError('pending terminal outcome escaped its compatible-name operation')
    }
    const transaction = this.#transaction([
      INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE,
      INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE,
    ], 'readwrite')
    try {
      const operation = await requireOperationRow(transaction, operationId)
      await assertRepairSummary(transaction, operation, summary)
      if (operation.header.pendingTerminalOutcome === undefined &&
          isTerminalRepairSummary(operation.header.repairSummary)) {
        abortIntegrity(transaction, 'final compatible-name repair summary is immutable')
      }
      if (operation.header.pendingTerminalOutcome !== undefined &&
          !sameValue(operation.header.pendingTerminalOutcome, outcome)) {
        abortIntegrity(transaction, 'pending compatible-name terminal outcome is immutable')
      }
      const header = replaceHeader(operation.header, {
        pendingTerminalOutcome: outcome,
        repairSummary: summary,
      })
      transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE)
        .put(storedOperationRow(header, operation.nextCommitOrdinal))
      await transactionCompletion(transaction)
      return header
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  async readPendingTerminalOutcome(
    operationId: string,
  ): Promise<CompatibleNamePendingTerminalOutcomeV1 | undefined> {
    return (await this.readHeader(operationId))?.pendingTerminalOutcome
  }

  async clearPendingTerminalOutcome(
    input: CompatibleNameTerminalRepairInput,
  ): Promise<CompatibleNameOperationHeaderV1> {
    const operationId = snapshotIdentity(input.operationId, 16, 'operation ID')
    const summary = compatibleNameRepairSummary(input.repairSummary)
    const footer = summary.latestObservedFooter
    if (summary.pendingCatchUp || footer === undefined || footer.state === 'active' ||
        footer.committedCount !== summary.committedCount) {
      throw new TypeError('pending terminal outcome requires a complete terminal footer')
    }
    const transaction = this.#transaction([
      INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE,
      INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE,
    ], 'readwrite')
    try {
      const operation = await requireOperationRow(transaction, operationId)
      await assertRepairSummary(transaction, operation, summary)
      if (operation.header.pendingTerminalOutcome === undefined &&
          !sameValue(operation.header.repairSummary, summary)) {
        abortIntegrity(transaction, 'cleared terminal repair summary is immutable')
      }
      const header = headerWithoutPendingOutcome(operation.header, summary)
      transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE)
        .put(storedOperationRow(header, operation.nextCommitOrdinal))
      await transactionCompletion(transaction)
      return header
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  async readRepairSummary(operationId: string): Promise<CompatibleNameRepairSummary | undefined> {
    const header = await this.readHeader(operationId)
    const summary = header?.repairSummary
    if (header === undefined || summary === undefined || header.pendingTerminalOutcome === undefined ||
        summary.pendingCatchUp) return summary
    // A crash after lifecycle publication but before clearing the pending row must
    // remain retryable in retained presentation even if the terminal footer is valid.
    return compatibleNameRepairSummary({ ...summary, pendingCatchUp: true })
  }

  async removeVerifiedEmptyOperation(
    expectedHeaderInput: CompatibleNameOperationHeaderV1,
  ): Promise<void> {
    const expectedHeader = compatibleNameOperationHeaderV1(expectedHeaderInput)
    const transaction = this.#transaction([
      INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE,
      INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE,
      INDEXEDDB_RECEIVE_HANDLE_STORE,
    ], 'readwrite')
    try {
      const operations = transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE)
      const operationValue = await requestResult<unknown>(operations.get(expectedHeader.operationId))
      const mappingValues = await operationMappingValues(transaction, expectedHeader.operationId)
      const handles = transaction.objectStore(INDEXEDDB_RECEIVE_HANDLE_STORE)
      const pairHandleValues = await Promise.all([
        requestResult<unknown>(handles.get(expectedHeader.pair.script.handleId)),
        requestResult<unknown>(handles.get(expectedHeader.pair.sidecar.handleId)),
      ])
      if (operationValue === undefined) {
        if (mappingValues.length !== 0 || pairHandleValues.some(value => value !== undefined)) {
          abortIntegrity(transaction, 'removed compatible-name operation retained durable authority')
        }
        await transactionCompletion(transaction)
        return
      }
      const operation = readOperationRow(operationValue)
      if (!sameCleanupAuthority(operation.header, expectedHeader) ||
          operation.nextCommitOrdinal !== 1 || operation.header.pendingTerminalOutcome !== undefined ||
          operation.header.activationState === 'prepared') {
        abortIntegrity(transaction, 'compatible-name operation is not a removable empty repair')
      }
      for (const value of mappingValues) {
        const mapping = readMapping(value)
        if (mapping.commitState !== 'uncommitted' || mapping.commitOrdinal !== undefined) {
          abortIntegrity(transaction, 'committed compatible-name repair cannot be removed as empty')
        }
      }
      const pairRecords = pairHandleValues.map(value =>
        value === undefined ? undefined : readPairHandle(value))
      const expectedKinds = [
        FSA_OPERATION_HANDLE_COMPATIBLE_NAME_SCRIPT,
        FSA_OPERATION_HANDLE_COMPATIBLE_NAME_SIDECAR,
      ] as const
      const expectedIdentities = [operation.header.pair.script, operation.header.pair.sidecar] as const
      pairRecords.forEach((record, index) => {
        const identity = expectedIdentities[index]!
        if (record === undefined || record.id !== identity.handleId ||
            record.operationId !== operation.header.operationId ||
            record.kind !== expectedKinds[index] ||
            record.authorityRef !== operation.header.authorityRef ||
            record.ownedObjectId !== identity.ownedObjectId) {
          abortIntegrity(transaction, 'empty repair pair ownership is missing or changed')
        }
      })
      const mappings = transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE)
      for (const value of mappingValues) mappings.delete(readMapping(value).id)
      handles.delete(operation.header.pair.script.handleId)
      handles.delete(operation.header.pair.sidecar.handleId)
      operations.delete(operation.header.operationId)
      await transactionCompletion(transaction)
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.#database.close()
  }

  async #persistSummary(
    operationIdInput: string,
    summaryInput: CompatibleNameRepairSummary,
  ): Promise<CompatibleNameOperationHeaderV1> {
    const operationId = snapshotIdentity(operationIdInput, 16, 'operation ID')
    const summary = compatibleNameRepairSummary(summaryInput)
    const transaction = this.#transaction([
      INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE,
      INDEXEDDB_COMPATIBLE_NAME_MAPPING_STORE,
    ], 'readwrite')
    try {
      const operation = await requireOperationRow(transaction, operationId)
      await assertRepairSummary(transaction, operation, summary)
      if (operation.header.pendingTerminalOutcome === undefined &&
          isTerminalRepairSummary(operation.header.repairSummary) &&
          !sameValue(operation.header.repairSummary, summary)) {
        abortIntegrity(transaction, 'final compatible-name repair summary is immutable')
      }
      const header = replaceHeader(operation.header, { repairSummary: summary })
      transaction.objectStore(INDEXEDDB_COMPATIBLE_NAME_OPERATION_STORE)
        .put(storedOperationRow(header, operation.nextCommitOrdinal))
      await transactionCompletion(transaction)
      return header
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  #transaction(
    storeNames: string | readonly string[],
    mode: IDBTransactionMode,
  ): IDBTransaction {
    this.#assertOpen()
    return this.#database.transaction(storeNames, mode)
  }

  #assertOpen(): void {
    if (this.#closed) {
      throw new DOMException('Compatible-name ledger is closed', 'InvalidStateError')
    }
  }
}

export {
  applyCompatibleNameBootstrapTransaction,
  assertCompatibleNameBootstrapTransaction,
  assertCompatibleNameBootstrapTransition,
} from './indexeddb-compatible-name-records'
